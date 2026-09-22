package main

// pr_absorb_test.go locks the behavior absorbed from PR #4 and PR #6 (both
// clean-room re-applied: the PRs' commits carry Claude co-author trailers and
// were never merged; the CODE was ported by hand onto the current tree).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// --- PR #4: regex fallback for blocked-template variants ---

func TestSanitizeBlockedTemplates_Variants(t *testing.T) {
	cases := []struct{ in, wantContains, wantMissing string }{
		{
			// verbatim template (OmniRoute parity)
			in:           "You are Claude Code, Anthropic's official CLI for Claude.",
			wantContains: "official CLI tool for Claude",
			wantMissing:  "official CLI for Claude.",
		},
		{
			// capitalization + no-leading-phrase variant (PR #4 safety net)
			in:           "ANTHROPIC'S OFFICIAL CLI FOR CLAUDE",
			wantContains: "official CLI tool for Claude",
			wantMissing:  "OFFICIAL CLI FOR CLAUDE",
		},
		{
			// mid-sentence variant without the "You are Claude Code" prefix
			in:           "Config from anthropic's official CLI for claude applies.",
			wantContains: "Anthropic's official CLI tool for Claude",
			wantMissing:  "anthropic's official CLI for claude applies",
		},
		{
			// main-branch git injection, lowercase drift
			in:           "main branch (you will usually use this for PRS)",
			wantContains: "Default branch (you will usually use this for PRs)",
			wantMissing:  "main branch (you will usually use this for PRS)",
		},
	}
	for _, tc := range cases {
		if got := sanitizeBlockedTemplates(tc.in); !strings.Contains(got, tc.wantContains) || strings.Contains(got, tc.wantMissing) {
			t.Errorf("sanitizeBlockedTemplates(%q) = %q; want contains %q, missing %q", tc.in, got, tc.wantContains, tc.wantMissing)
		}
	}
	// untouched text must pass through unchanged
	if got := sanitizeBlockedTemplates("harmless user prompt text"); got != "harmless user prompt text" {
		t.Errorf("clean text mutated: %q", got)
	}
}

// --- PR #6 / a19bb56: SSE comment frames must not be re-emitted as data: ---

func TestCleanChunkJSON_CommentFramesDropped(t *testing.T) {
	for _, frame := range []string{": keep-alive", ": heartbeat", ": chunky bacon"} {
		if got := cleanChunkJSON(frame); got != "" {
			t.Errorf("cleanChunkJSON(%q) = %q, want \"\" (comment frame must not be re-emitted as data:)", frame, got)
		}
	}
	// real payloads still pass through
	if got := cleanChunkJSON(`{"choices":[{"delta":{"content":"hi"}}]}`); !strings.Contains(got, `"content":"hi"`) {
		t.Errorf("real chunk damaged: %q", got)
	}
}

// --- issue #5 / PR #6: reasoning_content folded back into content ---

func TestInjectReasoningInPlace(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "q1"},
			map[string]any{
				"role":              "assistant",
				"content":           "a1",
				"reasoning_content": " first I check the config ",
			},
			map[string]any{"role": "user", "content": "q2"},
		},
	}
	if !injectReasoningInPlace(obj) {
		t.Fatal("expected a change")
	}
	msgs := obj["messages"].([]any)
	a1 := msgs[2].(map[string]any)
	got := a1["content"].(string)
	if !strings.HasPrefix(got, "<thought>\nfirst I check the config\n</thought>\n\na1") {
		t.Fatalf("assistant content not folded: %q", got)
	}
	// idempotent: a second pass must not double-prepend
	if injectReasoningInPlace(obj) {
		t.Error("second pass must be a no-op (idempotency)")
	}
	a1b := obj["messages"].([]any)[2].(map[string]any)
	if got := a1b["content"].(string); strings.Count(got, "<thought>") != 1 {
		t.Fatalf("double <thought> block: %q", got)
	}
}

func TestInjectReasoningInPlace_MultimodalAndNonAssistant(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{
				"role":      "assistant",
				"reasoning": "alt reasoning field",
				"content":   []any{map[string]any{"type": "text", "text": "answer"}},
			},
			map[string]any{"role": "user", "content": "plain"},
			map[string]any{"role": "assistant", "content": "no reasoning here"},
		},
	}
	if !injectReasoningInPlace(obj) {
		t.Fatal("expected the multimodal assistant turn to be folded")
	}
	msgs := obj["messages"].([]any)
	a1 := msgs[0].(map[string]any)
	parts := a1["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("multimodal content should gain one leading part, got %d", len(parts))
	}
	first := parts[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "alt reasoning field") {
		t.Errorf("leading part missing thought block: %v", first)
	}
	if _, has := msgs[1].(map[string]any); !has {
		t.Fatal("user message missing")
	}
	// user and reasoning-less assistant must be untouched
	if u := msgs[1].(map[string]any); u["content"] != "plain" {
		t.Errorf("user message mutated: %v", u)
	}
	if a2 := msgs[2].(map[string]any); a2["content"] != "no reasoning here" {
		t.Errorf("reasoning-less assistant mutated: %v", a2)
	}
}

