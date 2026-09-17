package main

import (
	"encoding/json"
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
