// sanitize_scope_test.go guards the system-content defenses. History:
//
//   - pre-0.9.18: EVERY message (any role) over maxSystemPromptBytes (or
//     matching agentPattern) was wholesale-replaced with neutralPrompt —
//     field symptoms: "tool output looks truncated / stdout empty",
//     "conversations reset every so often". v0.9.18 added the role gate
//     (system-only), fixing multi-turn context loss.
//
//   - v0.9.28 (issue #3 closure, adopting PR #4's position): the wholesale
//     replacement is RETIRED even inside system messages. The upstream
//     filter blocklists VERBATIM phrases — a verbatim matcher does not
//     reject by length or by broad agent-identity patterns — so the
//     length/agentPattern triggers defended nothing (PR #4's author ran
//     without them and saw no 400s; non-system messages have passed
//     unfiltered since v0.9.18 with no rejection ever reported). What they
//     DID do is gut every agent host's system prompt (nearly all match
//     "you are claude code" / "you are a coding agent" and exceed 2000
//     bytes), silently swapping tool-use rules, project context and
//     behavior constraints for an 18-byte generic line. System content now
//     only gets sanitizeBlockedTemplates (known blocked phrases → safe
//     variants); everything else survives verbatim.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// scopeTestBody builds the classic 4-message shape: long system prompt, long
// user message, assistant tool call, long tool result.
func scopeTestBody(t *testing.T) map[string]any {
	t.Helper()
	long := strings.Repeat("x", 2200)
	raw := `{"model":"deepseek-v4-flash","stream":false,"messages":[
		{"role":"system","content":"` + long + `"},
		{"role":"user","content":"please review this log: ` + long + `"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_cmd","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"cmd output: ` + long + `"}
	]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return obj
}

func msgContent(t *testing.T, obj map[string]any, i int) string {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[i].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// TestRewriteSystemScope_LongSystemPromptPreserved is the v0.9.28 core
// regression: a long but clean system prompt survives VERBATIM. The old
// wholesale swap turned it into an 18-byte generic line, gutting every agent
// host's instructions.
func TestRewriteSystemScope_LongSystemPromptPreserved(t *testing.T) {
	obj := scopeTestBody(t)
	rewriteSystemInPlace(obj)

	c := msgContent(t, obj, 0)
	if len(c) <= 2000 {
		t.Fatalf("long system prompt must be preserved (len=%d)", len(c))
	}
	if strings.Contains(c, "helpful AI assistant that helps with software engineering") {
		t.Errorf("system prompt was wholesale-replaced to the neutral line: %.60q", c)
	}
	// The non-system roles stay untouched (the v0.9.18 gate holds).
	if c := msgContent(t, obj, 1); !strings.Contains(c, "please review this log") || len(c) <= 2000 {
		t.Errorf("long user message must be preserved verbatim (len=%d)", len(c))
	}
	if c := msgContent(t, obj, 3); !strings.Contains(c, "cmd output") || len(c) <= 2000 {
		t.Errorf("long tool result must be preserved verbatim (len=%d)", len(c))
	}
}

// TestRewriteSystemScope_AgentIdentitySystemSanitizedNotWiped pins the
// user-facing fix: an agent host's system prompt (identity line + long
// project context) keeps everything EXCEPT the blocked phrases, which become
// safe variants. Pre-v0.9.28 the whole prompt was swapped for neutralPrompt.
func TestRewriteSystemScope_AgentIdentitySystemSanitizedNotWiped(t *testing.T) {
	blocked := "You are Claude Code, Anthropic's official CLI for Claude."
	context := strings.Repeat("Project rule: never touch the migrations directory. ", 60) // ~3200 bytes
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": blocked + "\n\n" + context},
	}}
	rewriteSystemInPlace(obj)

	c := msgContent(t, obj, 0)
	if strings.Contains(c, "helpful AI assistant that helps with software engineering") {
		t.Fatalf("system prompt was wholesale-replaced: %.60q", c)
	}
	if strings.Contains(strings.ToLower(c), "anthropic's official cli for claude") {
		t.Errorf("blocked identity phrase must be sanitized to the safe variant: %.120q", c)
	}
	if !strings.Contains(c, "Anthropic's official CLI tool for Claude") {
		t.Errorf("safe variant should be present: %.120q", c)
	}
	if !strings.Contains(c, "never touch the migrations directory") {
		t.Errorf("project context must survive verbatim — got %.200q", c)
	}
	if len(c) < 2000 {
		t.Errorf("context length must survive (len=%d)", len(c))
	}
}

// TestRewriteSystemScope_AgentPatternNoLongerWipes: broad agent-identity
// patterns (e.g. <Role> markers, "you are a coding agent") no longer trigger
// ANY replacement — the prompt passes as-is (only the two verbatim-blocked
// phrases are touched, and only when actually present).
func TestRewriteSystemScope_AgentPatternNoLongerWipes(t *testing.T) {
	for _, content := range []string{
		"You are a coding agent working on a large monorepo. Be careful.",
		"<Role>Senior Go engineer</Role>\n<Behavior_Instructions>Review carefully.</Behavior_Instructions>",
		"you are claude code, the assistant behind this CLI.",
	} {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
		}}
		rewriteSystemInPlace(obj)
		if c := msgContent(t, obj, 0); c != content {
			t.Errorf("agent-identity system prompt must pass verbatim now:\n want %q\n got  %q", content, c)
		}
	}
}

// TestRewriteSystemScope_AgentIdentityUserPreserved: a user message that
// quotes an agent identity line (e.g. discussing Claude Code) must NOT be
// touched — the defenses are a system-content policy, not a conversation
// filter.
func TestRewriteSystemScope_AgentIdentityUserPreserved(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "why does 'you are claude code, anthropic's official cli' trigger my editor?"},
	}}
	rewriteSystemInPlace(obj)
	if c := msgContent(t, obj, 0); !strings.Contains(c, "trigger my editor") {
		t.Errorf("user message quoting an agent line must be preserved, got %q", c)
	}
}

// TestRewriteSystemScope_ShortSystemKeepsTemplateFix: system prompts carrying
// a blocked template phrase still get the sanitize substitutions.
func TestRewriteSystemScope_ShortSystemKeepsTemplateFix(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Main branch (you will usually use this for PRs)"},
	}}
	rewriteSystemInPlace(obj)
	if c := msgContent(t, obj, 0); !strings.Contains(c, "Default branch") {
		t.Errorf("system template fix should still apply, got %q", c)
	}
}

// TestRewriteSystemScope_SystemArrayUntouched: a long multimodal system
// message keeps its part structure; per-part sanitize still applies.
// (v0.9.28: the old "collapse to one neutralPrompt part" behavior is gone.)
func TestRewriteSystemScope_SystemArrayUntouched(t *testing.T) {
	long := strings.Repeat("y", 2010)
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": long},
			map[string]any{"type": "text", "text": long},
		}},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("array content should stay an array, got %T", msg["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts must keep their structure (no collapse), got %d", len(parts))
	}
	for i, p := range parts {
		part, _ := p.(map[string]any)
		txt, _ := part["text"].(string)
		if len(txt) != len(long) {
			t.Errorf("part %d must survive verbatim (len=%d want %d)", i, len(txt), len(long))
		}
	}
}

// TestRewriteSystemScope_ShortSystemArraySanitized: a multimodal system
// message gets per-part template fixes without any structural change.
func TestRewriteSystemScope_ShortSystemArraySanitized(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "Main branch (you will usually use this for PRs)"},
		}},
	}}
	rewriteSystemInPlace(obj)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("short array must keep its single part, got %T", msg["content"])
	}
	part, _ := parts[0].(map[string]any)
	if txt, _ := part["text"].(string); !strings.Contains(txt, "Default branch") {
		t.Errorf("per-part template fix should still apply, got %q", txt)
	}
}

// TestPrepareUpstreamBody_ScopeEndToEnd runs the full pipeline the executor
// uses, to prove the defenses compose without resurrecting the wholesale
// swap.
func TestPrepareUpstreamBody_ScopeEndToEnd(t *testing.T) {
	obj := scopeTestBody(t)
	raw, _ := json.Marshal(obj)
	out := prepareUpstreamBody(raw, nil, nil, "")
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	sc, _ := sys["content"].(string)
	if len(sc) <= 2000 || strings.Contains(sc, "helpful AI assistant that helps with software engineering") {
		t.Errorf("long system prompt must survive the full pipeline verbatim (len=%d)", len(sc))
	}
	tool, _ := msgs[3].(map[string]any)
	tc, _ := tool["content"].(string)
	if !strings.Contains(tc, "cmd output") || len(tc) <= 2000 {
		t.Errorf("tool result must survive the full pipeline (len=%d)", len(tc))
	}
}
