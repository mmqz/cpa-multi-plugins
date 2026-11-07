// cooldown.go — per-(credential, model) cooldown table (issue #35).
//
// Field report (issue #35, fyzero): a workbuddy.ai Global account hitting a
// free-model (0x) DAILY LIMIT answers each request with HTTP 200 + an
// in-stream error frame (or an immediately-empty stream) in ~300 ms. The
// pre-0.9.57 surface recorded nothing the host could act on:
//
//   - usage monitoring logged failed=0 / in=0 / out=0 (the reporter's build
//     predated the v0.9.29 frame detector, but even with it the failure only
//     surfaced in-band on an already-200 stream);
//   - the host cooldown layer (statusError -> MarkResult) never fired — the
//     200 was already handed off, and stream_head_timeout defaults to off;
//   - session affinity kept pinning the conversation to the drained
//     credential; the only recovery was disabling the account by hand.
//
// The 6004 model-scoped rate limit (429 + declared reset) already had its own
// registry (model_ratelimit.go); everything else an upstream can do to a
// single (credential, model) pair had no owner. This file ports the
// qoder/zcode pattern (v0.8.18 / v0.12.76):
//
//  1. recordUpstreamFailure routes a failure body into the table:
//     rate-limit wording  -> 300 s hold,
//     in-stream error frames / empty streams -> 60 s hold,
//     hard credit exhaustion and 6004 -> deliberately NOT tabled (account
//     level stays with the lifecycle/statusError path; 6004's declared
//     reset stays authoritative in model_ratelimit.go).
//  2. The per-credential model catalog (handleModelForAuth) drops cooling
//     pairs, so the host's built-in scheduler stops re-offering them.
//  3. The opt-in scheduler picker (scheduler_mode: credits) skips cooling
//     candidates — same agreement qoder's picker has with its filter.
//  4. Executors fast-fail a cooling pair before any upstream call
//     (modelCooldownError, mirroring the 6004 fast-fail).
//
// The 60 s catch-all on in-stream frames is the honest answer to the issue's
// open question ("what does the Global gateway return for a daily limit?"):
// we cannot pattern-match wording we have never seen, but the frame detector
// (workBuddyStreamFrame) only fires on an upstream that REFUSED on a 200 —
// rotating that pair for a minute is mild, self-expiring, and strictly
// better than silently hammering the drained credential.
//
// v0.9.88 (issue #77): the management listing/clear endpoints merge the 6004
// model-rate-limit windows in — one operator surface for every
// per-(credential, model) hold, whichever table owns it.
//
// State is process-local: it survives config reloads but not a host restart.
package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// modelCooldownRateLimit backs off rate-limit-shaped failures: upstream
	// explicitly asked us to slow down on the pair.
	modelCooldownRateLimit = 300 * time.Second
	// modelCooldownUnknown covers empty streams and in-stream error frames:
	// short enough that a transient glitch re-probes quickly, long enough to
	// break the sticky-session retry loop the issue documents.
	modelCooldownUnknown = 60 * time.Second
	// modelCooldownMaxEntries bounds the table against a hostile upstream.
	modelCooldownMaxEntries = 4096
)

type cooldownReason string

const (
	cooldownReasonRateLimit cooldownReason = "rate_limit"
	cooldownReasonUnknown   cooldownReason = "upstream_error"
	// cooldownReasonDailyQuota（issue #35 补充分级，v0.9.58）：免费 0x 模型的
	// 每日限额措辞 —— 每日额度的自然重置点是 UTC+8 零点，60s 的 unknown
	// 窗口会让被限的 (账号, 模型) 对每分钟空转一次重试（最多 1440 次/天），
	// 持有到午夜是诚实且自愈的窗口。
	cooldownReasonDailyQuota cooldownReason = "daily_quota"
)

type modelCooldownKey struct {
	AuthID  string
	ModelID string
}

type modelCooldownEntry struct {
	Reason    cooldownReason
	Until     time.Time
	UpdatedAt time.Time
}

var (
	cooldownMu    sync.Mutex
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownNowFn = time.Now
)

// cooldownAuthIDFor derives the cooldown key for a credential, mirroring the
// host auth ID convention in toAuthDataOptsWithNote (sanitized UID, else
// provider name). Scheduler candidates carry the same identifier in
// SchedulerAuthCandidate.ID, and model.for_auth carries it in req.AuthID, so
// keys recorded at failure time match keys queried by every consumer.
func cooldownAuthIDFor(sa *storedAuth) string {
	if sa != nil {
		if id := sanitizeUIDForFileName(sa.Account.UID); id != "" {
			return id
		}
	}
	return providerName
}

