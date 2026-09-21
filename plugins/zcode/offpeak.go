// offpeak.go implements the off-peak (闲时任务) ticketing channel — the
// official low-priority discount lane of the coding plan (M3).
//
// Protocol source: the official open-source client zai-org/ZCode (Apache-2.0)
// — packages/services/src/session/offPeakServerClient.ts (the four JSON
// ticket endpoints + wire shapes), offPeakRuntimeModel.ts (dual-credential
// headers + bigmodel-team identity), packages/shared/src/off-peak-types.ts
// (server admission states), and
// apps/zcode-cli/packages/adapters/src/model/offpeak-retry.ts (the queue /
// ticket-expired failure semantics). The account-level wire contract carries
// no client fingerprint (see the open-source adoption policy: protocol
// shapes only; the behavior baseline stays the closed-source client).
//
// Wire contract (all paths under {offPeakBase}/api/v1/off-peak):
//
//	GET   /ticket/availability            → {can_take_number, next_take_at?}
//	POST  /ticket            {task_id}    → {ticket_id, state, position?, next_poll_after?}
//	POST  /ticket/status     {ticket_ids} → {next_poll_after?, tickets:[{ticket_id,state,position?,active_deadline?}]}
//	POST  /ticket/{id}/settle             → idempotent 2xx (unknown ticket also acked)
//	POST  /anthropic/v1/messages          → the LLM call, needs X-Off-Peak-Ticket-ID
//
// Server admission states: queued → ready (5min TTL) → active (3h cap) →
// expired/settled. Polling cadence is server-driven via next_poll_after in
// SECONDS (Retry-After convention). Business codes: 3101 not eligible,
// 3102 ticket expired (retake with the SAME task_id = resume semantics),
// 3103 take-number quota exhausted, 3105 queued (429 + Retry-After).
//
// Auth: `Authorization: Bearer {plan JWT}` + `x-coding-plan-api-key: {plan
// key}` on every ticket call; the messages call additionally carries
// `X-Off-Peak-Ticket-ID`. start-plan credentials are NOT supported by the
// server (the official client throws start_plan_not_supported) — the plugin
// only routes coding-plan accounts through this lane.
//
// Plugin adaptation: the official client runs long-lived background task
// queues; the CPA host expects synchronous execute/execute_stream calls.
// So the plugin does "ticket-then-send": acquire a ready ticket inside the
// wait budget (offpeak_max_wait, default 0 = only an immediately-ready
// ticket passes), post the translated anthropic body, then settle. The
// 429/3105 queue-wait and 3102 retake loops ride the non-stream path only —
// once a stream has begun emitting chunks a retry can no longer be transparent.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// offPeakBase and the messages endpoint are vars (not consts) so tests can
// point them at an httptest server. The HTTP exit mirrors auth_keyres.go.
var (
	offPeakBase = "https://zcode.z.ai"
	// offPeakMessagesEndpoint is the ticketed anthropic messages gateway —
	// an unsigned chat path (signing.go isUnsignedChatPath lists it).
	offPeakMessagesEndpoint = offPeakBase + "/api/v1/off-peak/anthropic/v1/messages"
	offPeakHTTPDo           = func(req *http.Request) (*http.Response, error) {
		return sharedHTTPClient().Do(req)
	}
)

const (
	offPeakAvailabilityPath = "/api/v1/off-peak/ticket/availability"
	offPeakTakePath         = "/api/v1/off-peak/ticket"
	offPeakStatusPath       = "/api/v1/off-peak/ticket/status"
	// settlePath builds "/api/v1/off-peak/ticket/{id}/settle" per ticket.
	offPeakSettlePathPrefix = "/api/v1/off-peak/ticket/"

	// offPeakRequestTimeout matches the official client's REQUEST_TIMEOUT_MS.
	offPeakRequestTimeout = 10 * time.Second
	// offPeakBatchLimit is the wire contract's batch-status cap; the official
	// client truncates rather than splitting (the take-number quota blocks a
	// single user long before 100 non-terminal tickets exist).
	offPeakBatchLimit = 100

	// Business codes (off-peak lane only — the official adapter refuses to
	// put these in the global provider business-code map because 3102/3105
	// mean different things on other bigmodel APIs).
	offPeakBizNotEligible      = 3101 // no off-peak eligibility
	offPeakBizTicketExpired    = 3102 // ticket unusable (ready 5min TTL / active 3h cap / settled)
	offPeakBizTakeQuota        = 3103 // take-number quota exhausted
	offPeakBizQueued           = 3105 // queue acknowledgment (rides HTTP 429 + Retry-After)
	offPeakBizTicketExpiredOld = 3001 // pre-3102 gateway shape (rolling-release compat)

	// Queue-wait clamps (offpeak-retry.ts): a queue ack means "wait and
	// probe again" — cap a single wait at 5min, default 60s when the
	// Retry-After header is missing.
	offPeakQueueWaitCapMs     = 5 * 60 * 1000
	offPeakQueueWaitDefaultMs = 60 * 1000
)

