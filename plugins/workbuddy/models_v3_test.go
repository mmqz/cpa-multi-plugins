package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNonChatModelMatrix(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		maxOut int64
		tags   []string
		extra  bool
		wantNC bool
	}{
		{"plain chat", "deepseek-v4.1-flash", 8192, nil, false, false},
		{"nes prefix", "nes-inline-completion", 4096, nil, false, true},
		{"completion prefix", "completion-default", 4096, nil, false, true},
		{"codewise prefix", "codewise-default-model-v2", 4096, nil, false, true},
		{"supportsExtra marker", "codewise-thing", 4096, nil, true, true},
		{"tiny output", "some-tiny-model", 256, nil, false, true},
		{"tiny output boundary above", "some-tiny-model", 257, nil, false, false},
		{"zero output tolerated", "some-model", 0, nil, false, false},
		{"text-to-image tag", "hunyuan-image-alpha", 4096, []string{"text-to-image"}, false, true},
		{"tag case-insensitive", "img-model", 4096, []string{"Text-To-Image"}, false, true},
		{"unrelated tag", "chat-model", 4096, []string{"reasoning"}, false, false},
		{"id case-insensitive", "NES-Foo", 4096, nil, false, true},
	}
	for _, tc := range cases {
		if got := nonChatModel(tc.id, tc.maxOut, tc.tags, tc.extra); got != tc.wantNC {
			t.Errorf("%s: nonChatModel(%q,%d,%v,%v) = %v, want %v",
				tc.name, tc.id, tc.maxOut, tc.tags, tc.extra, got, tc.wantNC)
		}
	}
}

// TestPromotionExcludesNonChatModels locks the v0.9.12 fix: the v0.9.8
// promotion path would surface ANY enabled data.models entry — including
// completion/text-to-image entries that upstream rejects with 11102/11133.
func TestPromotionExcludesNonChatModels(t *testing.T) {
	models := []discoveredModel{
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash"},
		{ID: "nes-completion-foo", Name: "NES Foo", MaxTokens: json.RawMessage(`4096`)},
		{ID: "hunyuan-image-alpha", Name: "Hunyuan Image", Tags: []string{"text-to-image"}},
		{ID: "completion-tiny", Name: "Tiny", MaxOutputTokens: json.RawMessage(`256`)},
	}
	got := modelsFromDiscovery(models, nil)
	if len(got) != 1 || got[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("only the chat model may be promoted, got %v", discoveryIDs(got))
	}
}

func TestDiscoverToInfoCapabilities(t *testing.T) {
	// v3 generation field names + vision + effort levels.
	m := discoveredModel{
		ID:              "hy4-preview",
		Name:            "Hy4 Preview",
		MaxInputTokens:  json.RawMessage(`1000000`),
		MaxOutputTokens: json.RawMessage(`393216`),
		SupportsImages:  true,
		Reasoning:       json.RawMessage(`{"supportedEfforts":["low","high","max"],"canDisableThinking":true}`),
	}
	info := discoverToInfo(m)
	if info.ContextLength != 1000000 || info.InputTokenLimit != 1000000 {
		t.Errorf("input tokens not mapped: %+v", info)
	}
	if info.MaxCompletionTokens != 393216 || info.OutputTokenLimit != 393216 {
		t.Errorf("output tokens not mapped: %+v", info)
	}
	if len(info.SupportedInputModalities) != 2 ||
		info.SupportedInputModalities[0] != "text" || info.SupportedInputModalities[1] != "image" {
		t.Errorf("image modality not advertised: %v", info.SupportedInputModalities)
	}
	if info.Thinking == nil || len(info.Thinking.Levels) != 3 || !info.Thinking.ZeroAllowed {
		t.Errorf("thinking levels not mapped: %+v", info.Thinking)
	}

	// Undeclared modality → nothing advertised (never fabricate capabilities).
	unknown := discoveredModel{ID: "m3"}
	if got := discoverToInfo(unknown); got.SupportedInputModalities != nil {
		t.Errorf("undeclared modality must be omitted, got %v", got.SupportedInputModalities)
	}

	// disabledMultimodal suppresses the image advertisement (v0.9.11 rule).
	switchedOff := discoveredModel{ID: "m5", SupportsImages: true, DisabledMultimodal: true}
	if got := discoverToInfo(switchedOff); got.SupportedInputModalities != nil {
		t.Errorf("disabledMultimodal must suppress advertisement, got %v", got.SupportedInputModalities)
	}

	// Enterprise generation field names fall through when v3 names are absent.
	ent := discoveredModel{ID: "m4", ContextWindow: json.RawMessage(`262144`), MaxTokens: json.RawMessage(`8192`)}
	got := discoverToInfo(ent)
	if got.ContextLength != 262144 || got.MaxCompletionTokens != 8192 {
		t.Errorf("enterprise field names must be honored: %+v", got)
	}
}

