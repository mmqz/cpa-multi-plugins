package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// issue #20 regression: the Global (workbuddy.ai) realm validates
// messages[0] — a payload whose FIRST message is not a system prompt is
// rejected upstream with 11128. ensureSystemMessageInPlace must therefore
// inject whenever the first message is not system, even when a system
// message sits mid-history (the old any-position scan skipped it).

func issue20GlobalSA() *storedAuth {
	return &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
}

func issue20Roles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("messages: %v", err)
	}
	roles := make([]string, 0, len(obj.Messages))
	for _, m := range obj.Messages {
		roles = append(roles, m.Role)
	}
	return roles
}

func TestEnsureSystemFirstInjectsWhenFirstIsUser(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}}
	if !ensureSystemMessageInPlace(obj, issue20GlobalSA()) {
		t.Fatal("injection expected when messages[0] is user")
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); !strings.EqualFold(role, "system") {
		t.Fatalf("first role = %q, want system", role)
	}
}

// THE issue #20 case: a mid-history system message used to suppress the
// injection and upstream answered 11128 ("first message is not system
// prompt"). The injected message opens the conversation; the mid-history
// one is preserved untouched.
func TestEnsureSystemFirstMidHistorySystemStillInjects(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "mid-history policy"},
		map[string]any{"role": "assistant", "content": "hello"},
	}}
	if !ensureSystemMessageInPlace(obj, issue20GlobalSA()) {
		t.Fatal("injection expected: mid-history system must not satisfy the first-position check")
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); !strings.EqualFold(role, "system") {
		t.Fatalf("first role = %q, want injected system", role)
	}
	mid, _ := msgs[2].(map[string]any)
	if c, _ := mid["content"].(string); c != "mid-history policy" {
		t.Fatalf("mid-history system content = %q, want preserved in place", c)
	}
}

func TestEnsureSystemFirstSystemFirstUntouched(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "System", "content": "be nice"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	if ensureSystemMessageInPlace(obj, issue20GlobalSA()) {
		t.Fatal("no injection expected when messages[0] is already system (case-insensitive)")
	}
	if msgs, _ := obj["messages"].([]any); len(msgs) != 2 {
		t.Fatal("messages must stay untouched")
	}
}

func TestEnsureSystemFirstCNUnchanged(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if ensureSystemMessageInPlace(obj, sa) {
		t.Fatal("CN traffic must stay byte-identical (no injection)")
	}
}

func TestEnsureSystemFirstIntlRealmUnchanged(t *testing.T) {
	// Intl (codebuddy.ai) has no first-message evidence — deliberately out
	// of scope (CHANGELOG 0.9.37); its payloads stay byte-identical.
	sa := &storedAuth{Auth: storedTokens{Domain: "codebuddy.ai"}}
	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if ensureSystemMessageInPlace(obj, sa) {
		t.Fatal("Intl realm must not be touched by the Global-only injection")
	}
}

func TestEnsureSystemFirstEmptyAndNilSafe(t *testing.T) {
	if ensureSystemMessageInPlace(map[string]any{"messages": []any{}}, issue20GlobalSA()) {
		t.Fatal("empty messages: no injection expected")
	}
	if ensureSystemMessageInPlace(map[string]any{}, issue20GlobalSA()) {
		t.Fatal("no messages array: no injection expected")
	}
	if ensureSystemMessageInPlace(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil) {
		t.Fatal("nil sa: no injection expected")
	}
}

// Pipeline-level: prepareUpstreamBody must open with a system message even
// when the client's developer-role turn lands mid-history —
// normalizeHistoryInPlace rewrites developer→system in place (position
// preserved), and the pre-0.9.37 presence scan would have skipped the
// injection, answering 11128 upstream.
func TestPrepareUpstreamBodyOpensWithSystemMidHistoryDeveloper(t *testing.T) {
	sa := issue20GlobalSA()
	payload := []byte(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"},{"role":"developer","content":"tool policy"}]}`)
	out := prepareUpstreamBody(payload, nil, sa, "gpt-5.1")
	roles := issue20Roles(t, out)
	if len(roles) == 0 || !strings.EqualFold(roles[0], "system") {
		t.Fatalf("first role = %v, want system first (issue #20)", roles)
	}
}
