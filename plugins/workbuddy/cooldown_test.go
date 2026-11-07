package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetCooldownTable empties the global table between tests.
func resetCooldownTable() {
	cooldownMu.Lock()
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownMu.Unlock()
}

// withFrozenCooldown freezes the table clock for one test and restores both
// clock and table afterwards.
func withFrozenCooldown(t *testing.T, now time.Time) {
	t.Helper()
	resetCooldownTable()
	orig := cooldownNowFn
	cooldownNowFn = func() time.Time { return now }
	t.Cleanup(func() {
		cooldownNowFn = orig
		resetCooldownTable()
	})
}

// withFrozenHoldTables freezes BOTH per-(credential, model) hold tables
// for tests that exercise the merged management surface (issue #77):
// the #35 cooldown table and the 6004 model-rate-limit registry.
func withFrozenHoldTables(t *testing.T, now time.Time) {
	t.Helper()
	withFrozenCooldown(t, now)
	withFrozenModelRateLimits(t, now)
}

// TestRecordUpstreamFailureMatrix locks the issue #35 classification:
//
//	6004               -> NOT tabled (model_ratelimit.go owns it)
//	hard credit        -> NOT tabled (account-level lifecycle owns it)
//	429 / soft limit   -> rate_limit hold (300 s)
//	empty stream       -> unknown hold (60 s)
//	in-stream frame    -> unknown hold (60 s)  <- the daily-quota shape
//	transport wording  -> NOT tabled (qoder parity)
//	unknown body       -> NOT tabled
func TestRecordUpstreamFailureMatrix(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantHold bool
		want     cooldownReason
	}{
		{"6004 defers to own registry", http.StatusTooManyRequests, modelRateLimitSamplePayload, false, ""},
		{"hard credit stays account-level", 0, "积分不足，请充值", false, ""},
		{"429 status", http.StatusTooManyRequests, `{"code":11120,"msg":"slow down"}`, true, cooldownReasonRateLimit},
		{"soft limit wording", 0, "upstream rate limit exceeded", true, cooldownReasonRateLimit},
		{"empty stream", 0, emptyStreamError().Error(), true, cooldownReasonUnknown},
		{"in-stream error frame", 0, "workbuddy upstream error: {\"code\":50001,\"msg\":\"service temporarily unavailable\"}", true, cooldownReasonUnknown},
		{"bare error event frame", 0, "workbuddy upstream error event", true, cooldownReasonUnknown},
		{"daily quota wording (en)", 0, "workbuddy upstream error: {\"code\":50001,\"msg\":\"daily quota reached\"}", true, cooldownReasonDailyQuota},
		{"daily quota wording (zh)", 0, "workbuddy upstream error: {\"code\":50001,\"msg\":\"今日免费模型请求次数已达上限\"}", true, cooldownReasonDailyQuota},
		{"read error wording", 0, "upstream stream read error (unexpected EOF): connection reset", false, ""},
		{"unknown body", 0, "something else entirely", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFrozenCooldown(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
			recordUpstreamFailure("acct-m", nil, "glm-5.3-flash", tc.status, tc.body)
			if !tc.wantHold {
				if modelIsCooling("acct-m", "glm-5.3-flash") {
					t.Fatalf("must not cool: %q", tc.body)
				}
				return
			}
			until := modelCoolingUntil("acct-m", "glm-5.3-flash")
			if until.IsZero() {
				t.Fatalf("expected a hold for %q", tc.body)
			}
			wantTTL := modelCooldownUnknown
			if tc.want == cooldownReasonRateLimit {
				wantTTL = modelCooldownRateLimit
			}
			cooldownMu.Lock()
			entry := cooldownTable[modelCooldownKey{AuthID: "acct-m", ModelID: "glm-5.3-flash"}]
			cooldownMu.Unlock()
			if entry.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", entry.Reason, tc.want)
			}
			if tc.want == cooldownReasonDailyQuota {
				// 冻结时刻 2026-10-12 12:00 UTC = 20:00 UTC+8 → 午夜重置在
				// 4 小时后（未携带可解析重置声明的回退点）。
				wantUntil := time.Date(2026, 10, 13, 0, 0, 0, 0, wbDailyZone)
				if !entry.Until.Equal(wantUntil) {
					t.Fatalf("daily until = %v, want %v", entry.Until, wantUntil)
				}
				return
			}
			if got := entry.Until.Sub(cooldownNowFn()); got != wantTTL {
				t.Fatalf("ttl = %v, want %v", got, wantTTL)
			}
		})
	}
}