func TestMergeDiscoveryListsOverlayAndExtras(t *testing.T) {
	enterprise := []pluginapi.ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1000000, MaxCompletionTokens: 8192},
		{ID: "kimi-k2.7", Name: "Kimi K2.7"},
	}
	v3 := []discoveredModel{
		// Capability overlay: kimi gains image support + ctx from v3.
		{ID: "kimi-k2.7", Name: "Kimi K2.7", MaxInputTokens: json.RawMessage(`262144`),
			SupportsImages: true},
		// v3-only family model appended after the enterprise block.
		{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex", MaxInputTokens: json.RawMessage(`400000`)},
		// Non-chat v3 entries never append.
		{ID: "nes-noise", Name: "Noise", MaxOutputTokens: json.RawMessage(`128`)},
	}
	got := mergeDiscoveryLists(enterprise, v3)
	if len(got) != 3 {
		t.Fatalf("expected 2 enterprise + 1 v3-only, got %v", discoveryIDs(got))
	}
	// Enterprise order preserved.
	if got[0].ID != "glm-5.2" || got[1].ID != "kimi-k2.7" || got[2].ID != "gpt-5.3-codex" {
		t.Fatalf("merge order broken: %v", discoveryIDs(got))
	}
	// glm-5.2 keeps its live enterprise ctx (never downgraded by v3).
	if got[0].ContextLength != 1000000 {
		t.Errorf("enterprise ctx must win when present: %+v", got[0])
	}
	// kimi gains ctx + image modality from v3.
	if got[1].ContextLength != 262144 {
		t.Errorf("v3 ctx must fill enterprise gaps: %+v", got[1])
	}
	if len(got[1].SupportedInputModalities) != 2 {
		t.Errorf("v3 image flag must overlay: %v", got[1].SupportedInputModalities)
	}
	// Empty v3 → enterprise verbatim.
	if got := mergeDiscoveryLists(enterprise, nil); len(got) != 2 {
		t.Fatalf("enterprise-only passthrough broken: %v", discoveryIDs(got))
	}
}

func TestExtractAccountUID(t *testing.T) {
	nested := []byte(`{"account":{"uid":" e5fd6787-6764-42cb-b8f0-309fb094ca12 "},"auth":{"accessToken":"tok"}}`)
	if got := extractAccountUID(nested); got != "e5fd6787-6764-42cb-b8f0-309fb094ca12" {
		t.Errorf("nested uid = %q", got)
	}
	flat := []byte(`{"uid":"uid-flat","accessToken":"tok"}`)
	if got := extractAccountUID(flat); got != "uid-flat" {
		t.Errorf("flat uid = %q", got)
	}
	if got := extractAccountUID([]byte(`{"accessToken":"tok"}`)); got != "" {
		t.Errorf("missing uid must be empty, got %q", got)
	}
}

func TestV3ConfigEndpointsPerRealm(t *testing.T) {
	if got := v3ConfigEndpointFor("cn"); got != "https://copilot.tencent.com/v3/config" {
		t.Errorf("cn v3 endpoint = %q", got)
	}
	if got := v3ConfigEndpointFor("intl"); got != "https://www.codebuddy.ai/v3/config" {
		t.Errorf("intl v3 endpoint = %q", got)
	}
	if got := v3ConfigEndpointFor("global"); got != "https://www.workbuddy.ai/v3/config" {
		t.Errorf("global v3 endpoint = %q", got)
	}
	if got := v3ConfigDomainFor("intl"); got != "www.codebuddy.ai" {
		t.Errorf("intl v3 domain = %q", got)
	}
	if got := v3ConfigDomainFor("cn"); got != "copilot.tencent.com" {
		t.Errorf("cn v3 domain = %q", got)
	}
	if !strings.Contains(v3ConfigUA, "CodeBuddyIDE/") {
		t.Errorf("v3 UA must be the IDE shape, got %q", v3ConfigUA)
	}
}