// Poll-interval clamps for the acquire loop (offPeakTaskService's
// OFF_PEAK_SYNC_MIN/MAX_INTERVAL_MS, tightened for the synchronous host's
// wait budget). Vars so tests can shrink the cadence.
var (
	offPeakPollMinInterval = 5 * time.Second
	offPeakPollMaxInterval = 60 * time.Second
)

// offPeakTicketExpiredMessage mirrors the stable error marker the official
// stack uses to classify a run as "retake the ticket and resume" instead of
// "fail" (OFF_PEAK_TICKET_EXPIRED_MARKER). Our classification is in-process
// so the marker only needs to surface in user-facing error text.
const offPeakTicketExpiredMarker = "off-peak-ticket-expired"

// -----------------------------------------------------------------------------
// Configuration (config.yaml → offpeak / offpeak_max_wait)
// -----------------------------------------------------------------------------

var (
	offPeakEnabledMu  sync.RWMutex
	offPeakEnabled    = false
	offPeakMaxWaitMu  sync.RWMutex
	offPeakMaxWait    time.Duration // 0 = only an immediately-ready ticket passes
	offPeakHTTPTimeMu sync.RWMutex
	offPeakHTTPTime   = 30 * time.Second // overall wait budget slack for tests
)

func configureOffPeak(enabled bool, maxWait time.Duration) {
	offPeakEnabledMu.Lock()
	offPeakEnabled = enabled
	offPeakEnabledMu.Unlock()
	offPeakMaxWaitMu.Lock()
	offPeakMaxWait = maxWait
	offPeakMaxWaitMu.Unlock()
}

// offPeakActive reports whether the ticketing lane is enabled. start-plan
// accounts are structurally excluded by the server (start_plan_not_supported).
func offPeakActive() bool {
	offPeakEnabledMu.RLock()
	defer offPeakEnabledMu.RUnlock()
	return offPeakEnabled
}

func offPeakWaitBudget() time.Duration {
	offPeakMaxWaitMu.RLock()
	defer offPeakMaxWaitMu.RUnlock()
	return offPeakMaxWait
}

// -----------------------------------------------------------------------------
// Wire types (loose parsing, snake_case per server v2; unknown fields ignored)
// -----------------------------------------------------------------------------

// offPeakTicketState is the server admission state axis (the official client
// task status queued/paused/running/... is a different axis the plugin does
// not model — a CPA request is one synchronous round).
type offPeakTicketState string

const (
	offPeakStateQueued   offPeakTicketState = "queued"
	offPeakStateReady    offPeakTicketState = "ready"
	offPeakStateActive   offPeakTicketState = "active"
	offPeakStateExpired  offPeakTicketState = "expired"
	offPeakStateSettled  offPeakTicketState = "settled"
	offPeakStateNotFound offPeakTicketState = "not_found"
)

func (s offPeakTicketState) valid() bool {
	switch s {
	case offPeakStateQueued, offPeakStateReady, offPeakStateActive,
		offPeakStateExpired, offPeakStateSettled, offPeakStateNotFound:
		return true
	}
	return false
}

// offPeakTicket is the take-number response. next_poll_after is in SECONDS
// (the server follows the Retry-After convention); position is only present
// while queued (null once ready/active).
type offPeakTicket struct {
	TicketID      string
	TaskID        string
	State         offPeakTicketState
	Position      *int
	NextPollAfter *float64 // seconds
}

type offPeakStatusEntry struct {
	TicketID       string
	State          offPeakTicketState
	Position       *int
	ActiveDeadline *float64 // unix seconds when present
}

