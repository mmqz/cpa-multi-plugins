package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The Tencent CN gateway blocklists the DeepSeek Harness opening signature as
// an "unapproved channel" and answers code 11-128. Replaying the same request
// with only this sentence swapped out returns 200, so the sanitizer has to
// rewrite it while leaving the rest of the system prompt intact.
func TestSanitizeBlockedTemplatesDeepSeekHarness(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "verbatim",
			in:   "You are an AI agent powered by DeepSeek Harness. Follow the rules.",
			want: "You are an AI assistant. Follow the rules.",
		},
		{
			name: "no trailing period",
			in:   "You are an AI agent powered by DeepSeek Harness",
			want: "You are an AI assistant",
		},
		{
			name: "case drift",
			in:   "you are an AI agent powered by deepseek harness.",
			want: "You are an AI assistant.",
		},
		{
			name: "embedded in a long prompt",
			in:   "Intro line.\nYou are an AI agent powered by DeepSeek Harness.\nTools follow.",
			want: "Intro line.\nYou are an AI assistant.\nTools follow.",
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

// The DSH runtime-context line "the DeepSeek Harness Web GUI" is a separate
// member of the same 11-128 phrase family (verified by single-variable
// replay: only the full four-token phrase triggers; any broken token —
// "DeepSeek Harness Site GUI", "DeepSeek Harness Web UI", "Harness Web GUI",
// bare "Web GUI" — returns 200). It must be rewritten to the verified-safe
// "web interface" phrasing.
func TestSanitizeBlockedTemplatesDeepSeekHarnessWebGUI(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "verbatim",
			in:   "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names.",
			want: "You are interacting with the user through the DeepSeek Harness web interface whose URL the runtime context names.",
		},
		{
			name: "case drift",
			in:   "through the deepseek harness web gui.",
			want: "through the DeepSeek Harness web interface.",
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

// Near-misses must survive untouched: only the full "You are an AI agent
// powered by DeepSeek Harness" prefix is blocklisted (verified by replay —
// each of these returned 200 against the live gateway).
func TestSanitizeBlockedTemplatesKeepsNearMisses(t *testing.T) {
	untouched := []string{
		"DeepSeek Harness",
		"powered by DeepSeek Harness",
		"You are an AI agent powered by DeepSeek.",
		"You are an AI agent powered by Acme Harness.",
		"AI agent powered by DeepSeek Harness.",
		// Web-GUI near-misses: each verified 200 against the live gateway.
		"DeepSeek Harness Site GUI",
		"DeepSeek Harness Web UI",
		"Harness Web GUI",
		"Web GUI",
		"DeepSeek Harness web interface",
	}
	for _, in := range untouched {
		if got := sanitizeBlockedTemplates(in); got != in {
			t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want it untouched", in, got)
		}
	}
}

// deepSeekHarnessBody rebuilds the captured-failure shape from PR #46's
// single-variable replay at miniature scale: a CN-realm account, a system
// message opening with the DeepSeek Harness signature followed by real
// instruction bulk, a user turn that pastes the same sentence, and the
// reasoning_effort=max marker. Both copies of the sentence are rewritten as
// of v0.9.63 (the gate reads the whole body, not just the system role).
func deepSeekHarnessBody(t *testing.T) []byte {
	t.Helper()
	tools := make([]map[string]any, 0, 8)
	for i := 0; i < 8; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "tool_" + string(rune('a'+i)),
				"description": strings.Repeat("does something useful. ", 40),
			},
		})
	}
	body := map[string]any{
		"model":            "deepseek-chat",
		"reasoning_effort": "max",
		"stream":           false,
		"tools":            tools,
		"messages": []any{
			map[string]any{
				"role": "system",
				"content": "You are an AI agent powered by DeepSeek Harness. " +
					"Follow the repository conventions and keep diffs minimal. " +
					strings.Repeat("Operate carefully. ", 120),
			},
			map[string]any{
				"role":    "user",
				"content": "Review this agent intro I found: You are an AI agent powered by DeepSeek Harness. Keep it?",
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestPrepareUpstreamBodyDeepSeekHarnessEndToEnd drives the full pipeline
// the compiled plugin runs (prepareUpstreamBody) with a CN-realm credential,
// proving the rewrite survives body preparation and lands in the outbound
// JSON. The user-role copy is rewritten too: the 2026-10-21 field failure
// pinned that the gate reads past the system message, and DSH itself injects
// its runtime-context boilerplate into user turns.
func TestPrepareUpstreamBodyDeepSeekHarnessEndToEnd(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{Domain: "copilot.tencent.com"}}
	out := prepareUpstreamBody(deepSeekHarnessBody(t), nil, sa, "")
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (CN: no system-first injection)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	sc, _ := sys["content"].(string)
	if strings.Contains(strings.ToLower(sc), "you are an ai agent powered by deepseek harness") {
		t.Fatalf("system intro must be rewritten in the outbound body, got %q", sc[:80])
	}
	if !strings.HasPrefix(sc, "You are an AI assistant. Follow the repository") {
		t.Errorf("rewrite must preserve the remainder of the system prompt, got %q", sc[:80])
	}
	user, _ := msgs[1].(map[string]any)
	uc, _ := user["content"].(string)
	if strings.Contains(uc, "You are an AI agent powered by DeepSeek Harness.") {
		t.Errorf("user-role copy must be rewritten as of v0.9.63 (whole-body gate), got %q", uc)
	}
	if !strings.Contains(uc, "You are an AI assistant.") {
		t.Errorf("user-role rewrite must keep the surrounding sentence, got %q", uc)
	}
	// The captured failure carried reasoning_effort=max; the pipeline mirrors
	// it to reasoning_summary but must not strip the rewrite.
	if _, ok := body["reasoning_summary"]; !ok {
		t.Errorf("reasoning_effort mirror expected in outbound body")
	}
}

// TestUpstreamStatusError11128CarriesRequestScopedStatus pins the wire shape
// the v8 host decodes: a 400/11128 rejection crosses the RPC envelope with
// http_status=400 so clienterror.IsRequestFault classifies it request-scoped
// (no rotation, no credential cooldown). Status 0 (the pre-0.9.62 shape) is
// what let the host walk and cool all 8 CN accounts into a 503 cascade.
func TestUpstreamStatusError11128CarriesRequestScopedStatus(t *testing.T) {
	payload := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"a02219ca-9976-4475-9213-7f3dc5b21d5b"}`
	inner := translateChatUpstreamErrorFull(http.StatusBadRequest, payload, nil, nil)
	wrapped := upstreamStatusError(http.StatusBadRequest, payload, inner)
	var se *statusError
	if !errors.As(wrapped, &se) {
		t.Fatalf("11128 must wrap statusError, got %T", wrapped)
	}
	if se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", se.StatusCode())
	}
	raw := errorEnvelopeFor(wrapped)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got ok=%v", env.OK)
	}
	if env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope http_status = %d, want 400 (v8 IsRequestFault keying)", env.Error.HTTPStatus)
	}
	// v0.9.79: the guidance spells the code hyphenated (11-128) — the raw
	// payload below still carries the unhyphenated form, so this also pins
	// that the RAW code stays visible for diagnosis while our own copy
	// cannot re-arm the content gate when echoed back.
	if !strings.Contains(env.Error.Message, "11-128") || !strings.Contains(env.Error.Message, `"code":11128`) {
		t.Fatalf("bilingual guidance must survive the envelope, got %q", env.Error.Message)
	}
}

// TestSanitizeHistoryPhrasesAssistantEcho pins the 2026-10-21 field failure:
// the triggers lived in an ASSISTANT history echo (a DSH runtime-context
// summary quoting "DeepSeek Harness Web GUI" 15x) and a user paste, with a
// clean system prompt — the system-only sanitizer left the body 400/11128.
// sanitizeHistoryPhrasesInPlace must scrub every role.
func TestSanitizeHistoryPhrasesAssistantEcho(t *testing.T) {
	body := map[string]any{
		"model":  "deepseek-chat",
		"stream": false,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an AI assistant."},
			map[string]any{"role": "user", "content": "context: interacting through the DeepSeek Harness Web GUI today."},
			map[string]any{
				"role":    "assistant",
				"content": "summary of runtime context:\n- the DeepSeek Harness Web GUI URL\n- Main branch (you will usually use this for PRs)\nDone.",
			},
			map[string]any{
				"role":    "user",
				"content": "Continue.",
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sa := &storedAuth{Auth: storedTokens{Domain: "copilot.tencent.com"}}
	out := prepareUpstreamBody(raw, nil, sa, "")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	lower := strings.ToLower(outStr(t, out))
	for _, bad := range []string{"deepseek harness web gui", "main branch (you will usually use this for prs)"} {
		if strings.Contains(lower, bad) {
			t.Errorf("outbound body must not carry trigger phrase %q\nbody=%s", bad, outStr(t, out))
		}
	}
	user, _ := msgs[1].(map[string]any)
	uc, _ := user["content"].(string)
	if !strings.Contains(uc, "DeepSeek Harness web interface") {
		t.Errorf("user paste must be rewritten to the safe phrasing, got %q", uc)
	}
	asst, _ := msgs[2].(map[string]any)
	ac, _ := asst["content"].(string)
	if !strings.Contains(ac, "Default branch (you will usually use this for PRs)") {
		t.Errorf("assistant echo of the git line must be rewritten, got %q", ac)
	}
	// v0.9.79: in identity roles the brand shrinks further after the
	// four-token rewrite (#574: brand + GUI-description co-occurrence is
	// the same 11-128 family; "DeepSeek" alone passes).
	if !strings.Contains(ac, "- the DeepSeek web interface URL") {
		t.Errorf("assistant echo must keep line structure with the shrunk brand, got %q", ac)
	}
	// Non-trigger content must stay byte-identical.
	tail, _ := msgs[3].(map[string]any)
	tc, _ := tail["content"].(string)
	if tc != "Continue." {
		t.Errorf("non-trigger user content must pass verbatim, got %q", tc)
	}
}

// TestSanitizeHistoryPhrasesToolCallsAndReasoning covers the two other
// carriers: tool_calls.function.arguments and assistant reasoning_content
// (which injectReasoningInPlace folds into content AFTER the scrub — the
// folded <thought> block must not re-arm the phrases).
func TestSanitizeHistoryPhrasesToolCallsAndReasoning(t *testing.T) {
	body := map[string]any{
		"model":  "deepseek-chat",
		"stream": false,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an AI assistant."},
			map[string]any{
				"role":    "assistant",
				"content": "checking",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "read_file",
							"arguments": `{"path":"notes.md","note":"system said: DeepSeek Harness Web GUI"}`,
						},
					},
				},
				"reasoning_content": "recall the Main branch (you will usually use this for PRs) rule",
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sa := &storedAuth{Auth: storedTokens{Domain: "copilot.tencent.com"}}
	out := prepareUpstreamBody(raw, nil, sa, "")
	lower := strings.ToLower(outStr(t, out))
	for _, bad := range []string{"deepseek harness web gui", "main branch (you will usually use this for prs)"} {
		if strings.Contains(lower, bad) {
			t.Errorf("outbound body must not carry trigger phrase %q\nbody=%s", bad, outStr(t, out))
		}
	}
	if !strings.Contains(lower, "default branch (you will usually use this for prs)") {
		t.Errorf("folded reasoning must be scrubbed too, got %s", outStr(t, out))
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	tc0, _ := tcs[0].(map[string]any)
	fn, _ := tc0["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	// v0.9.79: assistant tool_calls are identity-scoped — the four-token
	// rewrite runs first (all roles), then the brand shrink (identity
	// roles) takes "DeepSeek Harness web interface" down to "DeepSeek
	// web interface".
	if !strings.Contains(args, "DeepSeek web interface") {
		t.Errorf("tool_calls arguments must be scrubbed with the shrunk brand, got %q", args)
	}
}

func outStr(t *testing.T, b []byte) string {
	t.Helper()
	return string(b)
}

// TestUpstreamStatusErrorHyphenated11128IsRequestScoped pins #75's payoff:
// the CN gate's {"code":"11-128"} used to fall through isChannelRiskControl,
// and on a 403 it hit the business-envelope branch — an account-level
// cooldown, the host walking and cooling every account. With the hyphenated
// spelling classified, the wrap is 400 request-scoped no matter which 4xx
// carried it.
func TestUpstreamStatusErrorHyphenated11128IsRequestScoped(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		payload := `{"code":"11-128","msg":"request illegal"}`
		inner := translateChatUpstreamErrorFull(status, payload, nil, nil)
		wrapped := upstreamStatusError(status, payload, inner)
		var se *statusError
		if !errors.As(wrapped, &se) {
			t.Fatalf("status %d: hyphenated 11-128 must wrap statusError, got %T", status, wrapped)
		}
		if se.StatusCode() != http.StatusBadRequest {
			t.Fatalf("status %d: wrapped status = %d, want 400 (request-scoped)", status, se.StatusCode())
		}
	}
}