// TestRecordUpstreamFailureKeyDerivation: an empty authID derives the host
// auth-id convention (sanitized UID, provider fallback) from the stored auth.
func TestRecordUpstreamFailureKeyDerivation(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	sa := &storedAuth{Account: storedAccount{UID: "user/one@tencent"}}
	// sanitizeUIDForFileName folds '/' away.
	recordUpstreamFailure("", sa, "M", 0, emptyStreamError().Error())
	if !modelIsCooling("user_one_tencent", "m") {
		t.Fatal("derived key (sanitized uid, lowercased model) must hold")
	}
	// UID-less auth falls back to the provider name.
	recordUpstreamFailure("", &storedAuth{}, "M2", 0, emptyStreamError().Error())
	if !modelIsCooling(providerName, "m2") {
		t.Fatal("provider-name fallback must hold")
	}
}

// TestModelCooldownNeverShortens: a later, smaller hold cannot release an
// earlier, larger one.
func TestModelCooldownNeverShortens(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	withFrozenCooldown(t, now)
	markModelCooldown("a", "m", cooldownReasonRateLimit) // 300 s
	now2 := now.Add(200 * time.Second)
	cooldownNowFn = func() time.Time { return now2 }
	markModelCooldown("a", "m", cooldownReasonUnknown) // would end at +60 s < existing
	until := modelCoolingUntil("a", "m")
	if !until.Equal(now.Add(modelCooldownRateLimit)) {
		t.Fatalf("window was shortened: until=%v want=%v", until, now.Add(modelCooldownRateLimit))
	}
}

// TestModelCooldownLazyExpiry: reading an expired entry drops it.
func TestModelCooldownLazyExpiry(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	withFrozenCooldown(t, now)
	markModelCooldown("a", "m", cooldownReasonUnknown) // 60 s
	cooldownNowFn = func() time.Time { return now.Add(61 * time.Second) }
	if modelIsCooling("a", "m") {
		t.Fatal("expired entry must not report cooling")
	}
	cooldownMu.Lock()
	_, present := cooldownTable[modelCooldownKey{AuthID: "a", ModelID: "m"}]
	cooldownMu.Unlock()
	if present {
		t.Fatal("expired entry must be dropped on read")
	}
}

// TestClearModelCooldown: one pair, then the whole account.
func TestClearModelCooldown(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	markModelCooldown("a", "m1", cooldownReasonUnknown)
	markModelCooldown("a", "m2", cooldownReasonUnknown)
	markModelCooldown("b", "m1", cooldownReasonUnknown)
	if clearModelCooldown("a", "m1") != 1 {
		t.Fatal("pair clear must remove exactly one")
	}
	if modelIsCooling("a", "m1") || !modelIsCooling("a", "m2") || !modelIsCooling("b", "m1") {
		t.Fatal("clear must be pair-scoped")
	}
	if clearModelCooldown("a", "") != 1 {
		t.Fatal("account clear must remove the rest")
	}
	if clearModelCooldown("nope", "") != 0 {
		t.Fatal("unknown account clears nothing")
	}
}