type offPeakAvailability struct {
	CanTakeNumber bool
	// NextTakeAt is unix MILLISECONDS per the wire contract.
	NextTakeAt int64
}

// offPeakServerError is a typed non-2xx ticket/messages failure. The caller
// classifies by BizCode (3101/3103 fatal on take; 3102/3001 retake; 3105 or
// bare 429 queue-wait).
type offPeakServerError struct {
	Path       string
	HTTPStatus int
	BizCode    int
	Msg        string
	NextTakeAt int64 // unix ms, when the server quotes one
}

func (e *offPeakServerError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "off-peak %s failed: HTTP %d", e.Path, e.HTTPStatus)
	if e.BizCode != 0 {
		fmt.Fprintf(&b, " code=%d", e.BizCode)
	}
	if e.Msg != "" {
		b.WriteString(" " + e.Msg)
	}
	return b.String()
}

// offPeakBizHint renders the lane-specific business-code meaning the stock
// translator cannot know (these codes are only meaningful on this lane).
func offPeakBizHint(bizCode int) string {
	switch bizCode {
	case offPeakBizNotEligible:
		return "off-peak is not enabled for this account (code 3101)"
	case offPeakBizTakeQuota:
		return "off-peak take-number quota exhausted for now (code 3103)"
	case offPeakBizTicketExpired, offPeakBizTicketExpiredOld:
		return offPeakTicketExpiredMarker + " (the ticket expired; a retry will re-take one)"
	case offPeakBizQueued:
		return "off-peak queue is full — retry after the quoted delay (code 3105)"
	}
	return ""
}

// -----------------------------------------------------------------------------
// Ticket client (four JSON endpoints; loose envelope handling)
// -----------------------------------------------------------------------------

// offPeakRequest performs one ticket-plane call. Headers follow the official
// client: source-identity set (TV shape — no X-ZCode-Agent), the plan JWT as
// Bearer, and the coding-plan key as x-coding-plan-api-key. Both the bare
// body and the {code:0,data} envelope are accepted (server v2 ships bare;
// the zai gateway sometimes wraps).
func offPeakRequest(method, path string, sa *storedAuth, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	ctx, cancel := context.WithTimeout(context.Background(), offPeakRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, offPeakBase+path, reader)
	if err != nil {
		return nil, err
	}
	applyOffPeakTicketHeaders(req, sa)
	resp, err := offPeakHTTPDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &offPeakServerError{Path: path, HTTPStatus: resp.StatusCode}
		var probe struct {
			Code    int             `json:"code"`
			Msg     string          `json:"msg"`
			Message string          `json:"message"`
			NextAt  int64           `json:"next_take_at"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(payload, &probe) == nil {
			se.BizCode = probe.Code
			se.Msg = firstNonEmpty(probe.Msg, probe.Message)
			se.NextTakeAt = probe.NextAt
			if se.NextTakeAt == 0 && len(probe.Data) > 0 {
				var d struct {
					NextTakeAt int64 `json:"next_take_at"`
				}
				if json.Unmarshal(probe.Data, &d) == nil {
					se.NextTakeAt = d.NextTakeAt
				}
			}
		}
		return nil, se
	}
	var probe struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &probe) == nil && probe.Data != nil && probe.Code == 0 {
		return probe.Data, nil
	}
	return json.RawMessage(payload), nil
}

// applyOffPeakTicketHeaders mirrors buildOffPeakRequestAuth + the source
// header set: TV identity (control-plane shape), Bearer plan JWT,
// x-coding-plan-api-key. The bigmodel-team organization/project headers are
// only sent when both halves exist (the official client refuses half an
// identity; the plugin currently stores neither, so personal plans send none).
func applyOffPeakTicketHeaders(req *http.Request, sa *storedAuth) {
	req.Header.Set("Content-Type", "application/json")
	for k, v := range buildContextIdentityHeaders(zcodeIdentity(), sa.Auth.DeviceMid) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.JWT)
	if key := strings.TrimSpace(sa.Auth.AccessToken); key != "" {
		req.Header.Set("x-coding-plan-api-key", key)
	}
}

// offPeakQueryAvailability GETs the take-number snapshot. Contract: an
// unavailable answer without next_take_at is a dirty response and surfaces
// as an error (the official client does the same so the UI can schedule a
// re-probe instead of graying out forever).
func offPeakQueryAvailability(sa *storedAuth) (*offPeakAvailability, error) {
	raw, err := offPeakRequest(http.MethodGet, offPeakAvailabilityPath, sa, nil)
	if err != nil {
		return nil, err
	}
	var p struct {
		CanTakeNumber *bool  `json:"can_take_number"`
		NextTakeAt    int64  `json:"next_take_at"`
		NextAtStr     string `json:"-"` // placeholder to keep parse loose
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("off-peak availability parse: %w", err)
	}
	if p.CanTakeNumber == nil {
		return nil, fmt.Errorf("off-peak availability missing can_take_number")
	}
	if !*p.CanTakeNumber && p.NextTakeAt == 0 {
		return nil, fmt.Errorf("off-peak availability missing next_take_at while unavailable")
	}
	return &offPeakAvailability{CanTakeNumber: *p.CanTakeNumber, NextTakeAt: p.NextTakeAt}, nil
}

// offPeakTakeTicket POSTs the take-number request. The task_id is the
// client's stable queue identity — retaking with the SAME task_id is the
// documented resume semantics.
func offPeakTakeTicket(sa *storedAuth, taskID string) (*offPeakTicket, error) {
	body, err := json.Marshal(map[string]string{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	raw, err := offPeakRequest(http.MethodPost, offPeakTakePath, sa, body)
	if err != nil {
		return nil, err
	}
	return parseOffPeakTicket(raw)
}

func parseOffPeakTicket(raw json.RawMessage) (*offPeakTicket, error) {
	var p struct {
		TicketID      string   `json:"ticket_id"`
		TaskID        string   `json:"task_id"`
		State         string   `json:"state"`
		Position      *int     `json:"position"`
		NextPollAfter *float64 `json:"next_poll_after"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("off-peak ticket parse: %w", err)
	}
	if strings.TrimSpace(p.TicketID) == "" {
		return nil, fmt.Errorf("off-peak ticket missing ticket_id")
	}
	st := offPeakTicketState(strings.TrimSpace(p.State))
	if !st.valid() {
		return nil, fmt.Errorf("off-peak ticket unknown state %q", p.State)
	}
	return &offPeakTicket{
		TicketID:      p.TicketID,
		TaskID:        p.TaskID,
		State:         st,
		Position:      p.Position,
		NextPollAfter: p.NextPollAfter,
	}, nil
}

