package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The exact upstream shape observed on 2026-09-22 (issue round): the gateway
// itself declares the limit model-scoped by advising "switch to another model".
const modelRateLimitSamplePayload = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"055d655c-f97b-4d00-aba8-9d55a4e588f6"}`

// resetModelRateLimits empties the global registry between tests.
func resetModelRateLimits() {
	wbModelRateLimits.mu.Lock()
	wbModelRateLimits.entries = make(map[string]modelRateLimitEntry)
	wbModelRateLimits.mu.Unlock()
}

// withFrozenModelRateLimits freezes the registry clock for one test and
// restores both clock and registry afterwards.
func withFrozenModelRateLimits(t *testing.T, now time.Time) {
	t.Helper()
	resetModelRateLimits()
	orig := modelRateLimitNowFn
	modelRateLimitNowFn = func() time.Time { return now }
	t.Cleanup(func() {
		modelRateLimitNowFn = orig
		resetModelRateLimits()
	})
}

func TestIsModelScopedRateLimit(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
		want    bool
	}{
		{"canonical 6004", http.StatusTooManyRequests, modelRateLimitSamplePayload, true},
		{"json envelope 6004", http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, true},
		{"string code variant", http.StatusTooManyRequests, `gateway said "code":"6004" somewhere`, true},
		{"spaced variant", http.StatusTooManyRequests, `{ "code" : 6004 }`, true},
		{"other 429 code", http.StatusTooManyRequests, `{"code":11120,"msg":"quota exceeded"}`, false},
		{"6004 on 400", http.StatusBadRequest, `{"code":6004,"msg":"x"}`, false},
		{"6004 on 200", http.StatusOK, `{"code":6004,"msg":"x"}`, false},
		{"empty body", http.StatusTooManyRequests, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelScopedRateLimit(tc.status, tc.payload); got != tc.want {
				t.Fatalf("isModelScopedRateLimit(%d, %q) = %v, want %v", tc.status, tc.payload, got, tc.want)
			}
		})
	}
}

func TestParseRateLimitResetAtFrom(t *testing.T) {
	// now sits safely before the sample reset (09:46:39 UTC+8 on 2026-09-22).
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	reset, ok := parseRateLimitResetAtFrom(modelRateLimitSamplePayload, now)
	if !ok {
		t.Fatalf("canonical sample must parse, payload=%s", modelRateLimitSamplePayload)
	}
	want := time.Date(2026, 9, 22, 9, 46, 39, 0, wbCNZone)
	if !reset.Equal(want) {
		t.Fatalf("reset = %v, want %v", reset, want)
	}

	// Past reset → treated as absent (caller falls back to the default window).
	past, ok := parseRateLimitResetAtFrom(modelRateLimitSamplePayload, want.Add(time.Minute))
	if ok || !past.IsZero() {
		t.Fatalf("past reset must be absent, got %v ok=%v", past, ok)
	}

	// Beyond the horizon cap → absent.
	far, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"将在 2027-09-22 09:46:39 UTC+8 重置"}`, now)
	if ok || !far.IsZero() {
		t.Fatalf("beyond-horizon reset must be absent, got %v ok=%v", far, ok)
	}

	// Loose fallback requires the 重置 marker.
	loose, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"limit hit，窗口将于 2026-09-22 10:00:00 UTC+8 重置。"}`, now)
	if !ok || loose.IsZero() {
		t.Fatalf("loose fallback with 重置-free copy must not parse; got %v ok=%v", loose, ok)
	}

	// Unrelated future timestamps must NOT pin a model (no 重置 marker).
	junk, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"trial ends 2026-10-01 00:00:00"}`, now)
	if ok || !junk.IsZero() {
		t.Fatalf("timestamp without reset marker must be ignored, got %v ok=%v", junk, ok)
	}

	// Empty payload.
	if _, ok := parseRateLimitResetAtFrom("", now); ok {
		t.Fatalf("empty payload must not parse")
	}
}

