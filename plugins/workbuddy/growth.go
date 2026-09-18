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

	growthStreakPath        = "/activity/growth/streak"
	growthRedeemPath        = "/activity/growth/redeem"
	growthLotterySummary    = "/activity/growth/lottery/summary"
	growthLotteryDrawPath   = "/activity/growth/lottery/draw"
	growthHeatmapPath       = "/activity/growth/heatmap"
	growthMakeupUsePath     = "/activity/growth/makeup-cards/use"
	billingReportPath       = "/v2/report"
	billingClaimGiftPath    = "/billing/meter/claim-gift"
	billingClaimCompensPath = "/billing/meter/claim-compensation"
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
	req, err := http.NewRequest(method, upstreamBaseCN+path, reader)
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
	if resp.StatusCode >= 500 {
		snippet := strings.TrimSpace(redactSecrets(string(resp.Body)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, req.URL.Path, snippet)
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
		task.Claimable = !task.Claimed && task.Locked == false && task.Target > 0 && task.Current >= task.Target
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

// growthAcceptTasks accepts (enrolls) tasks. Upstream counts progress only
// for accepted tasks — skipping this step is the classic "report 200 but
// progress stuck at not_accepted" failure. Idempotent: already-accepted
// tasks return success or a benign business message.
func growthAcceptTasks(sa *storedAuth, taskCodes []string) error {
	if len(taskCodes) == 0 {
		return nil
	}
	_, err := growthCall(sa, http.MethodPost, growthTasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
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

// growthRedeemTier exchanges one streak tier ("7d"/"14d"/"28d"). Locked
// tiers answer 403 "连续登录天数不足" — callers treat that as expected skip.
func growthRedeemTier(sa *storedAuth, tier string) error {
	_, err := growthCall(sa, http.MethodPost, growthRedeemPath,
		map[string]any{"tier": tier, "client_token": growthClientToken()})
	return err
}

// growthLotteryChances returns the current lottery draw balance.
func growthLotteryChances(sa *storedAuth) (int, error) {
	data, err := growthCall(sa, http.MethodGet, growthLotterySummary, nil)
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
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp) // missing reward field is not fatal
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
	if conversationID == "" {
		conversationID = "wb-" + growthClientToken()
	}
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	ev := growthChatEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       "deepseek-v4-flash",
		RequestModelName:     "DeepSeek V4 Flash",
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
