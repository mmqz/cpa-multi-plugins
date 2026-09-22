package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// realmTestModels builds a ModelInfo list with the given IDs for cache tests.
func realmTestModels(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}})
	}
	return out
}

// resetDynamicModelsCache empties the package-level realm cache — tests share
// the process, and a stale entry from one test would poison another's hints.
func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.realms = map[string]realmModelsEntry{}
	dynamicModelsCache.Unlock()
}

// makeJWTWithIss builds a minimal JWT-shaped token whose payload carries the
// given iss claim — enough for isGlobalToken's base64/iss decode.
func makeJWTWithIss(iss string) string {
	payload, _ := json.Marshal(map[string]string{"iss": iss})
	return "hdr." + base64.URLEncoding.EncodeToString(payload) + ".sig"
}

// TestModelsEndpointFor pins the v0.12.18 realm→discovery-endpoint routing.
// Regression guard: the old callModelsAPI only special-cased Global and sent
// Intl (codebuddy.ai) tokens to the CN endpoint, so Intl accounts were shown
// the CN catalog and chat calls with CN-only models (deepseek-v4-flash) died
// on the Intl gateway with code 11102 "service info not found".
func TestModelsEndpointFor(t *testing.T) {
	cases := []struct {
		realm      string
		wantURL    string
		wantOrigin string
	}{
		{"cn", "https://copilot.tencent.com/console/enterprises/personal/models", "https://www.codebuddy.cn"},
		{"global", "https://www.workbuddy.ai/console/enterprises/personal/models", "https://www.workbuddy.ai"},
		{"intl", "https://www.codebuddy.ai/console/enterprises/personal/models", "https://www.codebuddy.ai"},
		{"", "https://copilot.tencent.com/console/enterprises/personal/models", "https://www.codebuddy.cn"}, // unknown → CN default
	}
	for _, c := range cases {
		url, origin := modelsEndpointFor(c.realm)
		if url != c.wantURL || origin != c.wantOrigin {
			t.Errorf("realm=%q: got (%s, %s), want (%s, %s)", c.realm, url, origin, c.wantURL, c.wantOrigin)
		}
	}
}

