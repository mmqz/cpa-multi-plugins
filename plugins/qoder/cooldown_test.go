// cooldown_test.go pins the per-(account, model) cooldown semantics added in
// v0.8.18 (fork bfSan/qoder-cpa-plugin review, adapted): classification of
// upstream failures, expiry, bounded table, model-key normalization, and the
// per-credential catalog filter that host routing consults.
package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func withCooldownNow(t *testing.T, now time.Time) func() {
	t.Helper()
	old := cooldownNowFn
	cooldownNowFn = func() time.Time { return now }
	return func() { cooldownNowFn = old }
}

func TestModelCooldownMarkExpiryClear(t *testing.T) {
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	restore := withCooldownNow(t, base)
	defer restore()

	if modelIsCooling("acct-1", "dfmodel") {
		t.Fatal("fresh table must not report cooling")
	}
	markModelCooldown("acct-1", "dfmodel", cooldownReasonRateLimit)
	if !modelIsCooling("acct-1", "dfmodel") {
		t.Fatal("marked pair must be cooling")
	}
	if got := modelCoolingUntil("acct-1", "dfmodel").Sub(base); got != modelCooldownRateLimit {
		t.Fatalf("rate-limit TTL = %v, want %v", got, modelCooldownRateLimit)
	}
	// Unknown-failure TTL is shorter.
	markModelCooldown("acct-2", "gmodel", cooldownReasonUnknown)
	if got := modelCoolingUntil("acct-2", "gmodel").Sub(base); got != modelCooldownUnknown {
		t.Fatalf("unknown TTL = %v, want %v", got, modelCooldownUnknown)
	}
	// Expired entries disappear on query.
	restore()
	restore2 := withCooldownNow(t, base.Add(modelCooldownUnknown+time.Second))
	defer restore2()
	if modelIsCooling("acct-2", "gmodel") {
		t.Fatal("expired entry must not be cooling")
	}
	if !modelCoolingUntil("acct-2", "gmodel").IsZero() {
		t.Fatal("expired entry must report zero time")
	}
	if n := clearModelCooldown("acct-1", "dfmodel"); n != 1 {
		t.Fatalf("clear removed %d entries, want 1", n)
	}
	if modelIsCooling("acct-1", "dfmodel") {
		t.Fatal("cleared pair must not be cooling")
	}
	if n := clearModelCooldown("nobody", ""); n != 0 {
		t.Fatalf("clear with empty model on unknown auth removed %d", n)
	}
}

func TestRequestModelForCooldown(t *testing.T) {
	// Metadata wins over req.Model (routing model survives alias rewrite).
	got := requestModelForCooldown("qoder/dfmodel", map[string]any{"auth_selection_model": "qoder/gmodel"})
	if got != "gmodel" {
		t.Fatalf("metadata model = %q, want gmodel", got)
	}
	// requested_model is the second key.
	got = requestModelForCooldown("qoder/dfmodel", map[string]any{"requested_model": "kmodel"})
	if got != "kmodel" {
		t.Fatalf("requested_model = %q, want kmodel", got)
	}
	// Fallback normalizes provider prefix + alias table onto catalog keys.
	got = requestModelForCooldown("qoder/deepseek-flash", nil)
	if got != "dfmodel" {
		t.Fatalf("normalized = %q, want dfmodel", got)
	}
	// Bare catalog key passes through.
	if got := requestModelForCooldown("dfmodel", nil); got != "dfmodel" {
		t.Fatalf("bare key = %q, want dfmodel", got)
	}
	// Empty model → empty key (entries without a model must be ignored).
	if got := requestModelForCooldown("  ", nil); got != "" {
		t.Fatalf("empty model = %q, want empty", got)
	}
}