func TestModelRateLimitRegistryLifecycle(t *testing.T) {
	uid, model := "uid-registry-lifecycle", "deepseek-v4.1-flash"
	key := modelRateLimitKey(uid, model)

	// Miss before any note.
	if _, ok := wbModelRateLimits.until(key, time.Now()); ok {
		t.Fatalf("fresh key must miss")
	}

	// Note a future reset → hit with the same instant.
	future := time.Now().Add(30 * time.Minute)
	noteModelRateLimit(uid, model, future)
	got, ok := wbModelRateLimits.until(key, time.Now())
	if !ok || !got.Equal(future) {
		t.Fatalf("until = %v ok=%v, want %v", got, ok, future)
	}

	// A later 6004 with an earlier parsed reset must never shorten the hold.
	noteModelRateLimit(uid, model, time.Now().Add(5*time.Minute))
	got, ok = wbModelRateLimits.until(key, time.Now())
	if !ok || !got.Equal(future) {
		t.Fatalf("hold was shortened: got %v, want %v", got, future)
	}

	// Zero reset falls back to the default window.
	defUid, defModel := "uid-registry-lifecycle", "glm-5.2"
	noteModelRateLimit(defUid, defModel, time.Time{})
	got, ok = modelRateLimitedUntil(defUid, defModel)
	if !ok || got.Before(time.Now()) || got.After(time.Now().Add(modelRateLimitDefaultWindow+time.Second)) {
		t.Fatalf("default window = %v ok=%v", got, ok)
	}

	// Lazy expiry: a past entry is dropped on read.
	expired := modelRateLimitKey("uid-registry-expired", "kimi-k2.7")
	wbModelRateLimits.note(expired, time.Now().Add(-time.Minute), time.Now())
	if _, ok := wbModelRateLimits.until(expired, time.Now()); ok {
		t.Fatalf("expired entry must be dropped")
	}
	if _, present := wbModelRateLimits.entries[expired]; present {
		t.Fatalf("expired entry must be deleted from the map")
	}
}

func TestNoteModelRateLimitFromPayload(t *testing.T) {
	uid, model := "uid-hook", "deepseek-v4.1-flash"

	// Non-6004 payloads never touch the registry.
	noteModelRateLimitFromPayload(uid, model, http.StatusTooManyRequests, `{"code":11120,"msg":"x"}`)
	if _, ok := modelRateLimitedUntil(uid, model); ok {
		t.Fatalf("non-6004 payload must not register a window")
	}

	// 6004 with a reset ~1h out → the registry carries exactly that instant.
	reset := time.Now().Add(time.Hour)
	stamp := reset.In(wbCNZone).Format("2006-01-02 15:04:05")
	payload := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` + stamp + ` UTC+8 重置，您也可以切换其他模型继续使用。"}`
	noteModelRateLimitFromPayload(uid, model, http.StatusTooManyRequests, payload)
	got, ok := modelRateLimitedUntil(uid, model)
	if !ok {
		t.Fatalf("6004 payload must register a window")
	}
	if diff := got.Sub(reset.Truncate(time.Second)); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("registered reset = %v, want ≈ %v", got, reset)
	}

	// 6004 without a parsable reset → default window entry.
	uid2, model2 := "uid-hook", "hy3-preview"
	noteModelRateLimitFromPayload(uid2, model2, http.StatusTooManyRequests, `{"code":6004,"msg":"limit"}`)
	got, ok = modelRateLimitedUntil(uid2, model2)
	if !ok || got.IsZero() {
		t.Fatalf("6004 without reset must still register the default window")
	}
}

// TestUpstreamStatusError6004StaysRequestLevel pins the v0.9.32 carve-out:
// the 6004 model-scoped rejection must NOT ride the 429 credential quota
// path on any host version — it stays a plain status-0 error.
func TestUpstreamStatusError6004StaysRequestLevel(t *testing.T) {
	base := errors.New("translated")
	err := upstreamStatusError(http.StatusTooManyRequests, modelRateLimitSamplePayload, base)
	var se *statusError
	if errors.As(err, &se) {
		t.Fatalf("6004 must stay plain, got statusError{%d}", se.status)
	}
	if !errors.Is(err, base) {
		t.Fatalf("6004 plain error must wrap the translated base")
	}
	// Regression: a 429 carrying a DIFFERENT business code still wraps.
	other := upstreamStatusError(http.StatusTooManyRequests, `{"code":11120,"msg":"quota"}`, base)
	if !errors.As(other, &se) || se.status != http.StatusTooManyRequests {
		t.Fatalf("non-6004 429 must still wrap as statusError{429}")
	}
}

