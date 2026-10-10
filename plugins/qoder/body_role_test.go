// body_role_test.go — issue #34: the gateway rejects OpenAI's "developer"
// role ("developer is not one of ['system', 'assistant', 'user', 'tool',
// 'function']"). buildQoderBody must normalize it to "system" — the mapping
// both the gateway's own validator enumeration and the reference proxy's
// normalize_roles() agree on — while preserving content, position, and every
// raw member, and without mutating the caller's request.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodedMessage is the minimal view of one message inside the built
// upstream body used by these assertions.
type decodedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	Name    string          `json:"name,omitempty"`
}

func buildBodyMessages(t *testing.T, body []byte) []decodedMessage {
	t.Helper()
	var m struct {
		Messages []decodedMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	return m.Messages
}

func roleRequest(t *testing.T, msgs ...map[string]any) *openAIRequest {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"model": "qfmodel", "messages": msgs})
	if err != nil {
		t.Fatalf("request encode: %v", err)
	}
	req := &openAIRequest{}
	if err := json.Unmarshal(raw, req); err != nil {
		t.Fatalf("request decode: %v", err)
	}
	return req
}

// TestDeveloperRoleNormalizedToSystem: a slim request whose instruction side
// is a lone developer turn must reach upstream as role "system" with the
// content byte-identical, and the caller's request must stay untouched.
func TestDeveloperRoleNormalizedToSystem(t *testing.T) {
	req := roleRequest(t,
		map[string]any{"role": "developer", "content": "You are a helpful coding assistant."},
		map[string]any{"role": "user", "content": "hi"},
	)
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	msgs := buildBodyMessages(t, body)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages upstream, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("instruction turn role = %q, want system (developer must not reach upstream)", msgs[0].Role)
	}
	var content string
	if err := json.Unmarshal(msgs[0].Content, &content); err != nil {
		t.Fatalf("content decode: %v", err)
	}
	if content != "You are a helpful coding assistant." {
		t.Errorf("content changed: %q", content)
	}
	if msgs[1].Role != "user" {
		t.Errorf("user turn role = %q, want user", msgs[1].Role)
	}
	// The caller's request is never mutated (loop-copy contract).
	if req.Messages[0].Role != "developer" {
		t.Errorf("caller request mutated: role = %q, want developer", req.Messages[0].Role)
	}
}

// TestSystemAndDeveloperBothKeptAsSystem: clients commonly send system first
// then developer (OpenAI docs allow the pair). After the rewrite upstream
// sees two system messages in original order — the gateway's per-message
// role Literal has no cardinality constraint, and the reference proxy keeps
// the same sequence.
func TestSystemAndDeveloperBothKeptAsSystem(t *testing.T) {
	req := roleRequest(t,
		map[string]any{"role": "system", "content": "base instructions"},
		map[string]any{"role": "developer", "content": "override instructions"},
		map[string]any{"role": "user", "content": "hi"},
	)
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	msgs := buildBodyMessages(t, body)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages upstream, got %d", len(msgs))
	}
	want := []string{"system", "system", "user"}
	for i, w := range want {
		if msgs[i].Role != w {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, w)
		}
	}
	raw := string(body)
	if strings.Contains(raw, `"role":"developer"`) || strings.Contains(raw, `"role": "developer"`) {
		t.Errorf("developer role leaked into upstream body")
	}
}

// TestDeveloperRawMembersRideAlong: the rewrite must not disturb the
// verbatim passthrough contract — extra members (name) survive, and the
// slim/template switch still triggers on developer-only requests (template
// system prompt dropped).
func TestDeveloperRawMembersRideAlong(t *testing.T) {
	req := roleRequest(t,
		map[string]any{"role": "developer", "content": "rules", "name": "project-rules"},
		map[string]any{"role": "user", "content": "hi"},
	)
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	msgs := buildBodyMessages(t, body)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages upstream, got %d", len(msgs))
	}
	if msgs[0].Name != "project-rules" {
		t.Errorf("raw member name lost: %q", msgs[0].Name)
	}
	if msgs[0].Role != "system" {
		t.Errorf("role = %q, want system", msgs[0].Role)
	}
	// Slim mode: the 10K-token template system prompt must be gone.
	if strings.Contains(string(body), "You are QoderWork") {
		t.Errorf("template system prompt present — slim detection did not fire on developer turn")
	}
}

// TestNonDeveloperRolesUntouched: user/assistant/tool roles round-trip
// exactly as before the normalization existed.
func TestNonDeveloperRolesUntouched(t *testing.T) {
	req := roleRequest(t,
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "assistant", "content": "a"},
		map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_1"},
		map[string]any{"role": "user", "content": "next"},
	)
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	msgs := buildBodyMessages(t, body)
	// No instruction turn → template mode: the template system prompt rides
	// in front of the four client messages.
	want := []string{"system", "user", "assistant", "tool", "user"}
	if len(msgs) != len(want) {
		t.Fatalf("expected %d messages upstream, got %d", len(want), len(msgs))
	}
	for i, w := range want {
		if msgs[i].Role != w {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, w)
		}
	}
}
