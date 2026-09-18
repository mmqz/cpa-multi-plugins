package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizeRolesFlipsDeveloperToSystem locks the 11128 fix: deepseek-harness
// and other OpenAI-compat agents put the system instruction in a "developer"
// role message, which the workbuddy upstream rejects with 11128 "Illegal API
// invocation from an unapproved channel". The rewrite must flip that role to
// "system" and leave every other role untouched.
func TestNormalizeRolesFlipsDeveloperToSystem(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "developer", "content": "You are a coding agent."},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
	}
	if !normalizeRolesInPlace(obj) {
		t.Fatal("expected developer role to be rewritten")
	}
	messages := obj["messages"].([]any)
	roles := []string{}
	for _, m := range messages {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	want := []string{"system", "user", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v want %v", roles, want)
		}
	}
}

// TestNormalizeRolesNoopWithoutDeveloper: a payload with no developer role must
// be reported as unchanged (callers use the bool to skip a re-marshal).
func TestNormalizeRolesNoopWithoutDeveloper(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if normalizeRolesInPlace(obj) {
		t.Fatal("no developer role must not report a change")
	}
}

// TestPrepareUpstreamBodyRewritesDeveloperRole exercises the full single-pass
// body rewrite the executor uses: a dsh-style body with a developer-role system
// prompt must come out with role=system and stream=true.
func TestPrepareUpstreamBodyRewritesDeveloperRole(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","stream":false,"messages":[{"role":"developer","content":"You are an AI agent."},{"role":"user","content":"1+1?"}]}`)
	out := prepareUpstreamBody(in, nil, nil, "deepseek-v4.1-flash")
	if len(out) == 0 {
		t.Fatal("body must not be empty")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if s, _ := obj["stream"].(bool); !s {
		t.Fatal("stream must be forced true")
	}
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if r, _ := first["role"].(string); r != "system" {
		t.Fatalf("developer role must be rewritten to system, got %q", r)
	}
}

// TestInjectReasoningFoldsIntoContent locks issue #5: a prior assistant turn's
// reasoning_content must be inlined as a <thought> block so the upstream (which
// drops the non-standard field) still carries the chain-of-thought forward.
func TestInjectReasoningFoldsIntoContent(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "find the bug"},
			map[string]any{"role": "assistant", "content": "Found it: off-by-one.", "reasoning_content": "checking loop bounds"},
			map[string]any{"role": "user", "content": "fix it"},
		},
	}
	if !injectReasoningInPlace(obj) {
		t.Fatal("expected reasoning to be injected")
	}
	msgs := obj["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	c, _ := assistant["content"].(string)
	if !strings.HasPrefix(c, "<thought>") || !strings.Contains(c, "checking loop bounds") || !strings.Contains(c, "Found it: off-by-one") {
		t.Fatalf("assistant content must keep thought block + original text, got %q", c)
	}
}

// TestInjectReasoningMultimodalAndIdempotent: array-of-parts content gets a text
// part prepended with the thought block; a first part already carrying
// <thought> is left untouched (idempotent).
func TestInjectReasoningMultimodalAndIdempotent(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{
				"role":              "assistant",
				"content":           []any{map[string]any{"type": "text", "text": "<thought>\nalready\n</thought>\n\nanswer"}},
				"reasoning_content": "already injected",
			},
		},
	}
	if injectReasoningInPlace(obj) {
		t.Fatal("must not re-inject when content already starts with <thought>")
	}
	// Multimodal without a thought block should get one prepended.
	obj2 := map[string]any{
		"messages": []any{
			map[string]any{
				"role":              "assistant",
				"content":           []any{map[string]any{"type": "text", "text": "answer"}},
				"reasoning_content": "chain here",
			},
		},
	}
	if !injectReasoningInPlace(obj2) {
		t.Fatal("expected multimodal injection")
	}
	msgs := obj2["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	firstPart := parts[0].(map[string]any)
	txt, _ := firstPart["text"].(string)
	if !strings.HasPrefix(txt, "<thought>") {
		t.Fatalf("first part must be the thought block, got %q", txt)
	}
	if len(parts) != 2 {
		t.Fatalf("thought part must be prepended before original, got %d parts", len(parts))
	}
}

// TestInjectReasoningNoopWithoutAssistantHistory: no assistant message with
// reasoning → nothing changes.
func TestInjectReasoningNoopWithoutAssistantHistory(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	if injectReasoningInPlace(obj) {
		t.Fatal("must not inject without assistant history")
	}
}

// TestRewriteSystemOnlyTouchesSystemRole locks issue #3: only system-role
// messages are rewritten; long user/assistant history — the multi-turn context
// — passes through untouched even when well over 2000 bytes.
func TestRewriteSystemOnlyTouchesSystemRole(t *testing.T) {
	long := strings.Repeat("x", 5000)
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude."},
			map[string]any{"role": "user", "content": long},
			map[string]any{"role": "assistant", "content": "Here is the long result " + long},
		},
	}
	rewriteSystemInPlace(obj)
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if c, _ := sys["content"].(string); strings.Contains(c, "Anthropic's official CLI for Claude") {
		t.Fatalf("system agent identity must be neutralized for WAF, got %q", c)
	}
	user := msgs[1].(map[string]any)
	if c, _ := user["content"].(string); c != long {
		t.Fatalf("long user content must NOT be rewritten (issue#3), got len=%d", len(c))
	}
	assistant := msgs[2].(map[string]any)
	if c, _ := assistant["content"].(string); c != "Here is the long result "+long {
		t.Fatalf("long assistant content must NOT be rewritten (issue#3)")
	}
}

// TestSanitizeLongSystemPromptKeepsMeaning: even a system prompt longer than
// the old 2000-byte cutoff is only template-substituted, not wholesale wiped.
func TestSanitizeLongSystemPromptKeepsMeaning(t *testing.T) {
	long := strings.Repeat("y", 3000)
	out := sanitizeContentText(long)
	if out != long {
		t.Fatalf("long system prompt must not be replaced by neutralPrompt (issue#3); got len %d", len(out))
	}
}