func TestTranslateChatUpstreamErrorFullModelScopedCopy(t *testing.T) {
	err := translateChatUpstreamErrorFull(http.StatusTooManyRequests, modelRateLimitSamplePayload, nil, nil)
	msg := err.Error()
	for _, want := range []string{"6004", "其他模型", "UTC+8", "2026-09-22 09:46:39"} {
		if !strings.Contains(msg, want) {
			t.Errorf("6004 copy must contain %q, got %s", want, msg)
		}
	}
	// The literal "429" must never appear: hosts lacking the envelope status
	// would re-read it from the message as credential quota.
	if strings.Contains(msg, "429") {
		t.Errorf("6004 copy must not carry the literal 429: %s", msg)
	}

	// No parsable reset → generic auto-recovery clause, still no "429".
	err = translateChatUpstreamErrorFull(http.StatusTooManyRequests, `{"code":6004,"msg":"limit"}`, nil, nil)
	msg = err.Error()
	if !strings.Contains(msg, "6004") || !strings.Contains(msg, "自动恢复") {
		t.Errorf("reset-less copy must fall back to auto-recovery wording: %s", msg)
	}
	if strings.Contains(msg, "429") {
		t.Errorf("reset-less copy must not carry the literal 429: %s", msg)
	}
}

func TestModelRateLimitErrorFastFail(t *testing.T) {
	uid, model := "uid-fastfail", "deepseek-v4.1-flash"
	if err := modelRateLimitError(uid, model); err != nil {
		t.Fatalf("no window registered yet, fast-fail must be nil, got %v", err)
	}
	noteModelRateLimit(uid, model, time.Now().Add(10*time.Minute))
	err := modelRateLimitError(uid, model)
	if err == nil {
		t.Fatalf("active window must produce a fast-fail error")
	}
	msg := err.Error()
	for _, want := range []string{"6004", "其他模型"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fast-fail copy must contain %q, got %s", want, msg)
		}
	}
	// Plain error: no StatusCode() implementation may leak onto the fast-fail.
	type statusCarrier interface{ StatusCode() int }
	if _, carries := any(err).(statusCarrier); carries {
		t.Fatalf("fast-fail error must not implement StatusCode()")
	}
	// A different model on the same credential is untouched.
	if err := modelRateLimitError(uid, "glm-5.2"); err != nil {
		t.Fatalf("sibling model must be unaffected, got %v", err)
	}
}

// withModelRateLimitEntry notes a 6004 window and returns via cleanup.
func withModelRateLimitEntry(t *testing.T, authID, model string, reset time.Time) {
	t.Helper()
	noteModelRateLimit(authID, model, reset)
	key := modelRateLimitKey(authID, model)
	t.Cleanup(func() {
		wbModelRateLimits.mu.Lock()
		delete(wbModelRateLimits.entries, key)
		wbModelRateLimits.mu.Unlock()
	})
}

// catalogIDsFor drives handleModelForAuth and returns the advertised ids.
func catalogIDsFor(t *testing.T, req pluginapi.AuthModelRequest) []string {
	t.Helper()
	raw, err := handleModelForAuth(mustMarshal(t, req))
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
	return modelIDsOf(env.Result.Models)
}

// TestModelRateLimitSchedulerSkipsWindowedPair (issue #76): a candidate
// whose (auth, model) pair has an active 6004 window is not selectable —
// the pick lands on the healthy sibling instead of burning a selection on
// the executor's local fast-fail. Priority reclaim must not resurrect the
// dead pair either, and an all-windowed pool defers to the host like every
// other veto class.
func TestModelRateLimitSchedulerSkipsWindowedPair(t *testing.T) {
	resetActiveAuth(t)
	const model = "deepseek-v4.1-flash"
	withModelRateLimitEntry(t, "wb-76-a", model, time.Now().Add(20*time.Minute))

	pick := func(t *testing.T) pluginapi.SchedulerPickResponse {
		t.Helper()
		raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
			Provider: providerName,
			Model:    model,
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "wb-76-a", Provider: providerName},
				{ID: "wb-76-b", Provider: providerName},
			},
		}))
		if err != nil {
			t.Fatalf("handleSchedulerPick: %v", err)
		}
		return parsePickResponse(t, raw)
	}

	// Sticky mode with the panel selection ON the windowed pair: the pair
	// loses the pick, the healthy sibling serves.
	setActiveAuthID("wb-76-a")
	resp := pick(t)
	if !resp.Handled || resp.AuthID != "wb-76-b" {
		t.Fatalf("windowed pair must lose the pick, got %+v", resp)
	}

	// Priority reclaim serves the first live candidate in host order —
	// wb-76-a is first, but the 6004 window must still veto it.
	restore := setPriorityReclaim(true)
	defer restore()
	resp = pick(t)
	if !resp.Handled || resp.AuthID != "wb-76-b" {
		t.Fatalf("priority reclaim must not resurrect a windowed pair, got %+v", resp)
	}

	// Every candidate windowed → defer to the host (Handled:false).
	withModelRateLimitEntry(t, "wb-76-b", model, time.Now().Add(20*time.Minute))
	resp = pick(t)
	if resp.Handled {
		t.Fatalf("all-windowed pool must defer to the host, got %+v", resp)
	}
}

