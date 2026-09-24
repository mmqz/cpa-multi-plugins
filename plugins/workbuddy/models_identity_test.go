package main

import (
	"context"
	"encoding/json"
	"testing"
)

// --- identity-split /v3/config discovery (0.9.35) ---
//
// The gateway serves a different catalog per client identity (measured
// 2026-09-22 on workbuddy.ai by workbuddy2api-panel: IDE UA → 10 chat with
// o4-mini/enhance-1.0/auto-chat and no deepseek series; CLI UA → 22 chat
// with deepseek-v4.1-flash/-sg, gpt-6-astra, kimi-k2.8-preview and none of
// those aliases). issue #17: the plugin only ever presented the IDE
// identity, so an Intl credential showed 8 models while the CLI roster was
// invisible. These tests pin the union machinery.

func TestV3ProbeUAsFor(t *testing.T) {
	cases := map[string]int{"global": 2, "intl": 2, "cn": 1, "": 1, "unknown": 1}
	for realm, want := range cases {
		if got := len(v3ProbeUAsFor(realm)); got != want {
			t.Errorf("realm=%q: %d UAs, want %d", realm, got, want)
		}
	}
	uas := v3ProbeUAsFor("intl")
	if uas[0] != v3ConfigUA || uas[1] != v3ConfigCLIUA {
		t.Errorf("intl probe order %v — the IDE identity must be first (field authority)", uas)
	}
	if cn := v3ProbeUAsFor("cn"); len(cn) != 1 || cn[0] != v3ConfigUA {
		t.Errorf("cn must stay IDE-UA single-probe, got %v", cn)
	}
}

func TestBuildV3ConfigRequestIdentity(t *testing.T) {
	ide, err := buildV3ConfigRequest(context.Background(), "https://x/v3/config", "dom.example", "tok", "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ide.Header.Get("User-Agent"); got != v3ConfigUA {
		t.Errorf("empty ua must default to the IDE identity, got %q", got)
	}
	// The IDE identity must stay header-identical to the pre-0.9.35 probe:
	// no X-IDE-Type/Name/Version injection, X-Domain preserved.
	if ide.Header.Get("X-IDE-Type") != "" || ide.Header.Get("X-IDE-Name") != "" {
		t.Errorf("IDE identity grew X-IDE-* headers: %v", ide.Header)
	}
	if got := ide.Header.Get("X-Domain"); got != "dom.example" {
		t.Errorf("X-Domain = %q, want dom.example", got)
	}
	if got := ide.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if got := ide.Header.Get("X-User-Id"); got != "u1" {
		t.Errorf("X-User-Id = %q, want u1", got)
	}

	cli, err := buildV3ConfigRequest(context.Background(), "https://x/v3/config", "dom.example", "tok", "", v3ConfigCLIUA)
	if err != nil {
		t.Fatal(err)
	}
	if got := cli.Header.Get("User-Agent"); got != v3ConfigCLIUA {
		t.Errorf("CLI identity UA = %q, want %q", got, v3ConfigCLIUA)
	}
	if got := cli.Header.Get("X-IDE-Type"); got != "CLI" {
		t.Errorf("CLI identity X-IDE-Type = %q, want CLI", got)
	}
	if got := cli.Header.Get("X-IDE-Name"); got != "CLI" {
		t.Errorf("CLI identity X-IDE-Name = %q, want CLI", got)
	}
	if got := cli.Header.Get("X-IDE-Version"); got == "" {
		t.Error("CLI identity must carry X-IDE-Version")
	}
	if got := cli.Header.Get("X-User-Id"); got != "" {
		t.Errorf("empty uid must omit X-User-Id, got %q", got)
	}
}