// markModelCooldown records (or re-arms) a throttled (credential, model)
// pair. Empty halves are ignored: without a model the entry would freeze the
// credential, without an auth id it would freeze the model everywhere.
func markModelCooldown(authID, model string, reason cooldownReason) {
	authID = strings.TrimSpace(authID)
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" {
		return
	}
	ttl := modelCooldownUnknown
	if reason == cooldownReasonRateLimit {
		ttl = modelCooldownRateLimit
	}
	markModelCooldownUntil(authID, model, reason, cooldownNowFn().Add(ttl))
}

// markModelCooldownUntil records (or re-arms) a hold with an explicit reset
// instant (daily-quota tier). Shared never-shorten semantics with the TTL
// variant above.
func markModelCooldownUntil(authID, model string, reason cooldownReason, until time.Time) {
	authID = strings.TrimSpace(authID)
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" || !until.After(cooldownNowFn()) {
		return
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	cooldownSweepLocked(now)
	if len(cooldownTable) >= modelCooldownMaxEntries {
		// Table full: evict the entry closest to natural expiry.
		var oldestKey modelCooldownKey
		oldestUntil := time.Time{}
		first := true
		for k, v := range cooldownTable {
			if first || v.Until.Before(oldestUntil) {
				oldestKey, oldestUntil, first = k, v.Until, false
			}
		}
		delete(cooldownTable, oldestKey)
	}
	// Never shorten an active window: an earlier, larger hold stays.
	if existing, ok := cooldownTable[modelCooldownKey{AuthID: authID, ModelID: model}]; ok && existing.Until.After(until) {
		cooldownMu.Unlock()
		return
	}
	cooldownTable[modelCooldownKey{AuthID: authID, ModelID: model}] = modelCooldownEntry{
		Reason:    reason,
		Until:     until,
		UpdatedAt: now,
	}
	cooldownMu.Unlock()
}

// modelCoolingUntil reports when the pair becomes usable again (zero time =
// not cooling). Lazy expiry on read.
func modelCoolingUntil(authID, model string) time.Time {
	authID = strings.TrimSpace(authID)
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" {
		return time.Time{}
	}
	now := cooldownNowFn()
	key := modelCooldownKey{AuthID: authID, ModelID: model}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[key]
	if !ok {
		return time.Time{}
	}
	if !entry.Until.After(now) {
		delete(cooldownTable, key)
		return time.Time{}
	}
	return entry.Until
}

// modelIsCooling reports whether the pair is cooling right now.
func modelIsCooling(authID, model string) bool {
	return !modelCoolingUntil(authID, model).IsZero()
}

// clearModelCooldown removes one pair, or every pair for the account when
// model is empty. Returns how many entries were removed.
func clearModelCooldown(authID, model string) int {
	authID = strings.TrimSpace(authID)
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" {
		return 0
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	removed := 0
	if model != "" {
		key := modelCooldownKey{AuthID: authID, ModelID: model}
		if _, ok := cooldownTable[key]; ok {
			delete(cooldownTable, key)
			removed++
		}
		return removed
	}
	for k := range cooldownTable {
		if k.AuthID == authID {
			delete(cooldownTable, k)
			removed++
		}
	}
	return removed
}

// sortCooldownViewRows orders listing rows deterministically: account, then
// model, then reason — a pair can legitimately sit in both tables at once
// (a short cooldown under a longer 6004 window, or the reverse), and the
// merged listing needs one order. withAuth=false is the per-account view.
func sortCooldownViewRows(rows []map[string]any, withAuth bool) {
	str := func(row map[string]any, key string) string { s, _ := row[key].(string); return s }
	sort.Slice(rows, func(i, j int) bool {
		if withAuth {
			if a, b := str(rows[i], "auth_id"), str(rows[j], "auth_id"); a != b {
				return a < b
			}
		}
		if a, b := str(rows[i], "model"), str(rows[j], "model"); a != b {
			return a < b
		}
		return str(rows[i], "reason") < str(rows[j], "reason")
	})
}

