// growth.go implements the CN growth-center (成长中心) upstream surface:
// task list / accept / reward claim, streak tiers + lottery, buddy travel,
// the daily chat-activity report, gift / compensation claims and makeup
// cards. Ported from workbuddy2api-panel internal/upstream (tasks.go /
// travel.go / streak.go / report.go / blackcat.go) whose endpoints were
// verified against the production API (2026-09 probes + multi-account
// trials recorded there).
//
// Three upstream domains cooperate (CN realm):
//   - growth domain  https://copilot.tencent.com  (upstreamBaseCN) — task
//     list / accept, travel, streak, lottery, heatmap, makeup cards, with
//     the same billing header set as the check-in family;
//   - billing domain https://www.codebuddy.cn     (billingBase) — chat
//     activity report (/v2/report) + gift / compensation claims;
//   - web domain     https://www.workbuddy.cn     — the ONLY host that
//     serves POST /activity/growth/tasks/<code>/claim. The same path on
//     copilot.tencent.com returns 400 "task not completed" — that trap is
//     exactly why the claim lives in its own helper with web headers.
//
// Realm gating: the growth center only exists for CN accounts. Global /
// Intl realms have no such endpoint family upstream (wb2api gates global
// out with the same reasoning) — taskcenter callers skip them before any
// upstream call is made.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// growth-center endpoint paths (wb2api-verified shapes; do not "normalize"
// the /v2 prefix away — tasks carry it, travel/streak do not).
const (
	growthTasksListPath   = "/v2/activity/growth/tasks"
	growthTasksAcceptPath = "/v2/activity/growth/tasks/accept"

	growthTravelStatusPath = "/activity/growth/buddy/travel/status"
	growthTravelDepartPath = "/activity/growth/buddy/travel/depart"
	growthTravelClaimPath  = "/activity/growth/buddy/travel/claim"
	growthBuddyInfoPath    = "/activity/growth/buddy/info"
	growthBuddyFirstPath   = "/activity/growth/buddy/first"
	growthBuddyAgreePath   = "/activity/growth/buddy/agreement"

	growthStreakPath         = "/activity/growth/streak"
	growthRedeemPath         = "/activity/growth/redeem"
	growthLotterySummary     = "/activity/growth/lottery/summary"
	growthLotteryChancesPath = "/activity/growth/lottery/chances"
	growthLotteryDrawPath    = "/activity/growth/lottery/draw"
	growthBuddyQuotaPath     = "/activity/growth/buddy/quota"
	growthBuddyOpenPath      = "/activity/growth/buddy/open"
	growthEnergyPath         = "/activity/growth/energy"
	growthHeatmapPath        = "/activity/growth/heatmap"
	growthMakeupUsePath      = "/activity/growth/makeup-cards/use"
	billingReportPath        = "/v2/report"
	billingClaimGiftPath     = "/billing/meter/claim-gift"
	billingClaimCompensPath  = "/billing/meter/claim-compensation"
)

// growthWebBaseCN hosts the web growth-center claim endpoint (workbuddy.cn,
// not codebuddy.cn — see ClaimReward notes in wb2api tasks.go). Var so tests
// can retarget it with an httptest server.
var growthWebBaseCN = "https://www.workbuddy.cn"

// growthClientToken mints the client_token idempotency token the streak
// redeem / lottery draw endpoints expect (front-end randomUUID semantics).
func growthClientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// growthCall is the generic growth-domain request (copilot.tencent.com for
// CN) with the billing header family. body == nil sends no request body
// (GET semantics); otherwise it is JSON-marshalled. Envelope and error
// semantics match billingCallOnce: HTTP >= 500 → transient-shaped error,
// code != 0 → business error carrying code+msg.
// growthBaseOverride lets tests retarget the growth domain (same pattern as
// setBillingBase); empty means the CN const applies.
var growthBaseOverride string

func setGrowthBase(s string) func() {
	old := growthBaseOverride
	growthBaseOverride = s
	return func() { growthBaseOverride = old }
}