func TestParseV3ConfigModels(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"fast-model","maxOutputTokens":393216},
		{"id":"deepseek-v4.1-flash","maxInputTokens":1000000},
		{"id":"nes-embed","maxOutputTokens":128},
		{"id":"codewise-fill","supportsExtra":true},
		{"id":"img-gen","tags":["text-to-image"]},
		{"id":"retired","disabled":true},
		{"id":""}]}}`)
	got, err := parseV3ConfigModels(body)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if len(got) != 2 || !ids["fast-model"] || !ids["deepseek-v4.1-flash"] {
		t.Errorf("parse kept %v, want exactly fast-model + deepseek-v4.1-flash", ids)
	}

	if _, err := parseV3ConfigModels([]byte(`{"code":11133}`)); err == nil {
		t.Error("non-zero envelope code must error")
	}
	if _, err := parseV3ConfigModels([]byte(`not-json`)); err == nil {
		t.Error("garbage body must error")
	}
	if _, err := parseV3ConfigModels([]byte(`{"code":0,"data":{"models":[]}}`)); err == nil {
		t.Error("empty roster must error")
	}
}

func TestMergeV3IdentityLists(t *testing.T) {
	ide := []discoveredModel{
		{ID: "fast-model", MaxOutputTokens: json.RawMessage(`393216`)},
		{ID: "o4-mini"},
	}
	cli := []discoveredModel{
		{ID: "deepseek-v4.1-flash"},
		{ID: "FAST-MODEL", MaxOutputTokens: json.RawMessage(`1`)}, // case-insensitive dup — IDE fields win
		{ID: "o4-mini"}, // exact dup — dropped
		{ID: "gpt-6-astra"},
	}
	got := mergeV3IdentityLists(ide, cli)
	var ids []string
	byID := map[string]discoveredModel{}
	for _, m := range got {
		ids = append(ids, m.ID)
		byID[m.ID] = m
	}
	if len(ids) != 4 || ids[0] != "fast-model" || ids[1] != "o4-mini" {
		t.Errorf("merge order/authority broken: %v — IDE roster must lead", ids)
	}
	if _, ok := byID["deepseek-v4.1-flash"]; !ok {
		t.Error("CLI-roster-only id must be appended")
	}
	if _, ok := byID["gpt-6-astra"]; !ok {
		t.Error("CLI-roster-only id must be appended")
	}
	if string(byID["fast-model"].MaxOutputTokens) != "393216" {
		t.Errorf("IDE entry fields must win, got %s", byID["fast-model"].MaxOutputTokens)
	}
	if got := mergeV3IdentityLists(ide, nil); len(got) != 2 {
		t.Errorf("nil extra must pass base through, got %d", len(got))
	}
	if got := mergeV3IdentityLists(nil, nil); got != nil {
		t.Errorf("both empty must be nil, got %v", got)
	}
}

func TestEnterpriseEndpointCandidates(t *testing.T) {
	urls, origin := enterpriseEndpointCandidates("global")
	if len(urls) != 2 ||
		urls[0] != "https://www.workbuddy.ai/v2/enterprises/personal/models" ||
		urls[1] != "https://www.workbuddy.ai/console/enterprises/personal/models" {
		t.Errorf("global candidates = %v, want [/v2, /console] on workbuddy.ai", urls)
	}
	if origin != "https://www.workbuddy.ai" {
		t.Errorf("global origin = %q", origin)
	}
	urls, origin = enterpriseEndpointCandidates("intl")
	if len(urls) != 2 ||
		urls[0] != "https://www.codebuddy.ai/v2/enterprises/personal/models" ||
		urls[1] != "https://www.codebuddy.ai/console/enterprises/personal/models" {
		t.Errorf("intl candidates = %v, want [/v2, /console] on codebuddy.ai", urls)
	}
	if origin != "https://www.codebuddy.ai" {
		t.Errorf("intl origin = %q", origin)
	}
	// cn keeps its measured /console-only sequence.
	urls, _ = enterpriseEndpointCandidates("cn")
	if len(urls) != 1 || urls[0] != "https://copilot.tencent.com/console/enterprises/personal/models" {
		t.Errorf("cn candidates = %v, want /console only", urls)
	}
}