// TestCooldownSnapshotViews: list-all includes auth_id, list-one does not,
// both are sorted and carry reason/until/seconds.
func TestCooldownSnapshotViews(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	markModelCooldown("zz", "b-model", cooldownReasonRateLimit)
	markModelCooldown("aa", "a-model", cooldownReasonUnknown)
	all := cooldownSnapshotAll()
	if len(all) != 2 {
		t.Fatalf("all: got %d", len(all))
	}
	if all[0]["auth_id"] != "aa" || all[1]["auth_id"] != "zz" {
		t.Fatalf("all must sort by auth then model: %#v", all)
	}
	one := cooldownSnapshotFor("zz")
	if len(one) != 1 || one[0]["model"] != "b-model" {
		t.Fatalf("one: %#v", one)
	}
	if _, has := one[0]["auth_id"]; has {
		t.Fatal("per-account view must not carry auth_id")
	}
	if one[0]["reason"] != string(cooldownReasonRateLimit) {
		t.Fatalf("reason: %#v", one[0])
	}
	if secs, _ := one[0]["seconds"].(int); secs != int(modelCooldownRateLimit/time.Second) {
		t.Fatalf("seconds: %#v", one[0])
	}
	if cooldownSnapshotFor("") != nil {
		t.Fatal("empty auth id must return nil")
	}
}

// TestFilterCoolingModels: only THIS credential's cooling pairs drop.
func TestFilterCoolingModels(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	markModelCooldown("acct-1", "deepseek-v4.1-flash", cooldownReasonUnknown)
	in := []pluginapi.ModelInfo{
		{ID: "glm-5.3-flash"},
		{ID: "deepseek-v4.1-flash"},
		{ID: "DeepSeek-V4.1-Flash"}, // case-insensitive key match
	}
	out := filterCoolingModels("acct-1", in)
	if len(out) != 1 || out[0].ID != "glm-5.3-flash" {
		t.Fatalf("cooling pairs (case-insensitive) must drop: %#v", out)
	}
	// Another credential keeps everything; empty auth id is a no-op.
	if got := filterCoolingModels("acct-2", in); len(got) != 3 {
		t.Fatalf("other credential untouched: %#v", got)
	}
	if got := filterCoolingModels("", in); len(got) != 3 {
		t.Fatalf("empty auth id must not filter: %#v", got)
	}
}

// TestHandleModelForAuthFiltersCoolingPairs drives the full model.for_auth
// path: pinned realm models + a cooling (auth, model) pair -> the host never
// sees the degraded pair; an empty AuthID (older hosts) degrades to no-op.
func TestHandleModelForAuthFiltersCoolingPairs(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	resetPinnedModels()
	resetDynamicModelsCache()
	defer func() { resetPinnedModels(); resetDynamicModelsCache() }()
	pinnedModelsMu.Lock()
	pinnedModels["cn"] = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}
	pinnedModelsMu.Unlock()

	markModelCooldown("acct-9", "deepseek-v4.1-flash", cooldownReasonUnknown)

	storage := []byte(`{"auth":{"domain":"copilot.tencent.com","accessToken":"tok"},"account":{"uid":"acct-9"}}`)
	raw, err := handleModelForAuth(mustMarshal(t, pluginapi.AuthModelRequest{
		AuthID:       "acct-9",
		AuthProvider: providerName,
		StorageJSON:  storage,
	}))
	if err != nil {
		t.Fatalf("handleModelForAuth: %v", err)
	}
	var env struct {
		OK     bool                    `json:"ok"`
		Result pluginapi.ModelResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %v raw=%s", err, raw)
	}
	ids := modelIDsOf(env.Result.Models)
	if len(ids) != 1 || ids[0] != "glm-5.3-flash" {
		t.Fatalf("cooling pair must be hidden from the catalog: %v", ids)
	}

	// Empty AuthID (hosts that do not populate it) — filter is a no-op.
	raw2, err := handleModelForAuth(mustMarshal(t, pluginapi.AuthModelRequest{
		AuthProvider: providerName,
		StorageJSON:  storage,
	}))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	var env2 struct {
		OK     bool                    `json:"ok"`
		Result pluginapi.ModelResponse `json:"result"`
	}
	if err := json.Unmarshal(raw2, &env2); err != nil || !env2.OK {
		t.Fatalf("envelope2: %v raw=%s", err, raw2)
	}
	if ids := modelIDsOf(env2.Result.Models); len(ids) != 2 {
		t.Fatalf("empty auth_id must not filter: %v", ids)
	}
}

