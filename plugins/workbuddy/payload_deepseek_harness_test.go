package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// [self 0.9.63] Upstream 0.9.62 (PR #46) + 0.9.63 (PR #49): Tencent's CN
// gateway answers 11128 "Illegal API invocation from an unapproved channel"
// for the DeepSeek Harness phrase family. These tests pin the exact triggers,
// the near-misses that must survive, and the all-role scope.
func TestSanitizeBlockedTemplates_DeepSeekHarness(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"intro verbatim",
			"You are an AI agent powered by DeepSeek Harness. Follow the rules.",
			"You are an AI assistant. Follow the rules.",
		},
		{
			"intro lowercase",
			"you are an ai agent powered by deepseek harness.",
			"You are an AI assistant.",
		},
		{
			"web gui verbatim",
			"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names.",
			"You are interacting with the user through the DeepSeek Harness web interface whose URL the runtime context names.",
		},
		{
			"web gui extra whitespace",
			"through the deepseek   harness web\ngui.",
			"through the DeepSeek Harness web interface.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tc.in); got != tc.want {
				t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Replay-verified near misses pass byte-identical: the gateway matches the
// verbatim phrase, not the brand words on their own.
func TestSanitizeBlockedTemplates_DeepSeekHarnessKeepsNearMisses(t *testing.T) {
	for _, s := range []string{
		"DeepSeek Harness",
		"powered by DeepSeek Harness",
		"You are an AI agent powered by DeepSeek.",
		"You are an AI agent powered by Acme Harness.",
		"DeepSeek Harness Site GUI",
		"DeepSeek Harness Web UI",
		"Harness Web GUI",
		"Web GUI alone",
		"DeepSeek Harness web interface",
	} {
		if got := sanitizeBlockedTemplates(s); got != s {
			t.Fatalf("near miss %q was rewritten to %q", s, got)
		}
	}
}

// The history pass covers every role: assistant echoes, user pastes, tool
// results, tool_calls arguments and assistant reasoning mirrors.
func TestSanitizeHistoryPhrasesInPlace_EveryRole(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness."},
			map[string]any{"role": "user", "content": "context: interacting through the DeepSeek Harness Web GUI today."},
			map[string]any{"role": "assistant", "content": "summary:\n- the DeepSeek Harness Web GUI URL\n- Main branch (you will usually use this for PRs)\nDone."},
			map[string]any{"role": "tool", "content": "tool output mentioning DeepSeek Harness Web GUI"},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "part text: DeepSeek Harness Web GUI"},
				},
				"tool_calls": []any{
					map[string]any{"function": map[string]any{
						"name":      "write",
						"arguments": `{"note":"system said: DeepSeek Harness Web GUI"}`,
					}},
				},
				"reasoning_content": "reasoning mentions DeepSeek Harness Web GUI",
				"reasoning":         "mirror mentions DeepSeek Harness Web GUI",
			},
		},
	}
	if !sanitizeHistoryPhrasesInPlace(obj) {
		t.Fatal("sanitizeHistoryPhrasesInPlace reported no change")
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(string(raw))
	for _, bad := range []string{
		"you are an ai agent powered by deepseek harness",
		"deepseek harness web gui",
		"main branch (you will usually use this for prs)",
	} {
		if strings.Contains(low, bad) {
			t.Fatalf("blocked phrase %q survived: %s", bad, raw)
		}
	}
	if !strings.Contains(string(raw), "You are an AI assistant.") {
		t.Fatalf("intro rewrite missing: %s", raw)
	}
	if !strings.Contains(string(raw), "DeepSeek Harness web interface") {
		t.Fatalf("web-gui rewrite missing: %s", raw)
	}
}

// Content without a trigger phrase must pass byte-identical: the guard against
// re-introducing the retired v0.9.18 wholesale rewrite.
func TestSanitizeHistoryPhrasesInPlace_NoMatchIsByteIdentical(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "plain question"},
			map[string]any{"role": "assistant", "content": "plain answer"},
		},
	}
	before, _ := json.Marshal(obj)
	if sanitizeHistoryPhrasesInPlace(obj) {
		t.Fatal("no-match body reported a change")
	}
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatalf("no-match body changed:\n before=%s\n after=%s", before, after)
	}
}

// The end-to-end order matters: reasoning is scrubbed before the fold, so the
// <thought> block injected from reasoning_content must not re-arm a phrase.
func TestPrepareUpstreamBody_DeepSeekHarnessReasoningFoldDoesNotRearm(t *testing.T) {
	payload := []byte(`{
		"model":"glm-5.3-flash",
		"messages":[
			{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},
			{"role":"assistant","content":"answer","reasoning_content":"thought via DeepSeek Harness Web GUI"}
		]
	}`)
	out := prepareUpstreamBody(payload, nil, &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, "glm-5.3-flash")
	low := strings.ToLower(string(out))
	if strings.Contains(low, "you are an ai agent powered by deepseek harness") {
		t.Fatalf("intro phrase survived end-to-end: %s", out)
	}
	if strings.Contains(low, "deepseek harness web gui") {
		t.Fatalf("web-gui phrase re-armed by the reasoning fold: %s", out)
	}
	// JSON escapes "<thought>" as \u003cthought\u003e, so parse before asserting.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("prepared body is not JSON: %v", err)
	}
	msgs, _ := parsed["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("messages missing after prepare: %s", out)
	}
	assistant, _ := msgs[1].(map[string]any)
	content, _ := assistant["content"].(string)
	if !strings.Contains(content, "<thought>") {
		t.Fatalf("reasoning fold did not run: %s", content)
	}
}
