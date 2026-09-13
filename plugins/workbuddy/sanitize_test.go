package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestRewriteSystemInPlace_PreservesUserAndAssistantMessages(t *testing.T) {
	longUserContent := "User detailed question: " + strings.Repeat("A", 3000)
	longAssistantContent := "Previous assistant answer with code: " + strings.Repeat("B", 3000)
	longSystemContent := "You are Claude Code, Anthropic's official CLI for Claude. " + strings.Repeat("Rule: be accurate. ", 200)

	obj := map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": longSystemContent},
			map[string]any{"role": "user", "content": longUserContent},
			map[string]any{"role": "assistant", "content": longAssistantContent},
			map[string]any{"role": "user", "content": "Follow up question"},
		},
	}

	rewriteSystemInPlace(obj)

	messages := obj["messages"].([]any)
	sysMsg := messages[0].(map[string]any)
	userMsg1 := messages[1].(map[string]any)
	asstMsg := messages[2].(map[string]any)
	userMsg2 := messages[3].(map[string]any)

	// User messages must be 100% untouched
	if userMsg1["content"] != longUserContent {
		t.Fatalf("userMsg1 content was modified! got length %d want %d", len(userMsg1["content"].(string)), len(longUserContent))
	}
	if userMsg2["content"] != "Follow up question" {
		t.Fatalf("userMsg2 content was modified!")
	}

	// Assistant message must be 100% untouched (no context loss!)
	if asstMsg["content"] != longAssistantContent {
		t.Fatalf("assistant content was modified! Context was lost! got length %d want %d", len(asstMsg["content"].(string)), len(longAssistantContent))
	}

	// System message must have its template sanitized, but must NOT be replaced with neutralPrompt
	sysContent := sysMsg["content"].(string)
	if sysContent == neutralPrompt {
		t.Fatalf("system message was wiped out and replaced with neutralPrompt!")
	}
	if !strings.Contains(sysContent, "You are Claude Code, Anthropic's official CLI tool for Claude.") {
		t.Fatalf("system message blocked template was not sanitized correctly: %q", sysContent[:100])
	}
	if !strings.Contains(sysContent, "Rule: be accurate.") {
		t.Fatalf("system message rules were lost!")
	}
}

func TestRewriteSystemForUpstream_PreservesContext(t *testing.T) {
	longAssistantContent := "Assistant code: " + strings.Repeat("X", 2500)
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"` + longAssistantContent + `"},{"role":"user","content":"Next"}]}`)

	out := rewriteSystemForUpstream(payload)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	messages := obj["messages"].([]any)
	asst := messages[1].(map[string]any)
	if asst["content"] != longAssistantContent {
		t.Fatalf("rewriteSystemForUpstream modified assistant message! Context lost!")
	}
}