// TestModelRateLimitCatalogHidesWindowedPair (issue #76): model.for_auth
// stops advertising a pair under an active 6004 window for THIS credential;
// another credential's catalog is untouched; a host that sends no AuthID
// still resolves the same key from the stored credential.
func TestModelRateLimitCatalogHidesWindowedPair(t *testing.T) {
	resetPinnedModels()
	resetDynamicModelsCache()
	defer func() { resetPinnedModels(); resetDynamicModelsCache() }()
	pinnedModelsMu.Lock()
	pinnedModels["cn"] = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}
	pinnedModelsMu.Unlock()

	const model = "deepseek-v4.1-flash"
	withModelRateLimitEntry(t, "acct-76", model, time.Now().Add(20*time.Minute))

	storage := []byte(`{"auth":{"domain":"copilot.tencent.com","accessToken":"tok"},"account":{"uid":"acct-76"}}`)
	ids := catalogIDsFor(t, pluginapi.AuthModelRequest{
		AuthID:       "acct-76",
		AuthProvider: providerName,
		StorageJSON:  storage,
	})
	if len(ids) != 1 || ids[0] != "glm-5.3-flash" {
		t.Fatalf("windowed pair must be hidden from the catalog: %v", ids)
	}

	// Another credential keeps the pair — the window is per (credential,
	// model), never global.
	ids = catalogIDsFor(t, pluginapi.AuthModelRequest{
		AuthID:       "acct-77",
		AuthProvider: providerName,
		StorageJSON:  []byte(`{"auth":{"domain":"copilot.tencent.com","accessToken":"tok"},"account":{"uid":"acct-77"}}`),
	})
	if len(ids) != 2 {
		t.Fatalf("sibling credential must keep the pair: %v", ids)
	}

	// Host without AuthID: the key derives from the stored credential —
	// the same sanitized UID, so the window still hides the pair.
	ids = catalogIDsFor(t, pluginapi.AuthModelRequest{
		AuthProvider: providerName,
		StorageJSON:  storage,
	})
	if len(ids) != 1 || ids[0] != "glm-5.3-flash" {
		t.Fatalf("storage-derived key must match the recorded window: %v", ids)
	}
}

// TestModelRateLimitJoinKeyMatchesHostConvention (issue #76): the canonical
// join key the reporter asked the maintainer to confirm. A credential whose
// raw UID carries filename-unsafe characters records its window under the
// SANITIZED form (the host auth id); scheduler candidates and the
// storage-derived catalog key resolve to that same string. The
// workbuddy-intl-<uid>.json names the reporter observed are the FILE layer —
// ids never carry the prefix, so the join holds on every host version.
func TestModelRateLimitJoinKeyMatchesHostConvention(t *testing.T) {
	const rawUID = "user@tenant|42"
	const model = "glm-5.3-flash"
	sa := &storedAuth{}
	sa.Account.UID = rawUID

	// An old host that sends no AuthID: the note falls back to the
	// sanitized-UID convention (cooldownAuthIDFor).
	noted := modelRateLimitAuthID("", sa)
	if noted != "user_tenant_42" {
		t.Fatalf("sanitized auth id = %q, want user_tenant_42", noted)
	}
	withModelRateLimitEntry(t, noted, model, time.Now().Add(20*time.Minute))

	// The scheduler's candidate id (the host auth id) resolves the window.
	if !modelRateLimitActive("user_tenant_42", model) {
		t.Fatal("scheduler candidate id must resolve the window")
	}
	// The raw UID is no longer a live key (the v0.9.32 keying is gone).
	if modelRateLimitActive(rawUID, model) {
		t.Fatal("raw UID must not be a registry key")
	}

	// The catalog derives the same key from storage when AuthID is absent.
	resetPinnedModels()
	resetDynamicModelsCache()
	defer func() { resetPinnedModels(); resetDynamicModelsCache() }()
	pinnedModelsMu.Lock()
	pinnedModels["cn"] = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}
	pinnedModelsMu.Unlock()
	ids := catalogIDsFor(t, pluginapi.AuthModelRequest{
		AuthProvider: providerName,
		StorageJSON:  []byte(`{"auth":{"domain":"copilot.tencent.com","accessToken":"tok"},"account":{"uid":"user@tenant|42"}}`),
	})
	if len(ids) != 1 || ids[0] != "deepseek-v4.1-flash" {
		t.Fatalf("storage-derived catalog key must hide the windowed pair: %v", ids)
	}
}

