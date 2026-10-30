// queue_retry.go — the 10605 RESOURCE_LIMIT queue envelope (v0.8.64).
//
// Observed 2026-10-20 in a Docker-deployed CPA (qfmodel, p3 queue): the
// Qoder gateway no longer only *holds* a request on the connection until the
// queue discharges (the historical behaviour chat_live_test.go documents); it
// can also reject immediately with a 403 whose body is a queue ticket asking
// the client to come back later:
//
//	HTTP 200 SSE frame, statusCodeValue=403
//	L1: {"code":403,"message":"<L2 json string>"}
//	L2: {"code":"10605","message":"<L3 json string>","subCode":"RESOURCE_LIMIT"}
//	L3: {"isQueued":true,"modelKey":"qfmodel","queueType":"p3",
//	     "retryAfterSeconds":30,"serviceAvailable":false,
//	     "subCode":"RESOURCE_LIMIT","waitTime":30}
//
// Before this file the plugin treated that as an ordinary upstream failure:
// recordUpstreamFailure + HTTP 500 with the raw JSON soup rendered by
// describeQoderEnvelopeRejection — the client saw "qoder upstream error:
// status=403 code=403 type=: {...}" and the credential was marked failed for
// what is really a transient capacity ticket.
//
// All three chat paths (async pump, synchronous collector, non-stream
// executor) now recognise the ticket and retry the request themselves after
// retryAfterSeconds, bounded by the constants below. The wait respects the
// request context, so a disconnected client cancels an in-flight queue wait.
//
// v0.8.71: the ticket's interval is only honoured when the ticket also reports
// a real queue depth. A ticket that says `queueCount: 0` — nobody ahead of us —
// while still asking for 30 seconds is not worth three 30-second sleeps
// (~90s): measured 2026-10-29, the call after such a ticket is usually admitted
// outright (a third consecutive ticket from one credential answered 200 and
// streamed), so those tickets retry on a 700ms ladder instead. A per-model
// window keeps one failing host request from paying that ladder once per
// credential it rotates through (measured: 21-31s per client request before the
// window, 4.9s after).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	queueWaitBudget = queueTotalBudget
	queueBudgetMu   sync.RWMutex
)

const (
	// queueCode is the gateway's queue/RESOURCE_LIMIT business code. It has
	// been observed as a JSON string ("10605"); older gateways emit plain
	// numbers elsewhere in the family, so the matcher accepts both shapes.
	queueCode = "10605"
	// queueSubCode is the resource-limit sub-code both the L2 envelope and
	// the L3 ticket carry.
	queueSubCode = "RESOURCE_LIMIT"
	// queueMarker is the greppable prefix queueLimitLine stamps into failure
	// text; the cooldown classifier keys on it (isQueueLimitFailure).
	queueMarker = "queue_limit"
	// queueHeadMarker is stamped beside queueMarker when the ticket itself
	// reported an empty queue (queueCount == 0). isHeadOfQueueFailure keys on
	// it to bench the pair for one ladder window instead of the one-ticket
	// cadence: the fast ladder already spent the ticket's interval, so holding
	// the credential for a full 60s would drop a serviceable credential out of
	// the scheduler for the rest of a conversation.
	queueHeadMarker = "queue_head"

	// queueMaxAttempts bounds one chat call: the original request plus at
	// most three queue retries (30s each with the observed ticket = ~90s of
	// waiting, inside the 120s shared-client window of a single attempt).
	queueMaxAttempts = 4
	// queueFastAttempts bounds the ladder used for an empty-queue ticket. The
	// ticket's own estimate is not worth sleeping when the gateway admits the
	// very next call, so these retries are spaced queueFastRetryBackoff apart.
	queueFastAttempts = 4
	// queueFastRetryBackoff is the spacing of the empty-queue ladder.
	queueFastRetryBackoff = 700 * time.Millisecond
	// queueFastLadderWindow bounds how often one model may run the ladder
	// inside one host request. The host rotates credentials when an execution
	// fails, and each rotation would otherwise pay for its own ladder.
	queueFastLadderWindow = 12 * time.Second
	// queueWaitDefault is used when the ticket carries no usable interval.
	queueWaitDefault = 30 * time.Second
	// queueWaitCap clamps a hostile/large retryAfterSeconds per round.
	queueWaitCap = 45 * time.Second
	// queueTotalBudget caps the cumulative queue wait across all rounds of
	// one chat call, so a 60s ticket cannot pin a request for minutes.
	queueTotalBudget = 150 * time.Second
)

// queueSignal is the parsed queue ticket. detected=false means the body was
// not a 10605/RESOURCE_LIMIT envelope.
type queueSignal struct {
	detected   bool
	subCode    string
	retryAfter time.Duration
	queueType  string
	modelKey   string
	// queueCount is the depth the ticket reported ahead of this request.
	// depthSeen distinguishes "the ticket said 0" from "the ticket did not
	// carry a depth at all" — only the former is a head-of-queue ticket.
	queueCount int
	depthSeen  bool
	// emptyQueue marks a ticket that reported queueCount == 0: the plugin
	// retries those on the fast ladder instead of the ticket's own estimate.
	emptyQueue bool
	// status is the status the rejection rode in on: the HTTP status for a
	// plain 403 response, or the frame's statusCodeValue when the ticket
	// travelled inside a 200-OK SSE envelope. 0 when unknown.
	status int
}