// cooldownSnapshotFor lists the still-active pairs for one account.
func cooldownSnapshotFor(authID string) []map[string]any {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if k.AuthID != authID || !v.Until.After(now) {
			continue
		}
		out = append(out, cooldownEntryView(k, v, now, false))
	}
	cooldownMu.Unlock()
	sortCooldownViewRows(out, false)
	return out
}

// cooldownSnapshotAll lists every active pair across accounts.
func cooldownSnapshotAll() []map[string]any {
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			continue
		}
		out = append(out, cooldownEntryView(k, v, now, true))
	}
	cooldownMu.Unlock()
	sortCooldownViewRows(out, true)
	return out
}

func cooldownEntryView(k modelCooldownKey, v modelCooldownEntry, now time.Time, withAuth bool) map[string]any {
	row := map[string]any{
		"model":      k.ModelID,
		"reason":     string(v.Reason),
		"until":      v.Until.UTC().Format(time.RFC3339),
		"seconds":    int(v.Until.Sub(now).Round(time.Second) / time.Second),
		"updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if withAuth {
		row["auth_id"] = k.AuthID
	}
	return row
}

// cooldownSweepLocked drops expired entries. Callers must hold cooldownMu.
func cooldownSweepLocked(now time.Time) {
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			delete(cooldownTable, k)
		}
	}
}

// recordUpstreamFailure routes one upstream failure into the model-scoped
// cooldown table. authID is the host auth id when known ("" = derive from
// sa); model is the upstream model id the request actually used.
func recordUpstreamFailure(authID string, sa *storedAuth, model string, status int, body string) {
	if authID == "" {
		authID = cooldownAuthIDFor(sa)
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" {
		return
	}
	// 6004 model-scoped rate limit: model_ratelimit.go owns it with the
	// upstream-declared reset; a second, blunter table entry would only
	// fight the precise one.
	if isModelScopedRateLimit(status, body) {
		return
	}
	// Hard credit exhaustion is account-level: the lifecycle/reconcile and
	// statusError envelope paths own it — cooling only the model would
	// UNDER-react (other models on a dead account must not keep firing).
	if isHardCreditError(status, body) {
		return
	}
	if status == http.StatusTooManyRequests || isSoftRateLimit(status, body) {
		markModelCooldown(authID, model, cooldownReasonRateLimit)
		// #59: the account-level 429 family is also the opt-in quota-lane
		// switch trigger — sticky, persisted, CN-realm only, capped per day.
		// 6004 (model-scoped) returned early above and never reaches here; a
		// model-scoped limit must not flip an account-scoped lane.
		maybeSwitchIdentityFamily(authID, sa)
		return
	}
	// Daily-allowance wording (issue #35 supplementary tier, v0.9.58): the
	// natural reset is the next UTC+8 midnight. A declared reset instant in
	// the payload ("将在 … 重置") wins when parsable. v0.9.59: the parse is
	// anchored to the table clock (cooldownNowFn), the SAME clock the
	// midnight fallback below uses — the real-clock entry point
	// parseRateLimitResetAt made the branch internally inconsistent under
	// a frozen table and the v0.9.58 test suite time-brittle (the frozen
	// dates passed their own horizon on 2026-10-13 and every day after).
	if isDailyQuotaFrame(body) {
		until, ok := parseRateLimitResetAtFrom(body, cooldownNowFn())
		if !ok {
			until = nextDailyResetUTC8(cooldownNowFn())
		}
		markModelCooldownUntil(authID, model, cooldownReasonDailyQuota, until)
		return
	}
	// Empty streams and detected in-stream error frames (the 200-refused
	// family, issue #35's exact shape) rotate the pair briefly. The body is
	// the reliable discriminator — status may be 0 on both paths.
	if isEmptyStreamFailure(body) || isInStreamErrorFailure(body) {
		markModelCooldown(authID, model, cooldownReasonUnknown)
	}
}

// isEmptyStreamFailure matches the emptyStreamError wording (also carried by
// the sync collector's error return).
func isEmptyStreamFailure(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(body, "empty_stream") ||
		strings.Contains(body, "stream closed before first payload")
}

// isInStreamErrorFailure matches the workBuddyStreamFrame error family — the
// upstream answered 200 and then refused in-band. This is the honest
// catch-all for quota wordings nobody has captured yet (issue #35's open
// question): rotate the pair for the short unknown window.
func isInStreamErrorFailure(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(body, "workbuddy upstream error")
}

