package main

import (
	"encoding/json"
	"testing"
)

// [self 0.9.63] Upstream 0.9.59 (issue #40, PR #42): Intl validates
// messages[0].role == "system" exactly like Global. CN stays untouched.
func TestEnsureSystemMessageInPlace_RealmGates(t *testing.T) {
	cases := []struct {
		name    string
		domain  string
		wantAdd bool
	}{
		{"cn untouched", "www.codebuddy.cn", false},
		{"cn legacy empty domain untouched", "", false},
		{"global injected", "www.workbuddy.ai", true},
		{"intl injected", "www.codebuddy.ai", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			}}
			got := ensureSystemMessageInPlace(obj, &storedAuth{Auth: storedTokens{Domain: tc.domain}})
			if got != tc.wantAdd {
				t.Fatalf("ensureSystemMessageInPlace = %v, want %v", got, tc.wantAdd)
			}
			msgs := obj["messages"].([]any)
			if tc.wantAdd {
				if len(msgs) != 2 {
					t.Fatalf("messages = %d, want 2", len(msgs))
				}
				first := msgs[0].(map[string]any)
				if first["role"] != "system" {
					t.Fatalf("first role = %v, want system", first["role"])
				}
			} else if len(msgs) != 1 {
				t.Fatalf("messages = %d, want CN unchanged 1", len(msgs))
			}
		})
	}
}

// An existing leading system message must suppress the injection on both
// Global and Intl.
func TestEnsureSystemMessageInPlace_ExistingSystemNotDuplicated(t *testing.T) {
	for _, domain := range []string{"www.workbuddy.ai", "www.codebuddy.ai"} {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "keep me"},
			map[string]any{"role": "user", "content": "hi"},
		}}
		if ensureSystemMessageInPlace(obj, &storedAuth{Auth: storedTokens{Domain: domain}}) {
			t.Fatalf("%s: existing leading system message was duplicated", domain)
		}
		if msgs := obj["messages"].([]any); len(msgs) != 2 {
			t.Fatalf("%s: messages = %d, want 2", domain, len(msgs))
		}
	}
}

// The byte-level helper must follow the same gate, since it is still used by
// the legacy helpers and tests.
func TestEnsureSystemMessage_RealmGates(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	for _, tc := range []struct {
		domain string
		want   bool
	}{
		{"www.codebuddy.cn", false},
		{"www.workbuddy.ai", true},
		{"www.codebuddy.ai", true},
	} {
		out := ensureSystemMessage(payload, &storedAuth{Auth: storedTokens{Domain: tc.domain}})
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("%s: invalid JSON: %v", tc.domain, err)
		}
		msgs := obj["messages"].([]any)
		if got := len(msgs) == 2; got != tc.want {
			t.Fatalf("%s: injected=%v, want %v", tc.domain, got, tc.want)
		}
	}
}