// TestModelRateLimitSnapshotAndClear locks the issue #77 operator surface:
// the registry's own views list active windows in the cooldown row shape,
// and clearModelRateLimit is the manual "retry now" exit.
func TestModelRateLimitSnapshotAndClear(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	withFrozenModelRateLimits(t, now)

	noteModelRateLimit("acct-a", "deepseek-v4.1-flash", now.Add(40*time.Minute))
	noteModelRateLimit("acct-a", "glm-5.2-air", now.Add(25*time.Minute))
	noteModelRateLimit("acct-b", "deepseek-v4.1-flash", now.Add(5*time.Minute))

	// Per-account view: only acct-a's pairs, no auth_id key, model-sorted.
	rows := modelRateLimitSnapshotFor("acct-a")
	if len(rows) != 2 {
		t.Fatalf("snapshotFor(acct-a) = %#v", rows)
	}
	if rows[0]["model"] != "deepseek-v4.1-flash" || rows[1]["model"] != "glm-5.2-air" {
		t.Fatalf("model order = %#v", rows)
	}
	for _, row := range rows {
		if row["reason"] != modelRateLimitViewReason {
			t.Fatalf("reason = %#v", row)
		}
		if _, has := row["auth_id"]; has {
			t.Fatal("per-account rows must not carry auth_id")
		}
		if got := row["updated_at"]; got != now.UTC().Format(time.RFC3339) {
			t.Fatalf("updated_at = %v, want %v", got, now.UTC().Format(time.RFC3339))
		}
	}
	if secs := rows[0]["seconds"].(int); secs < 2395 || secs > 2405 {
		t.Fatalf("seconds = %d, want ~2400", secs)
	}

	// All-accounts view: three rows, auth_id present, account-then-model order.
	all := modelRateLimitSnapshotAll()
	if len(all) != 3 {
		t.Fatalf("snapshotAll = %#v", all)
	}
	if all[0]["auth_id"] != "acct-a" || all[2]["auth_id"] != "acct-b" {
		t.Fatalf("account order = %#v", all)
	}

	// Blank auth id: no rows, no panic.
	if got := modelRateLimitSnapshotFor("  "); got != nil {
		t.Fatalf("blank auth snapshot = %#v", got)
	}

	// Expired entries are swept lazily by the snapshot.
	wbModelRateLimits.note(modelRateLimitKey("acct-c", "kimi-k2.7"), now.Add(-time.Minute), now.Add(-2*time.Minute))
	if swept := modelRateLimitSnapshotFor("acct-c"); len(swept) != 0 {
		t.Fatalf("expired entry must not list: %#v", swept)
	}
	wbModelRateLimits.mu.Lock()
	if _, ok := wbModelRateLimits.entries[modelRateLimitKey("acct-c", "kimi-k2.7")]; ok {
		t.Fatal("expired entry must be swept from the map")
	}
	wbModelRateLimits.mu.Unlock()

	// Clear one pair — case and padding normalize exactly like the key join.
	if n := clearModelRateLimit("acct-a", " DeepSeek-V4.1-FLASH "); n != 1 {
		t.Fatalf("clear pair removed %d", n)
	}
	if len(modelRateLimitSnapshotFor("acct-a")) != 1 {
		t.Fatal("acct-a must have one window left")
	}
	// Unknown pair / blank account: zero removed, nothing else disturbed.
	if n := clearModelRateLimit("acct-a", "nope"); n != 0 {
		t.Fatalf("clear unknown removed %d", n)
	}
	if n := clearModelRateLimit("", "deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("blank auth removed %d", n)
	}
	// Clear the whole account.
	if n := clearModelRateLimit("acct-a", ""); n != 1 {
		t.Fatalf("clear account removed %d", n)
	}
	if len(modelRateLimitSnapshotFor("acct-a")) != 0 {
		t.Fatal("acct-a must be empty after the account clear")
	}
	if len(modelRateLimitSnapshotAll()) != 1 {
		t.Fatal("acct-b must survive the acct-a clear")
	}
}