// dailyQuotaMarkers / dailyQuotaDayWords / dailyQuotaLimitWords（issue #35
// 补充分级，v0.9.58）：免费 0x 模型每日限额的文案词族。帧形状至今无人
// 捕获（报告者旧版无法提供、凭据轮换无法实测），所以设计为信封无关的
// 保守匹配：组合词直接命中；拆分词族要求（每日/今日/daily 类）与
// （上限/限额/exceed/limit 类）同时命中才算，避免误伤普通频率限制。
// 6004 已在 recordUpstreamFailure 先判排除——它的重置措辞不是每日语义。
var dailyQuotaMarkers = []string{
	"daily limit",
	"daily quota",
	"daily usage",
	"daily allowance",
	"exceeded the daily",
	"per-day limit",
	"每日上限",
	"每日限额",
	"每日限制",
	"每日用量",
	"每日额度",
	"今日上限",
	"今日限额",
	"今日额度",
}

var (
	dailyQuotaDayWords   = []string{"daily", "每日", "今日", "当天"}
	dailyQuotaLimitWords = []string{"limit", "quota", "exceed", "allowance", "上限", "限额", "限制", "超出", "超限", "额度", "用量"}
)

// isDailyQuotaFrame reports whether a failure body carries daily-allowance
// semantics. 6004-shaped payloads are excluded (their own path owns those).
func isDailyQuotaFrame(payload string) bool {
	if strings.TrimSpace(payload) == "" {
		return false
	}
	if isModelScopedRateLimit(http.StatusTooManyRequests, payload) {
		return false
	}
	low := strings.ToLower(payload)
	if containsAny(low, dailyQuotaMarkers...) {
		return true
	}
	day, limit := false, false
	for _, w := range dailyQuotaDayWords {
		if strings.Contains(low, w) {
			day = true
			break
		}
	}
	for _, w := range dailyQuotaLimitWords {
		if strings.Contains(low, w) {
			limit = true
			break
		}
	}
	return day && limit
}

// wbDailyZone is the fixed UTC+8 offset the upstream reset copy carries.
var wbDailyZone = time.FixedZone("UTC+8", 8*60*60)

// nextDailyResetUTC8 returns the next UTC+8 midnight — the natural reset
// point of a daily allowance. Result is at most 24h out, so the hold can
// never pin a pair beyond the day it was earned on.
func nextDailyResetUTC8(now time.Time) time.Time {
	cur := now.In(wbDailyZone)
	midnight := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, wbDailyZone)
	if !midnight.After(now) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight
}

// noteModelCooldownFailure is the failure-site hook used by the stream paths
// (pump, collector tail, executor branches). authID may be "" when only the
// stored auth is in scope; the resolver inside falls back to it.
func noteModelCooldownFailure(authID string, sa *storedAuth, model string, status int, body string) {
	recordUpstreamFailure(authID, sa, model, status, body)
}

// modelCooldownError renders the fast-fail error for a pair with an active
// window, or nil. Deliberately plain (no StatusCode() impl): every host
// version must treat it as a transient request-level failure, never as
// credential-level quota — the pair cools, the credential stays healthy.
func modelCooldownError(authID string, sa *storedAuth, model string) error {
	if authID == "" {
		authID = cooldownAuthIDFor(sa)
	}
	until := modelCoolingUntil(authID, model)
	if until.IsZero() {
		return nil
	}
	remaining := int(until.Sub(cooldownNowFn()).Round(time.Second) / time.Second)
	if remaining < 1 {
		remaining = 1
	}
	reason, _ := cooldownReasonOf(authID, model)
	return &cooldownFastFailError{authID: authID, model: strings.TrimSpace(model), seconds: remaining, until: until, reason: reason}
}

// cooldownFastFailError carries the fields for its message; kept as a type
// so tests can assert on the shape without string-matching the whole copy.
type cooldownFastFailError struct {
	authID  string
	model   string
	seconds int
	until   time.Time
	reason  cooldownReason
}

