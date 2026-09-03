package main

import (
	"encoding/base64"
	"encoding/json"
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
	cn := wbModels()[:1]
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

// TestModelHintForRealm checks the 11102 error hint: cached discovery wins,
// static fallback is clearly labeled as CN-flavored, and the list is bounded.
func TestModelHintForRealm(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	hint := modelHintForRealm("intl")
	if !strings.Contains(hint, "deepseek-v4-flash") {
		t.Fatalf("static fallback must list the static IDs, got: %s", hint)
	}
	if !strings.Contains(hint, "static CN catalog") || !strings.Contains(hint, "静态 CN 目录") {
		t.Fatalf("static fallback must be labeled bilingual, got: %s", hint)
	}
	// After a successful intl discovery the hint switches to the cached label.
	storeDynamicModels("intl", realmTestModels("intl-real-model"))
	hint = modelHintForRealm("intl")
	if strings.Contains(hint, "static CN catalog") {
		t.Fatalf("cached catalog must not be labeled static, got: %s", hint)
	}
	if !strings.Contains(hint, "cached realm catalog") {
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
	if !strings.Contains(msg, "static CN catalog") && !strings.Contains(msg, "cached realm catalog") {
		t.Errorf("11102 error must carry a model catalog hint: %s", msg)
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