// offPeakBatchStatus POSTs the batch status probe (≤100 ids, truncated like
// the official client). Entries must be matched by ticket id — the response
// order/content is not guaranteed to mirror the request.
func offPeakBatchStatus(sa *storedAuth, ticketIDs []string) ([]offPeakStatusEntry, *float64, error) {
	if len(ticketIDs) == 0 {
		return nil, nil, nil
	}
	if len(ticketIDs) > offPeakBatchLimit {
		ticketIDs = ticketIDs[:offPeakBatchLimit]
	}
	body, err := json.Marshal(map[string]any{"ticket_ids": ticketIDs})
	if err != nil {
		return nil, nil, err
	}
	raw, err := offPeakRequest(http.MethodPost, offPeakStatusPath, sa, body)
	if err != nil {
		return nil, nil, err
	}
	var p struct {
		NextPollAfter *float64 `json:"next_poll_after"`
		Tickets       []struct {
			TicketID       string   `json:"ticket_id"`
			State          string   `json:"state"`
			Position       *int     `json:"position"`
			ActiveDeadline *float64 `json:"active_deadline"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("off-peak status parse: %w", err)
	}
	entries := make([]offPeakStatusEntry, 0, len(p.Tickets))
	for _, t := range p.Tickets {
		st := offPeakTicketState(strings.TrimSpace(t.State))
		if strings.TrimSpace(t.TicketID) == "" || !st.valid() {
			continue // skip malformed entries instead of failing the whole batch
		}
		entries = append(entries, offPeakStatusEntry{
			TicketID:       t.TicketID,
			State:          st,
			Position:       t.Position,
			ActiveDeadline: t.ActiveDeadline,
		})
	}
	return entries, p.NextPollAfter, nil
}

// offPeakSettle POSTs the idempotent settle. Unknown tickets / repeated
// settles are 2xx-acked by the server; a 4xx therefore also counts as an
// ack (nothing left to release). Only network errors / 5xx propagate.
func offPeakSettle(sa *storedAuth, ticketID string) error {
	path := offPeakSettlePathPrefix + url.PathEscape(ticketID) + "/settle"
	_, err := offPeakRequest(http.MethodPost, path, sa, nil)
	if err == nil {
		return nil
	}
	if se, ok := err.(*offPeakServerError); ok && se.HTTPStatus < 500 {
		return nil // 4xx = already gone; idempotent ack
	}
	return err
}

// offPeakSettleBestEffort swallows every settle failure — the server's
// timeout reaper is the documented fallback for a lost settle.
func offPeakSettleBestEffort(sa *storedAuth, ticketID string) {
	if ticketID == "" {
		return
	}
	_ = offPeakSettle(sa, ticketID)
}

// -----------------------------------------------------------------------------
// Failure semantics (offpeak-retry.ts, lane-local on purpose)
// -----------------------------------------------------------------------------

type offPeakFailureKind int

const (
	offPeakFailureNone offPeakFailureKind = iota
	offPeakFailureQueued
	offPeakFailureTicketExpired
)

// offPeakFailureDecision classifies one failed messages attempt:
//   - 3102/3001 → ticketExpired (retake with the same task_id and continue)
//   - 3105 or a bare 429 → queued (wait min(Retry-After, 5min), default 60s)
//
// Everything else is a plain failure (kind none). retryAfterMs ≤ 0 means the
// header was absent.
func offPeakFailureDecision(statusCode, bizCode int, retryAfterMs int64) (offPeakFailureKind, int64) {
	if statusCode < 400 {
		// Success is never a lane failure, whatever body rode along — the
		// caller only consults this after a failed send, but stay defensive.
		return offPeakFailureNone, 0
	}
	if bizCode == offPeakBizTicketExpired || bizCode == offPeakBizTicketExpiredOld {
		return offPeakFailureTicketExpired, 0
	}
	if bizCode == offPeakBizQueued || statusCode == http.StatusTooManyRequests {
		wait := offPeakQueueWaitDefaultMs
		if retryAfterMs > 0 {
			wait = int(retryAfterMs)
		}
		if wait > offPeakQueueWaitCapMs {
			wait = offPeakQueueWaitCapMs
		}
		return offPeakFailureQueued, int64(wait)
	}
	return offPeakFailureNone, 0
}

// retryAfterMillis parses the Retry-After header (seconds per HTTP spec).
func retryAfterMillis(h http.Header) int64 {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > 0 {
		return secs * 1000
	}
	return 0
}

// extractOffPeakBizCode pulls the business code out of a failed response
// body: bare {"code":3102,...}, the {code:0,data:{code:...}} envelope, or an
// anthropic-style {"error":{"code":...}} wrapper.
func extractOffPeakBizCode(body []byte) int {
	var probe struct {
		Code  int `json:"code"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Data struct {
			Code int `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}
	for _, c := range []int{probe.Code, probe.Error.Code, probe.Data.Code} {
		if c != 0 {
			return c
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// Ticket lifecycle (acquire → ready, then the caller sends + settles)
// -----------------------------------------------------------------------------

// newOffPeakTaskID mints one request-scoped queue identity. Retakes inside
// one execute round reuse it (the official resume semantics); the next host
// request gets a fresh one.
func newOffPeakTaskID() string {
	return "offpeak-" + uuid.NewString()
}

// offPeakAcquireTicket drives take → (poll while queued) until a ready
// ticket or the wait budget runs out. expired entries re-take with the same
// task_id (resume). budget ≤ 0 means only an immediately-ready ticket
// passes — the honest default for a synchronous host.
func offPeakAcquireTicket(sa *storedAuth, taskID string, budget time.Duration) (*offPeakTicket, error) {
	deadline := time.Time{}
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}
	ticket, err := offPeakTakeTicket(sa, taskID)
	if err != nil {
		// Take failures surface their lane meaning (3101/3103 are final for
		// this round — the official create flow reports them to the UI).
		if se, ok := err.(*offPeakServerError); ok {
			if hint := offPeakBizHint(se.BizCode); hint != "" {
				return nil, fmt.Errorf("%s: %w", hint, err)
			}
		}
		return nil, err
	}
	for {
		switch ticket.State {
		case offPeakStateReady, offPeakStateActive:
			// active: another dispatch on this task already advanced the
			// ticket — the messages call is still valid with this ticket id.
			return ticket, nil
		case offPeakStateQueued:
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				return nil, offPeakQueueBudgetError(ticket)
			}
			wait := offPeakPollMaxInterval
			if ticket.NextPollAfter != nil && *ticket.NextPollAfter > 0 {
				wait = time.Duration(*ticket.NextPollAfter * float64(time.Second))
			}
			if wait < offPeakPollMinInterval {
				wait = offPeakPollMinInterval
			}
			if wait > offPeakPollMaxInterval {
				wait = offPeakPollMaxInterval
			}
			if !deadline.IsZero() {
				if left := time.Until(deadline); left < wait {
					wait = left
				}
				if wait <= 0 {
					return nil, offPeakQueueBudgetError(ticket)
				}
			}
			time.Sleep(wait)
			entries, _, err := offPeakBatchStatus(sa, []string{ticket.TicketID})
			if err != nil {
				// A failed status probe does not kill the ticket: loop back
				// with the same cadence (the server advances states on poll).
				if !deadline.IsZero() && !time.Now().Before(deadline) {
					return nil, offPeakQueueBudgetError(ticket)
				}
				continue
			}
			matched := (*offPeakStatusEntry)(nil)
			for i := range entries {
				if entries[i].TicketID == ticket.TicketID {
					matched = &entries[i]
					break
				}
			}
			if matched == nil {
				continue
			}
			switch matched.State {
			case offPeakStateReady, offPeakStateActive:
				ticket.State = matched.State
				ticket.Position = matched.Position
			case offPeakStateExpired, offPeakStateNotFound, offPeakStateSettled:
				// Ready-TTL blown while queued: re-take with the same
				// task_id (resume-to-queue-tail, per the official sync loop).
				next, terr := offPeakTakeTicket(sa, taskID)
				if terr != nil {
					return nil, terr
				}
				ticket = next
			case offPeakStateQueued:
				ticket.Position = matched.Position
			}
		default:
			// expired/settled/not_found straight out of take: the round is
			// over — surface the lane hint rather than spin.
			return nil, fmt.Errorf("%s: ticket %s is %s", offPeakTicketExpiredMarker, ticket.TicketID, ticket.State)
		}
	}
}

func offPeakQueueBudgetError(ticket *offPeakTicket) error {
	pos := ""
	if ticket.Position != nil && *ticket.Position > 0 {
		pos = fmt.Sprintf(", position #%d", *ticket.Position)
	}
	return fmt.Errorf("off-peak queue wait budget exhausted before the ticket turned ready (state %s%s) — raise offpeak_max_wait or retry during the off-peak window", ticket.State, pos)
}

// -----------------------------------------------------------------------------
// Chat route integration
// -----------------------------------------------------------------------------

// applyOffPeakChatHeaders = the start-plan header set + the ticket plane's
// dual credentials + the ticket id. Trace uses the three-header subset (the
// messages call has no query/session context, same as start-plan).
func applyOffPeakChatHeaders(req *http.Request, sa *storedAuth, ticketID string) {
	applyStartPlanChatHeaders(req, sa, "")
	req.Header.Set("X-Coding-Plan-Api-Key", strings.TrimSpace(sa.Auth.AccessToken))
	if ticketID != "" {
		req.Header.Set("X-Off-Peak-Ticket-ID", ticketID)
	}
}

// offPeakEligible reports whether this credential may ride the lane: the
// plugin config enabled it AND the account is a coding-plan account (the
// server rejects start-plan tickets with start_plan_not_supported).
func offPeakEligible(sa *storedAuth) bool {
	if sa == nil || !offPeakActive() {
		return false
	}
	return sa.Auth.Plan != planStart
}

// -----------------------------------------------------------------------------
// Model catalog
// -----------------------------------------------------------------------------

// offPeakModelIDs is the model set the official off-peak lane serves
// (open-source builtin catalog: off-peak = GLM-5.3 / GLM-5.3-Flash).
var offPeakModelIDs = map[string]struct{}{
	"glm-5.3":       {},
	"glm-5.3-flash": {},
}

// filterOffPeakModels narrows the coding-plan catalog to the off-peak set
// while the lane is enabled (mirrors the official idle-plan model picker).
func filterOffPeakModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, ok := offPeakModelIDs[strings.ToLower(m.ID)]; ok {
			out = append(out, m)
		}
	}
	return out
}
