package main

import "testing"

func TestSanitizeBlockedTemplates_ClaudeCode(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace blocked template")
	}
	want := "You are Claude Code, Anthropic's official CLI tool for Claude."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestSanitizeBlockedTemplates_MainBranch(t *testing.T) {
	in := "Main branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace Main branch")
	}
}

func TestSanitizeBlockedTemplates_NoMatch(t *testing.T) {
	in := "Hello world"
	out := sanitizeBlockedTemplates(in)
	if out != in {
		t.Fatal("should pass through unchanged")
	}
}

func TestForceMaxThinking_Hy3Model(t *testing.T) {
	obj := map[string]any{"model": "hy3-std"}
	changed := forceMaxThinking(obj)
	if !changed {
		t.Fatal("should change hy3 model")
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatal("should set high")
	}
}

func TestForceMaxThinking_Hy4Model(t *testing.T) {
	obj := map[string]any{"model": "hy4-preview"}
	changed := forceMaxThinking(obj)
	if !changed {
		t.Fatal("should change hy4 model")
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatal("should set high")
	}
}

func TestForceMaxThinking_Hy4ModelCaseInsensitive(t *testing.T) {
	obj := map[string]any{"model": "Hy4 Preview"}
	if !forceMaxThinking(obj) {
		t.Fatal("should match Hy4 case-insensitively (pre-rewrite alias form)")
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatal("should set high")
	}
}

func TestForceMaxThinking_NonHy3Model(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change non-hy3 model")
	}
}

func TestForceMaxThinking_AlreadyHigh(t *testing.T) {
	obj := map[string]any{"model": "hy3-std", "reasoning_effort": "high"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change when already high")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}

// TestSanitizeBlockedTemplates_CodexCLIIntro pins the issue #31 fix: the
// Codex CLI base-instructions opening signature — Tencent gateway channel
// risk-control 11128 ("Illegal API invocation from an unapproved channel") —
// is replaced by a generic line, with surrounding prompt text preserved.
func TestSanitizeBlockedTemplates_CodexCLIIntro(t *testing.T) {
	intro := "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	out := sanitizeBlockedTemplates(intro)
	want := "You are a helpful coding assistant running in the terminal. You are expected to be precise, safe, and helpful."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

// TestSanitizeBlockedTemplates_CodexCLIIntroVariants covers tolerant matching
// across Codex release drift: case differences and a first sentence that
// ends without the descriptive clause.
func TestSanitizeBlockedTemplates_CodexCLIIntroVariants(t *testing.T) {
	variants := []string{
		"You are a CODING AGENT running in the Codex CLI. Codex CLI is an open source project led by OpenAI.",
		"you are a coding agent running in the codex cli, a terminal-based coding assistant.  Codex CLI is an open source project led by OpenAI.",
	}
	for _, in := range variants {
		out := sanitizeBlockedTemplates(in)
		if out != "You are a helpful coding assistant running in the terminal." {
			t.Fatalf("variant not sanitized: %q -> %q", in, out)
		}
	}
}

// TestRewriteSystemMessagesInPlace_CodexRoleGate pins the scope contract:
// the Codex intro is sanitized in system messages only — user / developer
// content passes verbatim (v0.9.18 lesson: never rewrite non-system roles).
func TestRewriteSystemMessagesInPlace_CodexRoleGate(t *testing.T) {
	intro := "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI."
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": intro},
			map[string]any{"role": "system", "content": "preamble. " + intro + " tail"},
		},
	}
	if !rewriteSystemMessagesInPlace(obj) {
		t.Fatal("expected system message rewrite")
	}
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != intro {
		t.Fatalf("user content must stay verbatim, got %q", got)
	}
	sys := msgs[1].(map[string]any)["content"].(string)
	if sys != "preamble. You are a helpful coding assistant running in the terminal. tail" {
		t.Fatalf("system content not sanitized as expected: %q", sys)
	}
}