// parseQueueSignal walks body (an upstream error payload from either the HTTP
// layer or an SSE frame) looking for the queue ticket at any nesting depth.
// The gateway nests the actionable object inside JSON-string "message" (and
// sometimes "details") fields, so both are unwrapped recursively; the walk is
// depth-bounded and collects metadata below an identified queue envelope.
func parseQueueSignal(body string) queueSignal {
	var sig queueSignal
	walkQueueBody(strings.TrimSpace(body), &sig, 0, false)
	if sig.detected {
		if sig.retryAfter <= 0 {
			sig.retryAfter = queueWaitDefault
		}
		if sig.retryAfter > queueWaitCap {
			sig.retryAfter = queueWaitCap
		}
		// Head of the queue: the ticket itself says nobody is ahead, yet it
		// still carries the same 30s estimate. Sleeping that three times buys
		// nothing when the next call is usually admitted, so those tickets run
		// the fast ladder instead. A ticket with a real depth keeps its
		// interval and the general attempt/budget bounds.
		if sig.depthSeen && sig.queueCount <= 0 {
			sig.emptyQueue = true
			sig.retryAfter = queueFastRetryBackoff
		}
	}
	return sig
}

// walkQueueBody is parseQueueSignal's recursion. depth guards against
// pathological nesting. Layers do not short-circuit on the first hit: the
// observed envelope splits the identification (code 10605 on L2) from the
// payload (retryAfterSeconds/modelKey on L3), so every layer gets to
// contribute what it carries. inQueue applies only to descendants of an
// identified envelope, so an unrelated sibling cannot overwrite its metadata.
func walkQueueBody(s string, sig *queueSignal, depth int, inQueue bool) {
	if depth > 4 {
		return
	}
	if !strings.HasPrefix(s, "{") {
		return
	}
	var obj struct {
		Code              json.RawMessage `json:"code"`
		SubCode           string          `json:"subCode"`
		Message           json.RawMessage `json:"message"`
		Details           json.RawMessage `json:"details"`
		RetryAfterSeconds float64         `json:"retryAfterSeconds"`
		WaitTime          float64         `json:"waitTime"`
		QueueType         string          `json:"queueType"`
		ModelKey          string          `json:"modelKey"`
		QueueCount        json.RawMessage `json:"queueCount"`
	}
	if json.Unmarshal([]byte(s), &obj) != nil {
		return
	}
	if codeScalarString(obj.Code) == queueCode || strings.EqualFold(strings.TrimSpace(obj.SubCode), queueSubCode) {
		sig.detected = true
		inQueue = true
	}
	if inQueue {
		if v := strings.TrimSpace(obj.SubCode); v != "" {
			sig.subCode = v
		}
		switch {
		case obj.RetryAfterSeconds > 0:
			sig.retryAfter = time.Duration(obj.RetryAfterSeconds * float64(time.Second))
		case obj.WaitTime > 0:
			sig.retryAfter = time.Duration(obj.WaitTime * float64(time.Second))
		}
		if v := strings.TrimSpace(obj.QueueType); v != "" {
			sig.queueType = v
		}
		if v := strings.TrimSpace(obj.ModelKey); v != "" {
			sig.modelKey = v
		}
		// A malformed depth must not read as "nobody ahead", so only a value
		// that actually parses counts as seen.
		if v, ok := codeScalarInt(obj.QueueCount); ok {
			sig.depthSeen = true
			sig.queueCount = v
		}
	}
	// The ticket may be one level deeper (L2 hits on code, L3 carries
	// retryAfterSeconds) — keep walking the JSON-string carriers.
	if nested := jsonObjectString(obj.Message); nested != "" {
		walkQueueBody(nested, sig, depth+1, inQueue)
	}
	if nested := jsonObjectString(obj.Details); nested != "" {
		walkQueueBody(nested, sig, depth+1, inQueue)
	}
}

// codeScalarString renders a JSON code field that is either a quoted string
// ("10605") or a number (10605) as its plain text form; anything else is "".
func codeScalarString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// codeScalarInt renders a numeric metadata field as an int, accepting both the
// number shape (0) and the quoted-string shape ("0") the gateway mixes into
// these envelopes. ok=false means the field was absent or not numeric.
func codeScalarInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int(f), true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int(v), true
		}
	}
	return 0, false
}

// jsonObjectString returns the JSON *string* carried by raw only when its
// content itself looks like a JSON object — the gateway's nested-envelope
// convention. Plain-text messages return "" so the recursion never chases
// prose.
func jsonObjectString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") {
		return t
	}
	return ""
}