func TestRecordUpstreamFailureClassification(t *testing.T) {
	base := time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC)
	restore := withCooldownNow(t, base)
	defer restore()

	// 429 → rate-limit backoff.
	recordUpstreamFailure("a1", "dfmodel", 429, `{"error":"slow down"}`)
	if !modelIsCooling("a1", "dfmodel") {
		t.Fatal("429 must cool the pair")
	}
	// Body-driven soft rate limit (non-429 status).
	recordUpstreamFailure("a2", "dfmodel", 503, `upstream Rate Limit exceeded`)
	if !modelIsCooling("a2", "dfmodel") {
		t.Fatal("soft rate-limit body must cool the pair")
	}
	// Empty stream rides any status (body is the discriminator).
	recordUpstreamFailure("a3", "gmodel", 0, "empty_stream: qoder upstream closed before a completion payload")
	if !modelIsCooling("a3", "gmodel") {
		t.Fatal("empty_stream must cool the pair")
	}
	// Hard-credit failures stay with lifecycle handling, not model cooldown.
	recordUpstreamFailure("a4", "dfmodel", 200, "Insufficient Credits for this account")
	if modelIsCooling("a4", "dfmodel") {
		t.Fatal("hard-credit failure must NOT cool the model pair")
	}
	// Transport noise with no rate-limit/empty-stream marker → no entry.
	recordUpstreamFailure("a5", "dfmodel", 0, "connection reset by peer")
	if modelIsCooling("a5", "dfmodel") {
		t.Fatal("transport noise must not cool the pair")
	}
	// Unresolvable model (empty after normalization) → ignored entirely.
	recordUpstreamFailure("a6", "", 429, "x")
	if len(cooldownSnapshotFor("a6")) != 0 {
		t.Fatal("empty model must not create entries")
	}
}

func TestModelCooldownBoundedTable(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	restore := withCooldownNow(t, base)
	defer restore()

	for i := 0; i < modelCooldownMaxEntries+50; i++ {
		markModelCooldown("bulk", fmt.Sprintf("m%05d", i), cooldownReasonUnknown)
	}
	cooldownMu.Lock()
	size := len(cooldownTable)
	cooldownMu.Unlock()
	if size > modelCooldownMaxEntries {
		t.Fatalf("table size %d exceeds bound %d", size, modelCooldownMaxEntries)
	}
	clearModelCooldown("bulk", "")
	cooldownMu.Lock()
	size = len(cooldownTable)
	cooldownMu.Unlock()
	if size != 0 {
		t.Fatalf("account-wide clear left %d entries", size)
	}
}

func TestFilterCoolingModelsPerCredential(t *testing.T) {
	base := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC)
	restore := withCooldownNow(t, base)
	defer restore()

	sa := &storedAuth{}
	sa.Account.UID = "u 10086"  // sanitizes to u_10086
	sa.Auth.AccessToken = "tok" // keep struct fields referenced
	catalog := []pluginapi.ModelInfo{
		{ID: "dfmodel", Name: "DeepSeek-Flash"},
		{ID: "gmodel", Name: "GLM-5.3"},
		{ID: "kmodel", Name: "Kimi-K2.8-Preview"},
	}
	authID := cooldownAuthIDFor(sa)
	if authID != "u_10086" {
		t.Fatalf("authID = %q, want u_10086", authID)
	}
	// No cooldowns → list untouched (same slice, zero allocs path).
	if got := filterCoolingModels(sa, catalog); len(got) != 3 {
		t.Fatalf("clean filter changed list size: %d", len(got))
	}
	markModelCooldown(authID, "dfmodel", cooldownReasonUnknown)
	got := filterCoolingModels(sa, catalog)
	if len(got) != 2 {
		t.Fatalf("filtered size = %d, want 2", len(got))
	}
	for _, m := range got {
		if m.ID == "dfmodel" {
			t.Fatal("cooled model must be filtered from the credential catalog")
		}
	}
	// Another credential keeps the full catalog (per-(auth, model) scoping).
	other := &storedAuth{}
	other.Account.UID = "u-999"
	if got := filterCoolingModels(other, catalog); len(got) != 3 {
		t.Fatalf("other credential lost models: %d", len(got))
	}
	// Cooldown marking must invalidate the discovery cache.
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = catalog
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	markModelCooldown(authID, "gmodel", cooldownReasonUnknown)
	if _, ok := cachedDynamicModels("k"); ok {
		t.Fatal("cooldown mark must drop the dynamic model cache")
	}
}