// TestRealmForStorage covers realm classification across the three storage
// shapes that reach model.for_auth: nested plugin OAuth files, flat CPA-UI
// imports, and legacy files with neither domain nor region (JWT iss fallback).
func TestRealmForStorage(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		token string
		want  string
	}{
		{"nested intl domain", `{"auth":{"domain":"codebuddy.ai","accessToken":"t"}}`, "t", "intl"},
		{"nested global region", `{"auth":{"region":"global"}}`, "t", "global"},
		{"nested cn region", `{"auth":{"region":"cn"}}`, "t", "cn"},
		{"flat intl domain", `{"domain":"www.codebuddy.ai"}`, "t", "intl"},
		{"flat global domain", `{"domain":"workbuddy.ai"}`, "t", "global"},
		{"flat region field", `{"region":"intl"}`, "t", "intl"},
		{"legacy iss global", `{}`, makeJWTWithIss("https://auth.workbuddy.ai/realms/workbuddy"), "global"},
		{"legacy iss cn", `{}`, makeJWTWithIss("https://codebuddy.cn"), "cn"},
		{"garbage json + cn token", `not-json`, "t", "cn"},
	}
	for _, c := range cases {
		if got := realmForStorage([]byte(c.raw), c.token); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// TestRealmFromRegionDomain pins the pair-mapping including the empty default
// (unknown pair → "" so the caller falls through to the next probe).
func TestRealmFromRegionDomain(t *testing.T) {
	if got := realmFromRegionDomain("", ""); got != "" {
		t.Errorf("empty pair: got %q, want empty", got)
	}
	if got := realmFromRegionDomain("INTL", ""); got != "intl" {
		t.Errorf("case-insensitive region: got %q", got)
	}
	if got := realmFromRegionDomain("", "sub.codebuddy.ai"); got != "intl" {
		t.Errorf("domain suffix: got %q", got)
	}
	if got := realmFromRegionDomain("unknown", "example.com"); got != "" {
		t.Errorf("unknown pair: got %q, want empty", got)
	}
}

// TestDynamicModelsCachePerRealm proves one realm's discovery answer never
// satisfies another realm's model.for_auth (the old single-entry cache let
// CN answers leak into Intl listings).
func TestDynamicModelsCachePerRealm(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	cn := realmTestModels("cn-catalog-model")
	intl := realmTestModels("intl-only-model")
	storeDynamicModels("cn", cn)
	storeDynamicModels("intl", intl)

	gotCN, okCN := cachedDynamicModels("cn")
	if !okCN || len(gotCN) != 1 || gotCN[0].ID != cn[0].ID {
		t.Fatalf("cn cache: ok=%v len=%d", okCN, len(gotCN))
	}
	gotIntl, okIntl := cachedDynamicModels("intl")
	if !okIntl || len(gotIntl) != len(intl) {
		t.Fatalf("intl cache: ok=%v len=%d", okIntl, len(gotIntl))
	}
	if gotCN[0].ID == gotIntl[0].ID {
		t.Fatalf("cache collision: cn=%s intl=%s", gotCN[0].ID, gotIntl[0].ID)
	}
	if _, ok := cachedDynamicModels("global"); ok {
		t.Fatalf("unknown realm must miss")
	}
}

// TestStaticAdvertisementEmpty pins the v0.9.33 discovery-only policy: the
// host's model.static call advertises NOTHING and a credential without a
// token advertises nothing either. Upstream retired the hy3 family on
// 2026-09-22 while the old hand-maintained static catalog still listed it —
// the stale ids surfaced as host-level "unknown provider" 400s that no
// plugin log could explain. Better empty than wrong: advertisement must
// only ever mirror what upstream discovery currently serves.
func TestStaticAdvertisementEmpty(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	raw, err := handleModelStatic([]byte(`{}`))
	if err != nil {
		t.Fatalf("handleModelStatic: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("static advertisement must be empty, got %d model(s)", len(resp.Models))
	}

	// Tokenless credential → discovery impossible → nothing advertised.
	storage := []byte(`{"auth":{"region":"cn"}}`)
	if got := fetchDynamicModelsFromStorage(storage); len(got) != 0 {
		t.Fatalf("tokenless credential must advertise nothing, got %#v", got)
	}
	if st := realmModelStateFor("cn"); st == nil || st.Source != "none (no token in storage)" {
		t.Fatalf("state source = %+v", st)
	}
}

// TestParsePinnedModelList covers quoting, YAML flow lists, dedup and empties.
func TestParsePinnedModelList(t *testing.T) {
	if got := parsePinnedModelList("hy4-preview"); !pinnedSame(got, []string{"hy4-preview"}) {
		t.Errorf("single: got %#v", got)
	}
	if got := parsePinnedModelList(` "hy4-preview, claude-sonnet-5" `); !pinnedSame(got, []string{"hy4-preview", "claude-sonnet-5"}) {
		t.Errorf("quoted pair: got %#v", got)
	}
	if got := parsePinnedModelList("[A, b ,a]"); !pinnedSame(got, []string{"A", "b"}) {
		t.Errorf("flow dedup (case-insensitive): got %#v", got)
	}
	if got := parsePinnedModelList(""); got != nil {
		t.Errorf("empty: got %#v", got)
	}
	if got := parsePinnedModelList(",, ,"); got != nil {
		t.Errorf("commas only: got %#v", got)
	}
}

func pinnedSame(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resetPinnedModels empties the pinned config between tests.
func resetPinnedModels() {
	pinnedModelsMu.Lock()
	pinnedModels = map[string][]string{}
	pinnedModelsMu.Unlock()
}

// configYAMLEnvelope wraps raw YAML in the plugin.register/reconfigure JSON
// envelope the host actually sends (config_yaml carries the YAML bytes).
func configYAMLEnvelope(yaml string) []byte {
	req := struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(yaml)}
	raw, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestConfigurePinsModelsRealm pins the config_yaml → realm accessor path.
// v0.9.33: pinned IDs all get generic metadata (the static metadata table
// is gone) — order, dedup and realm reset semantics are unchanged.
func TestConfigurePinsModelsRealm(t *testing.T) {
	resetPinnedModels()
	defer resetPinnedModels()
	configure(configYAMLEnvelope("enabled: true\nmodels_intl: \"hy4-preview, claude-sonnet-5\"\n"))
	got := pinnedModelsForRealm("intl")
	if len(got) != 2 || got[0].ID != "hy4-preview" || got[1].ID != "claude-sonnet-5" {
		t.Fatalf("pinned intl: got %#v", got)
	}
	if got[0].Name != "hy4-preview" || got[0].ContextLength != 0 {
		t.Fatalf("pinned ID gets generic metadata (v0.9.33), got %#v", got[0])
	}
	if got[1].OwnedBy != providerName || got[1].ContextLength != 0 {
		t.Fatalf("unknown ID gets generic metadata, got %#v", got[1])
	}
	if got := pinnedModelsForRealm("cn"); got != nil {
		t.Fatalf("cn unpinned: got %#v", got)
	}
	// Reconfigure without the key resets the realm to discovery+static.
	configure(configYAMLEnvelope("enabled: true\n"))
	if got := pinnedModelsForRealm("intl"); got != nil {
		t.Fatalf("missing key must reset pins, got %#v", got)
	}
}

// TestFetchDynamicModelsPinnedWins: a pinned realm skips discovery entirely.
func TestFetchDynamicModelsPinnedWins(t *testing.T) {
	resetPinnedModels()
	resetDynamicModelsCache()
	defer func() { resetPinnedModels(); resetDynamicModelsCache() }()
	pinnedModelsMu.Lock()
	pinnedModels["intl"] = []string{"claude-sonnet-5"}
	pinnedModelsMu.Unlock()
	calls := 0
	orig := discoverModelsFn
	discoverModelsFn = func(token, realm, uid string) ([]pluginapi.ModelInfo, error) {
		calls++
		return realmTestModels("should-not-be-used"), nil
	}
	defer func() { discoverModelsFn = orig }()
	raw := []byte(`{"auth":{"domain":"codebuddy.ai","accessToken":"tok"}}`)
	got := fetchDynamicModelsFromStorage(raw)
	if len(got) != 1 || got[0].ID != "claude-sonnet-5" {
		t.Fatalf("pinned output expected, got %#v", got)
	}
	if calls != 0 {
		t.Fatalf("discovery must be skipped when pinned, calls=%d", calls)
	}
}

// TestFetchDynamicModelsFailurePerRealm: with the static catalogs gone
// (v0.9.33), a discovery failure with no cache advertises NOTHING for every
// realm — the v0.12.18 bug (shared CN fallback advertised deepseek-v4-flash
// to Intl accounts → upstream 11102) stays dead by construction, and the
// 2026-09-22 hy3 rot (stale static ids) cannot come back either.
func TestFetchDynamicModelsFailurePerRealm(t *testing.T) {
	resetPinnedModels()
	resetDynamicModelsCache()
	defer func() { resetPinnedModels(); resetDynamicModelsCache() }()
	orig := discoverModelsFn
	discoverModelsFn = func(token, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("models API status 500")
	}
	defer func() { discoverModelsFn = orig }()

	intlRaw := []byte(`{"auth":{"domain":"codebuddy.ai","accessToken":"tok"}}`)
	if got := fetchDynamicModelsFromStorage(intlRaw); len(got) != 0 {
		t.Fatalf("intl failure must advertise nothing, got %#v", got)
	}
	if st := realmModelStateFor("intl"); st == nil || st.Source != "none (discovery failed)" {
		t.Fatalf("intl state = %+v", st)
	}

	cnRaw := []byte(`{"auth":{"region":"cn","accessToken":"tok"}}`)
	if got := fetchDynamicModelsFromStorage(cnRaw); len(got) != 0 {
		t.Fatalf("cn failure must advertise nothing, got %#v", got)
	}
	if st := realmModelStateFor("cn"); st == nil || st.Source != "none (discovery failed)" {
		t.Fatalf("cn state = %+v", st)
	}
}

// TestModelHintForRealm checks the 11102 error hint under the v0.9.33
// discovery-only policy: the fresh cache wins, the stale cache (last known
// discovery answer) is next, and with no cache at all the hint says so and
// points at the models_<realm> pin instead of inventing model ids.
func TestModelHintForRealm(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	hint := modelHintForRealm("intl")
	if strings.Contains(hint, "deepseek") || strings.Contains(hint, "hy4") {
		t.Fatalf("empty-realm hint must not invent model ids, got: %s", hint)
	}
	if !strings.Contains(hint, "no cached catalog") || !strings.Contains(hint, "models_intl") {
		t.Fatalf("empty-realm hint must be bilingual and point at the pin, got: %s", hint)
	}
	// After a successful intl discovery the hint lists the cached ids.
	storeDynamicModels("intl", realmTestModels("intl-real-model"))
	hint = modelHintForRealm("intl")
	if !strings.Contains(hint, "intl-real-model") || !strings.Contains(hint, "cached realm catalog") {
		t.Fatalf("cached label missing, got: %s", hint)
	}
}

// TestTranslateChatUpstreamError_11102 pins the bilingual actionable rewrite
// for the Intl 11102 rejection and the untouched passthrough for everything
// else (log parsers rely on the "upstream <status>: <payload>" shape).
func TestTranslateChatUpstreamError_11102(t *testing.T) {
	payload := `{"code":11102,"msg":"model [deepseek-v4-flash] service info not found","requestId":"6b416a6b-b9c9-499e-ae30-8d92cb4e0e04"}`
	sa := &storedAuth{}
	sa.Auth.Domain = "codebuddy.ai"

	err := translateChatUpstreamError(http.StatusBadRequest, payload, sa)
	msg := err.Error()
	for _, want := range []string{"11102", "codebuddy.ai", "deepseek-v4-flash"} {
		if !strings.Contains(msg, want) {
			t.Errorf("11102 error missing %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "cached realm catalog") && !strings.Contains(msg, "last known realm catalog") && !strings.Contains(msg, "no cached catalog") {
		t.Errorf("11102 error must carry a realm model catalog hint: %s", msg)
	}

	// Global account → its gateway is named, not the Intl one.
	saG := &storedAuth{}
	saG.Auth.Domain = "workbuddy.ai"
	if got := translateChatUpstreamError(http.StatusBadRequest, payload, saG).Error(); !strings.Contains(got, "workbuddy.ai") {
		t.Errorf("global account error must name the global gateway: %s", got)
	}

	// Non-11102 failure keeps the historical passthrough shape.
	other := `{"code":1001,"message":"unauthorized"}`
	if got := translateChatUpstreamError(http.StatusUnauthorized, other, sa).Error(); !strings.HasPrefix(got, "upstream 401: ") {
		t.Errorf("non-11102 must keep raw shape, got: %s", got)
	}

	// Marker-based detection for non-JSON envelopes.
	if !isModelNotRegistered(http.StatusBadRequest, `<html>11102 service info not found</html>`) {
		t.Errorf("marker path must detect wrapped 11102 payloads")
	}
	if isModelNotRegistered(http.StatusInternalServerError, payload) {
		t.Errorf("only 400 carries the 11102 rewrite")
	}
}