func (e *cooldownFastFailError) Error() string {
	if e.reason == cooldownReasonDailyQuota {
		reset := e.until.In(wbDailyZone).Format("2006-01-02 15:04:05 UTC+8")
		return "该模型今日免费额度已用完（上游每日限额）——仅此 (账号, 模型) 组合暂停，同账号其他模型与池内其他账号不受影响，将于 " + reset +
			" 重置；期间调度器会自动改用其他候选，可切换其他模型或凭证继续。" +
			" // This model hit the upstream daily allowance for today — only this (credential, model) pair is paused, other models and credentials are unaffected, resets at " + reset +
			"; the scheduler rotates automatically meanwhile."
	}
	return "该 (账号, 模型) 对处于上游失败冷却中——仅此组合暂停约 " + strconv.Itoa(e.seconds) +
		" 秒（至 " + e.until.UTC().Format(time.RFC3339) + "），同账号其他模型与池内其他账号不受影响；期间调度器会自动改用其他候选，无需更换凭证。" +
		" // This (credential, model) pair is cooling after an upstream failure — paused for ~" + strconv.Itoa(e.seconds) +
		"s, other models on the account and other credentials in the pool are unaffected; the scheduler rotates automatically meanwhile."
}

// cooldownReasonOf returns the active entry's reason for a pair (empty
// string when not cooling) — the fast-fail copy is reason-aware.
func cooldownReasonOf(authID, model string) (cooldownReason, bool) {
	authID = strings.TrimSpace(authID)
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" {
		return "", false
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[modelCooldownKey{AuthID: authID, ModelID: model}]
	if !ok || !entry.Until.After(cooldownNowFn()) {
		return "", false
	}
	return entry.Reason, true
}

// filterCoolingModels removes models currently cooling for THIS credential
// so the host's built-in scheduler is never offered a degraded (auth, model)
// pair. A credential with no active cooldowns returns the list untouched.
func filterCoolingModels(authID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(models) == 0 || len(cooldownSnapshotFor(authID)) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if modelIsCooling(authID, m.ID) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// handleCooldownList reports every active pair, or one account's pairs when
// auth_id is given. Since v0.9.88 (issue #77) the 6004 model-rate-limit
// windows ride along in the same row shape (reason "model_rate_limit").
// Both tables live in memory, so the response says so.
func handleCooldownList(req pluginapi.ManagementRequest) map[string]any {
	if vals := req.Query["auth_id"]; len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
		authID := strings.TrimSpace(vals[0])
		entries := cooldownSnapshotFor(authID)
		entries = append(entries, modelRateLimitSnapshotFor(authID)...)
		sortCooldownViewRows(entries, false)
		return map[string]any{
			"entries":    entries,
			"count":      len(entries),
			"persistent": false,
		}
	}
	entries := cooldownSnapshotAll()
	entries = append(entries, modelRateLimitSnapshotAll()...)
	sortCooldownViewRows(entries, true)
	return map[string]any{
		"entries":    entries,
		"count":      len(entries),
		"persistent": false,
	}
}

// handleCooldownClear clears one pair (auth_id + model) or the whole account
// (auth_id only) from BOTH tables — the #35 cooldown holds and the 6004
// model-rate-limit windows (issue #77). Panel action: "give this model
// another chance now".
func handleCooldownClear(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthID string `json:"auth_id"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		return map[string]any{"error": "auth_id required"}
	}
	removed := clearModelCooldown(authID, body.Model)
	removed += clearModelRateLimit(authID, body.Model)
	return map[string]any{"success": true, "removed": removed}
}

// dailyQuotaLimitCopy renders the bilingual daily-allowance copy for a raw
// payload: a declared reset instant ("将在 … UTC+8 重置") wins, otherwise the
// next UTC+8 midnight. Structurally identical to the 6004 copy but with
// explicit "今日免费额度" semantics; like it, the text never carries the
// literal "429" — hosts without the envelope status would re-read it from
// the message alone as credential quota (issue #35).
func dailyQuotaLimitCopy(payload string) string {
	resetAt, ok := parseRateLimitResetAt(payload)
	if !ok {
		resetAt = nextDailyResetUTC8(time.Now())
	}
	return "该模型今日免费额度已用完（上游每日限额）——仅此模型受限，同一凭证其他模型不受影响，将于 " +
		resetAt.In(wbDailyZone).Format("2006-01-02 15:04:05 UTC+8") +
		" 重置；可切换其他模型或凭证继续。" +
		" // This model hit the upstream daily allowance for today — other models on the same credential keep working, resets at " +
		resetAt.In(wbDailyZone).Format("2006-01-02 15:04:05 UTC+8") +
		"; switch model or credential meanwhile."
}