// queueLimitLine preserves the upstream sub-code without assuming every 10605
// rejection is model-capacity trouble unrelated to the account.
// The queue_limit prefix is load-bearing — recordUpstreamFailure's classifier
// keys on it to apply the short (auth, model) cooldown.
func queueLimitLine(sig queueSignal) string {
	line := queueMarker + ": code=" + queueCode
	if sig.subCode != "" {
		line += " subCode=" + sig.subCode
	}
	if sig.emptyQueue {
		line += " " + queueHeadMarker
	}
	line += " 上游排队或受限"
	if sig.modelKey != "" {
		line += "，model=" + sig.modelKey
	}
	if sig.queueType != "" {
		line += "，队列=" + sig.queueType
	}
	if sig.emptyQueue {
		line += "（票据自报队列已空 queueCount=0）"
		line += " — 瞬时并发下的空队列票，直接重试或改用其他模型"
		return line
	}
	line += " — 按上游建议等待后重试，或改用其他模型"
	return line
}

// queueRetryAllowed reports whether one more queue retry round fits the
// attempt count and the cumulative wait budget. attempt is the number of
// retries already performed (0 on the original request).
func queueRetryAllowed(attempt int, waited time.Duration, wait time.Duration) bool {
	return queueRetryAllowedFor(attempt, waited, wait, queueMaxAttempts)
}

// queueRetryAllowedFor is queueRetryAllowed's ladder-parameterised form: the
// empty-queue ladder is bounded by its own attempt count so it cannot turn into
// a long wait if the gateway keeps issuing tickets.
func queueRetryAllowedFor(attempt int, waited time.Duration, wait time.Duration, maxAttempts int) bool {
	if wait <= 0 {
		wait = queueWaitDefault
	}
	if wait > queueWaitCap {
		wait = queueWaitCap
	}
	queueBudgetMu.RLock()
	budget := queueWaitBudget
	queueBudgetMu.RUnlock()
	return attempt+1 < maxAttempts && waited+wait <= budget
}

// queueRetryContinue is the executors' decision point for one queue ticket. It
// combines the empty-queue ladder's attempt ceiling with the per-model window
// that keeps credential rotation from paying for the ladder N times inside one
// logical request.
//
// A ticket that carries a real queue depth keeps the v0.8.64 behaviour: retry
// while the attempt count and the queue budget allow it.
func queueRetryContinue(attempt int, waited time.Duration, sig queueSignal) bool {
	if !sig.detected {
		return false
	}
	if !sig.emptyQueue {
		return queueRetryAllowed(attempt, waited, sig.retryAfter)
	}
	if attempt == 0 {
		// Exactly one credential runs the ladder per window; the siblings the
		// host rotates through fail fast with the same actionable ticket text.
		// The first round is bounded like every other one, so a configured
		// budget of 0 still disables queue retries outright.
		if !queueRetryAllowedFor(0, waited, queueFastRetryBackoff, queueFastAttempts) {
			return false
		}
		return queueFastLadderClaim(sig.modelKey, time.Now())
	}
	return queueRetryAllowedFor(attempt, waited, queueFastRetryBackoff, queueFastAttempts)
}

var (
	queueFastLadderMu sync.Mutex
	queueFastLadderAt = map[string]time.Time{}
)

// queueFastLadderClaim reports whether this execution may run the empty-queue
// ladder, claiming the window when it may. Failures land here once per
// credential — the host rotates on a failed execution — so the window is keyed
// by model, not by credential.
func queueFastLadderClaim(modelKey string, now time.Time) bool {
	key := strings.ToLower(strings.TrimSpace(modelKey))
	queueFastLadderMu.Lock()
	defer queueFastLadderMu.Unlock()
	if last, ok := queueFastLadderAt[key]; ok && now.Sub(last) < queueFastLadderWindow {
		return false
	}
	queueFastLadderAt[key] = now
	return true
}

// queueWait sleeps for the ticket's interval, honouring ctx cancellation. It
// returns false when ctx fired first (the client is gone — abandon the call
// without recording any failure). A nil ctx waits unconditionally, matching
// the synchronous paths that have no request context today.
//
// Package var so tests can run the retry loops at full speed.
var queueWaitFn = func(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = queueWaitDefault
	}
	if d > queueWaitCap {
		d = queueWaitCap
	}
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// queueWaitFor clamps a ticket interval to the per-round cap.
func queueWaitFor(d time.Duration) time.Duration {
	if d <= 0 {
		return queueWaitDefault
	}
	if d > queueWaitCap {
		return queueWaitCap
	}
	return d
}

// queueExhaustedLine renders the terminal failure after the retry budget is
// spent: the actionable queue line plus what the plugin already did about it,
// so a user reading a Docker log understands the 90s of silence.
func queueExhaustedLine(sig queueSignal, attempts int, waited time.Duration) string {
	return queueLimitLine(sig) + queueExhaustedNote(attempts, waited, sig)
}

// queueExhaustedNote renders the retry history: an empty-queue ticket was
// retried on the fast ladder, so reporting the ticket's 30s interval there
// would misdescribe what happened.
func queueExhaustedNote(attempts int, waited time.Duration, sig queueSignal) string {
	if sig.emptyQueue {
		return fmt.Sprintf("（票据自报队列已空，已快速重试%d次共%.1fs仍未放行）", attempts, waited.Seconds())
	}
	return fmt.Sprintf("（已自动重试%d次共%.0fs仍排队）", attempts, waited.Seconds())
}