// prepareUpstreamBody must run the fold (issue #5 end-to-end through the
// single-pass pipeline).
func TestPrepareUpstreamBody_FoldsReasoning(t *testing.T) {
	sa := &storedAuth{}
	payload := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":"a","reasoning_content":"step 1"},` +
		`{"role":"user","content":"q2"}]}`)
	out := prepareUpstreamBody(payload, nil, sa, "m")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("message count = %d", len(msgs))
	}
	a1 := msgs[1].(map[string]any)
	content, _ := a1["content"].(string)
	if !strings.Contains(content, "<thought>") || !strings.Contains(content, "step 1") {
		t.Fatalf("pipeline output lost reasoning fold: %s", content)
	}
}

// --- PR #6 / ab64e63: transient discovery failure keeps the last good list ---

func TestDiscoveryTransientFailureKeepsLastGood(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	orig := discoverModelsFn
	defer func() { discoverModelsFn = orig }()
	storage := []byte(`{"accessToken":"tok","region":"cn"}`)

	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return realmTestModels("deepseek-v4.1-flash", "glm-5.2", "third-model"), nil
	}
	got := fetchDynamicModelsFromStorage(storage)
	if len(got) != 3 {
		t.Fatalf("first discovery must seed the cache: %d", len(got))
	}

	// age the cache entry past its TTL so the next query re-runs discovery
	// (a fresh TTL hit would short-circuit correctly and never see the fault).
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.realms["cn"]
	entry.fetched = time.Now().Add(-dynamicModelsCacheTTL - time.Minute)
	dynamicModelsCache.realms["cn"] = entry
	dynamicModelsCache.Unlock()

	// transient failure: the stale-but-real list must survive, NOT collapse
	// to the static catalog, and the diagnostics must say which answer is
	// being served.
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("models API status 503")
	}
	got = fetchDynamicModelsFromStorage(storage)
	if len(got) != 3 || got[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("transient failure must serve the last successful discovery, got %+v", discoveryIDs(got))
	}
	st := realmModelStateFor("cn")
	if st == nil || st.Source != "last discovery (transient failure)" {
		t.Fatalf("state source = %+v, want last discovery (transient failure)", st)
	}
	if !strings.Contains(st.LastError, "503") {
		t.Errorf("failure reason must be recorded, got %q", st.LastError)
	}

	// a second failure must keep serving the same list (idempotent); age the
	// entry again first, same as above.
	dynamicModelsCache.Lock()
	entry = dynamicModelsCache.realms["cn"]
	entry.fetched = time.Now().Add(-dynamicModelsCacheTTL - time.Minute)
	dynamicModelsCache.realms["cn"] = entry
	dynamicModelsCache.Unlock()
	got = fetchDynamicModelsFromStorage(storage)
	if len(got) != 3 {
		t.Fatalf("second failure must still serve last good list: %+v", discoveryIDs(got))
	}

	// recovery replaces everything
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return realmTestModels("fresh-model"), nil
	}
	got = fetchDynamicModelsFromStorage(storage)
	if len(got) != 1 || got[0].ID != "fresh-model" {
		t.Fatalf("recovery must serve fresh discovery: %+v", discoveryIDs(got))
	}
	st = realmModelStateFor("cn")
	if st == nil || st.Source != "discovery" || st.LastError != "" {
		t.Fatalf("recovered state = %+v", st)
	}
}

func TestDiscoveryFailureWithoutCacheAdvertisesNothing(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	orig := discoverModelsFn
	defer func() { discoverModelsFn = orig }()
	storage := []byte(`{"accessToken":"tok","region":"cn"}`)

	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("never worked")
	}
	got := fetchDynamicModelsFromStorage(storage)
	if len(got) != 0 {
		t.Fatalf("no-cache failure must advertise nothing (v0.9.33): %v", discoveryIDs(got))
	}
	st := realmModelStateFor("cn")
	if st == nil || st.Source != "none (discovery failed)" {
		t.Fatalf("state source = %+v", st)
	}
}