// TestModelCooldownErrorFastFail: the executor fast-fail renders an honest,
// plain (status-less) error; the 6004 fast-fail contract is mirrored.
func TestModelCooldownErrorFastFail(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	if err := modelCooldownError("acct-1", nil, "glm-5.3-flash"); err != nil {
		t.Fatalf("no hold yet, got %v", err)
	}
	markModelCooldown("acct-1", "glm-5.3-flash", cooldownReasonUnknown)
	err := modelCooldownError("acct-1", nil, "glm-5.3-flash")
	if err == nil {
		t.Fatal("active hold must fast-fail")
	}
	if strings.Contains(err.Error(), "429") {
		t.Fatalf("copy must never carry 429 (host re-reads messages): %v", err)
	}
	// Other pairs and credentials are untouched.
	if modelCooldownError("acct-1", nil, "kimi-k3") != nil {
		t.Fatal("other model on the same account must pass")
	}
	if modelCooldownError("acct-2", nil, "glm-5.3-flash") != nil {
		t.Fatal("other credential must pass")
	}
	// Empty authID derives from sa.
	sa := &storedAuth{Account: storedAccount{UID: "acct-1"}}
	if modelCooldownError("", sa, "glm-5.3-flash") == nil {
		t.Fatal("derived key must fast-fail")
	}
}

// TestSchedulerPickSkipsCoolingPair: the opt-in picker agrees with the
// catalog filter — a candidate whose (id, model) pair is cooling is skipped.
func TestSchedulerPickSkipsCoolingPair(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	resetActiveAuth(t)
	setActiveAuthID("healthy")
	markModelCooldown("cooled", "glm-5.3-flash", cooldownReasonUnknown)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "glm-5.3-flash",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "cooled", Provider: providerName, Status: "active"},
			{ID: "healthy", Provider: providerName, Status: "active"},
		},
	}))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "healthy" {
		t.Fatalf("cooling candidate must be skipped, got handled=%v auth=%q", resp.Handled, resp.AuthID)
	}

	// All candidates cooling → defer to the host.
	setActiveAuthID("cooled")
	markModelCooldown("healthy", "glm-5.3-flash", cooldownReasonUnknown)
	raw2, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "glm-5.3-flash",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "cooled", Provider: providerName, Status: "active"},
			{ID: "healthy", Provider: providerName, Status: "active"},
		},
	}))
	if err != nil {
		t.Fatalf("pick2: %v", err)
	}
	if resp2 := parsePickResponse(t, raw2); resp2.Handled {
		t.Fatal("all candidates cooling → defer to host")
	}
}