func growthBase() string {
	if growthBaseOverride != "" {
		return growthBaseOverride
	}
	return upstreamBaseCN
}

func growthCall(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, growthBase()+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	return growthDo(req)
}

// growthBillingCall posts to the billing domain (codebuddy.cn) without the
// retry wrapper — used for /v2/report and the gift / compensation claims
// where a silent double-send would corrupt activity counters.
func growthBillingCall(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(http.MethodPost, billingBaseFor(sa)+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	return growthDo(req)
}

// growthDo routes the request through the host bridge and decodes the
// {code,msg,data} envelope (same contract as billingCallOnce, minus the
// 5xx retry loop — growth actions are not idempotent-safe to blind-retry).
func growthDo(req *http.Request) (json.RawMessage, error) {
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(redactSecrets(string(resp.Body)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		// Prefer the envelope msg when the failure body is our usual
		// {code,msg,data} shape; fall back to the raw snippet (WAF pages).
		var env apiEnvelope
		msg := ""
		if json.Unmarshal(resp.Body, &env) == nil && env.Code != 0 {
			msg = truncateRedacted(env.Msg, 120)
		}
		if msg == "" {
			msg = snippet
		}
		return nil, &growthHTTPError{status: resp.StatusCode, msg: msg}
	}
	var env apiEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		snippet := strings.TrimSpace(redactSecrets(string(resp.Body)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, snippet)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

// growthHTTPError carries the upstream HTTP status of a growth-center call so
// callers can separate credential-level rejections (401/403 — Coding2API
// growth_runner discipline: abort the run, every further call just 401s
// again) from business rules (tier locked / quota exhausted) and
// infrastructure failures (5xx).
type growthHTTPError struct {
	status int
	msg    string
}

func (e *growthHTTPError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("http %d: %s", e.status, e.msg)
	}
	return fmt.Sprintf("http %d", e.status)
}

// growthErrStatus reports the HTTP status carried by err (0 when none).
func growthErrStatus(err error) int {
	var he *growthHTTPError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}

// isGrowthSessionDead mirrors Coding2API growth_runner._is_session_dead:
// 401/403 means the credential was rejected — continuing would just pile up
// more rejections. The tier-locked 403 ("连续登录天数不足") is exempt: it is
// the normal "tier not unlocked" state, and treating it as session death
// would abort the whole run over a healthy credential (upstream issue #6).
func isGrowthSessionDead(err error) bool {
	st := growthErrStatus(err)
	if st != 401 && st != 403 {
		return false
	}
	return !isGrowthTierLocked(err)
}

// isGrowthTierLocked reports the expected 403 "连续登录天数不足" answer for
// an unredeemed-tier attempt — a normal state, not a failure.
func isGrowthTierLocked(err error) bool {
	if growthErrStatus(err) != 403 {
		return false
	}
	return strings.Contains(err.Error(), "不足")
}

// isGrowthUnknownTier reports the 400 "unknown tier" parameter-form
// rejection — the server redeemed nothing, so retrying with the legacy
// day-number form is safe (Coding2API is_unknown_tier, f18dc3d).
func isGrowthUnknownTier(err error) bool {
	if growthErrStatus(err) != 400 {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "tier") &&
		(strings.Contains(msg, "unknown") || strings.Contains(msg, "invalid") ||
			strings.Contains(msg, "unsupported"))
}

// -----------------------------------------------------------------------------
// Growth tasks: list / accept / claim
// -----------------------------------------------------------------------------

// growthTask is one entry of the growth task list (field names follow the
// upstream JSON; Progress is kept raw because it appears in two shapes —
// {"current":N,"target":M} object or flat top-level fields).
type growthTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	TaskDesc     string `json:"task_desc,omitempty"`
	RewardCredit int64  `json:"reward_credit,omitempty"`
	RewardEnergy int64  `json:"reward_energy,omitempty"`
	HasReward    bool   `json:"has_reward,omitempty"`
	TaskType     string `json:"task_type,omitempty"`
	Tag          string `json:"tag,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	// Derived (not upstream fields):
	Claimable bool `json:"claimable,omitempty"`
	Claimed   bool `json:"claimed,omitempty"`
}

// parseGrowthTasks decodes the task list. Progress may arrive as an object
// {"current","target"} overriding the flat fields (wb2api observed both
// shapes across task types); a null/absent progress keeps the flat values.
func parseGrowthTasks(data json.RawMessage) []growthTask {
	var resp struct {
		Tasks []struct {
			growthTask
			Progress json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}
	out := make([]growthTask, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		task := t.growthTask
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				task.Current, task.Target = pr.Current, pr.Target
			}
		}
		task.Claimed = task.AcceptStatus == "claimed"
		// 2026-09 five-state contract: accept_status=="completed" is the
		// authoritative claim signal (progress fields lag on some task types);
		// the progress-reached test stays as the fallback for shapes where
		// completed is not yet published.
		task.Claimable = !task.Claimed && !task.Locked &&
			(task.AcceptStatus == "completed" || (task.Target > 0 && task.Current >= task.Target))
		out = append(out, task)
	}
	return out
}

func growthListTasks(sa *storedAuth) ([]growthTask, error) {
	data, err := growthCall(sa, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data), nil
}

// growthAcceptResult is one per-task outcome of a batch accept call
// (2026-09 contract: data.results carries each code's status — "error"
// entries carry the upstream message, e.g. "prerequisite not met").
type growthAcceptResult struct {
	TaskCode string `json:"task_code"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// growthAcceptTasks accepts (enrolls) tasks. Upstream counts progress only
// for accepted tasks — skipping this step is the classic "report 200 but
// progress stuck at not_accepted" failure. The 2026-09 contract requires the
// PLURAL array body {"task_codes": [...]} (the singular form answers 400) and
// reports per-task outcomes in data.results; surfacing them is the only way
// per-task rejections stay visible (silence here is how five tasks piled up
// 650 unclaimed credits on a real account upstream). When the upstream omits
// results, every code is treated as accepted.
func growthAcceptTasks(sa *storedAuth, taskCodes []string) ([]growthAcceptResult, error) {
	if len(taskCodes) == 0 {
		return nil, nil
	}
	data, err := growthCall(sa, http.MethodPost, growthTasksAcceptPath, map[string]any{"task_codes": taskCodes})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []growthAcceptResult `json:"results"`
	}
	_ = json.Unmarshal(data, &resp)
	if len(resp.Results) > 0 {
		return resp.Results, nil
	}
	out := make([]growthAcceptResult, 0, len(taskCodes))
	for _, code := range taskCodes {
		out = append(out, growthAcceptResult{TaskCode: code, Status: "ok"})
	}
	return out, nil
}

// growthClaimReward claims ONE task's reward on the WEB domain (workbuddy.cn).
// The same path on the CLI domain does not exist (400 "task not completed").
// Returns the credited credit/energy (0/0 when already claimed — idempotent).
func growthClaimReward(sa *storedAuth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		growthWebBaseCN+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", bytes.NewReader(nil))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", growthWebBaseCN)
	req.Header.Set("Referer", growthWebBaseCN+"/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
	data, err := growthDo(req)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil
	}
	return resp.Credit, resp.Energy, nil
}

// -----------------------------------------------------------------------------
// Streak (连登) tiers + lottery
// -----------------------------------------------------------------------------

type growthStreakFull struct {
	Streak struct {
		Days              int    `json:"days"`
		MonthTotalDays    int    `json:"month_total_days"`
		NextTier          string `json:"next_tier"`
		NextTierRemaining int    `json:"next_tier_remaining"`
	} `json:"streak"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max     int `json:"max"`
	} `json:"makeup_cards"`
	RedemptionStatus struct {
		Tier7dStatus  string `json:"tier_7d_status"`
		Tier14dStatus string `json:"tier_14d_status"`
		Tier28dStatus string `json:"tier_28d_status"`
		Tiers         []struct {
			Tier    string `json:"tier"`
			Days    int    `json:"days"`
			Credit  int    `json:"credit"`
			Energy  int    `json:"energy"`
			Cards   int    `json:"cards"`
			Chances int    `json:"chances"`
		} `json:"tiers"`
	} `json:"redemption_status"`
}

func growthStreak(sa *storedAuth) (*growthStreakFull, error) {
	data, err := growthCall(sa, http.MethodGet, growthStreakPath, nil)
	if err != nil {
		return nil, err
	}
	out := &growthStreakFull{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// growthRedeemTier exchanges one streak tier ("7d"/"14d"/"28d") and returns
// what was actually granted. Locked tiers answer 403 "连续登录天数不足" —
// callers treat that as expected skip (isGrowthTierLocked). A 400 "unknown
// tier" is a parameter-form rejection (nothing was redeemed) — retry once
// with the legacy day-number form (Coding2API f18dc3d keeps the same
// fallback). The 2026-09 response carries the GRANTED fields
// (credit_granted/energy_granted); the bare credit/energy members are empty
// on this endpoint, so reading them reports zero for every redemption.
func growthRedeemTier(sa *storedAuth, tier string) (credit, energy int64, err error) {
	credit, energy, err = growthRedeemOnce(sa, tier)
	if err != nil && isGrowthUnknownTier(err) {
		if days, ok := growthTierDays[tier]; ok {
			credit, energy, err = growthRedeemOnce(sa, days)
		}
	}
	return credit, energy, err
}

// growthTierDays maps tier ids to the legacy day-number parameter form —
// only used for the unknown-tier fallback retry.
var growthTierDays = map[string]int{"7d": 7, "14d": 14, "28d": 28}

func growthRedeemOnce(sa *storedAuth, tier any) (int64, int64, error) {
	data, err := growthCall(sa, http.MethodPost, growthRedeemPath,
		map[string]any{"tier": tier, "client_token": growthClientToken()})
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		CreditGranted int64 `json:"credit_granted"`
		EnergyGranted int64 `json:"energy_granted"`
		Credit        int64 `json:"credit"`
		Energy        int64 `json:"energy"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.CreditGranted != 0 || resp.EnergyGranted != 0 {
		return resp.CreditGranted, resp.EnergyGranted, nil
	}
	return resp.Credit, resp.Energy, nil
}

// growthLotteryChances returns the current lottery draw balance. The
// 2026-09 contract serves it at /lottery/chances (balance field); the older
// /lottery/summary (chances field) stays as the fallback so an endpoint
// rollout never silently zeroes the draw loop. A session-dead answer is
// propagated instead of retried on the legacy endpoint.
func growthLotteryChances(sa *storedAuth) (int, error) {
	data, err := growthCall(sa, http.MethodGet, growthLotteryChancesPath, nil)
	if err == nil {
		var resp struct {
			Balance int `json:"balance"`
			Chances int `json:"chances"`
		}
		if json.Unmarshal(data, &resp) == nil {
			if resp.Balance != 0 {
				return resp.Balance, nil
			}
			if resp.Chances != 0 {
				return resp.Chances, nil
			}
			return 0, nil // a valid "nothing to draw" on the new endpoint
		}
	} else if isGrowthSessionDead(err) {
		return 0, err
	}
	data, err = growthCall(sa, http.MethodGet, growthLotterySummary, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Chances int `json:"chances"`
		Module  struct {
			Enabled bool `json:"enabled"`
		} `json:"module"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Chances, nil
}

// growthLotteryDraw spends one chance and returns the raw prize payload
// (shape is campaign-dependent — surfaced to the caller as-is).
func growthLotteryDraw(sa *storedAuth) (json.RawMessage, error) {
	return growthCall(sa, http.MethodPost, growthLotteryDrawPath,
		map[string]any{"client_token": growthClientToken()})
}

// growthBuddyQuota returns (affordable, costPerOpen, maxOpen) for the energy
// buddy box. This is the ENERGY SINK: energy has no other spend outlet, so
// unspent energy just sits there (Coding2API _buddy_box).
func growthBuddyQuota(sa *storedAuth) (affordable, costPerOpen, maxOpen int, err error) {
	data, err := growthCall(sa, http.MethodGet, growthBuddyQuotaPath, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		Affordable   int `json:"affordable"`
		CostPerOpen  int `json:"cost_per_open"`
		MaxOpenCount int `json:"max_open_count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, err
	}
	if resp.MaxOpenCount <= 0 {
		resp.MaxOpenCount = 1
	}
	return resp.Affordable, resp.CostPerOpen, resp.MaxOpenCount, nil
}

// growthBuddyOpen spends energy to open buddy boxes (count per call; the
// 2026-09 shape takes {"count": n, "client_token": ...}).
func growthBuddyOpen(sa *storedAuth, count int) (json.RawMessage, error) {
	return growthCall(sa, http.MethodPost, growthBuddyOpenPath,
		map[string]any{"count": count, "client_token": growthClientToken()})
}

// growthEnergyBalance reads the energy balance (display-only tail step).
func growthEnergyBalance(sa *storedAuth) (int64, error) {
	data, err := growthCall(sa, http.MethodGet, growthEnergyPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int64 `json:"balance"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Balance, nil
}

// -----------------------------------------------------------------------------
// Buddy travel (猫猫旅行) state machine
// -----------------------------------------------------------------------------

// Travel state values reported by the status endpoint.
const (
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// growthTravel is the travel status payload.
type growthTravel struct {
	State             string `json:"state"`
	DailyLimitReached bool   `json:"daily_limit_reached"`
	RecordID          int64  `json:"record_id"`
	RewardCredit      int64  `json:"reward_credit"`
}

func growthTravelStatus(sa *storedAuth) (*growthTravel, error) {
	data, err := growthCall(sa, http.MethodGet, growthTravelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	st := &growthTravel{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}

// growthTravelLocation is the fixed depart location. All four locations
// share the same reward/duration range (wb2api measured) — no optimum.
const growthTravelLocation = 4

func growthTravelDepart(sa *storedAuth, locationID int) error {
	_, err := growthCall(sa, http.MethodPost, growthTravelDepartPath,
		map[string]any{"location_id": locationID})
	return err
}

func growthTravelClaim(sa *storedAuth, recordID int64) (int64, error) {
	data, err := growthCall(sa, http.MethodPost, growthTravelClaimPath,
		map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit       int64 `json:"credit"`
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp) // missing reward field is not fatal
	}
	// 2026-09 contract: the claim response reads credit (parse_reward);
	// reward_credit stays as the fallback for the older shape.
	if resp.Credit != 0 {
		return resp.Credit, nil
	}
	return resp.RewardCredit, nil
}

// growthBuddyInfo returns the account's buddy profile; (nil, nil) means the
// account has no buddy yet (data.buddy == null).
func growthBuddyInfo(sa *storedAuth) (*struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}, error) {
	data, err := growthCall(sa, http.MethodGet, growthBuddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	b := &struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}{}
	if err := json.Unmarshal(resp.Buddy, b); err != nil {
		return nil, err
	}
	return b, nil
}

func growthBuddyFirst(sa *storedAuth) error {
	_, err := growthCall(sa, http.MethodPost, growthBuddyFirstPath, map[string]any{})
	return err
}

func growthBuddyAgreement(sa *storedAuth) error {
	_, err := growthCall(sa, http.MethodPost, growthBuddyAgreePath, map[string]any{"agree": true})
	return err
}

// -----------------------------------------------------------------------------
// Activity report (活跃上报) + gift / compensation + makeup cards
// -----------------------------------------------------------------------------

// growthChatEvent mirrors the client chat_request_send telemetry event.
// UserID is REQUIRED — with it missing the server answers 200 but silently
// drops the event (wb2api REPORT-active-map.md §2), so it is part of the
// struct, not an optional decoration. The full field list is intentional:
// the upstream tightened validation before; do not trim to 3 fields.
type growthChatEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// growthReportActivity posts one chat_request_send event to /v2/report.
// One report per account per day lights the growth streak and unlocks the
// first_buddy adoption gate; the report does NOT need a real conversation —
// the conversationID is caller-generated. Deliberately no retry: the event
// is day-idempotent upstream and a blind resend would skew counters.
func growthReportActivity(sa *storedAuth, conversationID, requestID string) error {
	return growthReportActivityModel(sa, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// growthReportActivityModel 同上，但上报可携带指定模型：「体验某模型」类任务
// （Model_chat_GLM5.2、black_cat）判据对齐 requestModelId 与实际对话一致
// ——对话用 glm-5.2、上报却带默认 flash 会被判不匹配。
func growthReportActivityModel(sa *storedAuth, conversationID, requestID, modelID, modelName string) error {
	if conversationID == "" {
		conversationID = "wb-" + growthClientToken()
	}
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := growthChatEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               sa.Account.UID,
	}
	raw, err := json.Marshal([]growthChatEvent{ev})
	if err != nil {
		return err
	}
	_, err = growthBillingCall(sa, billingReportPath, json.RawMessage(raw))
	return err
}

// growthClaimGift / growthClaimCompensation: one-shot bonus credits.
// Already-claimed answers a business error — callers log-and-move-on.
func growthClaimGift(sa *storedAuth) (int64, error) {
	data, err := growthBillingCall(sa, billingClaimGiftPath, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

func growthClaimCompensation(sa *storedAuth) (int64, error) {
	data, err := growthBillingCall(sa, billingClaimCompensPath, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// growthYesterdayMissed reports whether yesterday's heatmap cell is empty
// (score == 0) — the makeup-card precondition.
func growthYesterdayMissed(sa *storedAuth) (bool, error) {
	data, err := growthCall(sa, http.MethodGet, growthHeatmapPath, nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// growthUseMakeupCard spends one makeup card on the given date to preserve
// the streak continuity (a broken streak restarts the 7-day climb).
func growthUseMakeupCard(sa *storedAuth, date string) error {
	_, err := growthCall(sa, http.MethodPost, growthMakeupUsePath,
		map[string]any{"target_date": date})
	return err
}

// growthPrereqMarker tasks/accept 逐条结果里前置未满足的固定前缀
// （Coding2API growth_runner 2026-09-20 对齐）。
const growthPrereqMarker = "prerequisite not met:"

// growthPrerequisiteOf 从接单失败消息提取前置任务 code；非前置类返回 ("", false)。
// 消息形如 "prerequisite not met: first_buddy"。前置缺失是账号状态常态
// （新账号全部任务被 first_buddy 门住），不是逐条报的故障。
func growthPrerequisiteOf(msg string) (string, bool) {
	lower := strings.ToLower(msg)
	i := strings.Index(lower, growthPrereqMarker)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(msg[i+len(growthPrereqMarker):])
	if rest == "" {
		return "", true
	}
	return strings.Trim(strings.Fields(rest)[0], ".,;"), true
}

// growthPrerequisiteLabel 前置任务可读标签：接口只回 task_code，这里补一句
// 用户该做什么（未知 code 回落 code 本身）。
func growthPrerequisiteLabel(code string) string {
	if code == "first_buddy" {
		return "领取一只 Buddy（在官方客户端新建任务并发起对话）"
	}
	return code
}

// growthAcceptNeedsNoAccept 上游明示该任务无需接单——正常应答而非失败
// （此前当失败报，误导用户以为出了问题；对齐上游 growth_runner 2026-09-20）。
func growthAcceptNeedsNoAccept(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "does not require acceptance")
}