// TestCooldownManagementEndpoints: list / list-one / clear round-trip.
func TestCooldownManagementEndpoints(t *testing.T) {
	withFrozenHoldTables(t, time.Now())
	markModelCooldown("acct-a", "m1", cooldownReasonRateLimit)
	markModelCooldown("acct-b", "m2", cooldownReasonUnknown)

	all := handleCooldownList(pluginapi.ManagementRequest{Query: map[string][]string{}})
	if all["count"].(int) != 2 {
		t.Fatalf("list all: %#v", all)
	}
	one := handleCooldownList(pluginapi.ManagementRequest{Query: map[string][]string{"auth_id": {"acct-a"}}})
	if one["count"].(int) != 1 {
		t.Fatalf("list one: %#v", one)
	}
	// Clear without auth_id errors.
	if _, has := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{}`)})["error"]; !has {
		t.Fatal("clear without auth_id must error")
	}
	res := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{"auth_id":"acct-a","model":"m1"}`)})
	if res["removed"].(int) != 1 {
		t.Fatalf("clear pair: %#v", res)
	}
	res2 := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{"auth_id":"acct-b"}`)})
	if res2["removed"].(int) != 1 {
		t.Fatalf("clear account: %#v", res2)
	}
	if len(cooldownSnapshotAll()) != 0 {
		t.Fatal("table must be empty after clears")
	}
}

func modelIDsOf(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// TestCooldownListMergesRateLimitWindows: one operator surface, both tables
// (issue #77) — 6004 windows ride along in the same row shape and the merged
// order is deterministic.
func TestCooldownListMergesRateLimitWindows(t *testing.T) {
	now := time.Now()
	withFrozenHoldTables(t, now)

	markModelCooldownUntil("acct-a", "m1", cooldownReasonRateLimit, now.Add(300*time.Second))
	markModelCooldownUntil("acct-a", "m2", cooldownReasonDailyQuota, now.Add(time.Hour))
	noteModelRateLimit("acct-a", "m3", now.Add(40*time.Minute))
	noteModelRateLimit("acct-b", "m1", now.Add(5*time.Minute))

	all := handleCooldownList(pluginapi.ManagementRequest{Query: map[string][]string{}})
	if all["count"].(int) != 4 {
		t.Fatalf("merged list = %#v", all)
	}
	entries := all["entries"].([]map[string]any)
	wantOrder := []struct{ auth, model, reason string }{
		{"acct-a", "m1", "rate_limit"},
		{"acct-a", "m2", "daily_quota"},
		{"acct-a", "m3", modelRateLimitViewReason},
		{"acct-b", "m1", modelRateLimitViewReason},
	}
	for i, w := range wantOrder {
		if entries[i]["auth_id"] != w.auth || entries[i]["model"] != w.model || entries[i]["reason"] != w.reason {
			t.Fatalf("row %d = %#v, want %v", i, entries[i], w)
		}
	}

	// The per-account view merges too, and its rows carry no auth_id.
	one := handleCooldownList(pluginapi.ManagementRequest{Query: map[string][]string{"auth_id": {"acct-a"}}})
	if one["count"].(int) != 3 {
		t.Fatalf("merged per-account list = %#v", one)
	}
	for _, row := range one["entries"].([]map[string]any) {
		if _, has := row["auth_id"]; has {
			t.Fatal("per-account rows must not carry auth_id")
		}
	}
}

// TestCooldownClearRemovesBothTables: the panel's "give this pair another
// chance now" must lift both holds — a pair can sit in both tables at once
// (a short cooldown under a longer 6004 window).
func TestCooldownClearRemovesBothTables(t *testing.T) {
	now := time.Now()
	withFrozenHoldTables(t, now)

	markModelCooldownUntil("acct-a", "m1", cooldownReasonUnknown, now.Add(60*time.Second))
	noteModelRateLimit("acct-a", "m1", now.Add(40*time.Minute))
	markModelCooldownUntil("acct-b", "m2", cooldownReasonRateLimit, now.Add(300*time.Second))

	// Pair clear removes both holds and reports the total.
	res := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{"auth_id":"acct-a","model":"m1"}`)})
	if res["removed"].(int) != 2 {
		t.Fatalf("pair clear = %#v", res)
	}
	if len(cooldownSnapshotFor("acct-a")) != 0 || len(modelRateLimitSnapshotFor("acct-a")) != 0 {
		t.Fatal("acct-a must be fully released")
	}

	// Account clear sweeps whatever is left for the account.
	res2 := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{"auth_id":"acct-b"}`)})
	if res2["removed"].(int) != 1 {
		t.Fatalf("account clear = %#v", res2)
	}
	final := handleCooldownList(pluginapi.ManagementRequest{Query: map[string][]string{}})
	if final["count"].(int) != 0 {
		t.Fatalf("both tables must be empty after clears: %#v", final)
	}
}

// TestCooldownRoutesRegistered: the endpoints are advertised to the host the
// same way qoder's are (issue #77) — otherwise operators can't discover them.
func TestCooldownRoutesRegistered(t *testing.T) {
	reg := managementRegistration()
	base := "/plugins/" + providerName
	var get, post bool
	for _, r := range reg.Routes {
		if r.Path == base+"/cooldowns" && r.Method == http.MethodGet {
			get = true
		}
		if r.Path == base+"/cooldowns/clear" && r.Method == http.MethodPost {
			post = true
		}
	}
	if !get || !post {
		t.Fatalf("cooldowns routes not registered: get=%v post=%v", get, post)
	}
}
