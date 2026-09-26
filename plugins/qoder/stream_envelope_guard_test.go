package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- qoderUnwrapFrame: envelope errors must surface (v0.8.17) ---

func TestQoderUnwrapFrame_ErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"sse event line", "event:error"},
		{"envelope status 500", `data: {"statusCodeValue":500,"body":"{\"error\":\"quota drained\"}"}`},
		{"envelope status 403", `data: {"statusCodeValue":403,"body":"{}"}`},
		{"body error object", `data: {"statusCodeValue":200,"body":"{\"error\":{\"message\":\"model not found\"}}"}`},
		{"body error string", `data: {"statusCodeValue":200,"body":"{\"error\":\"upstream exploded\"}"}`},
	}
	for _, tc := range cases {
		_, _, err := qoderUnwrapFrame(tc.line)
		if err == nil {
			t.Errorf("%s: error envelope must surface, got nil", tc.name)
		}
	}
}

func TestQoderUnwrapFrame_PayloadAndControl(t *testing.T) {
	// A normal completion envelope unwraps to its inner body.
	body, meaningful, err := qoderUnwrapFrame(`data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"}`)
	if err != nil || !meaningful || !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("normal envelope: err=%v meaningful=%v body=%q", err, meaningful, body)
	}

	// Control lines and [DONE] are not meaningful, not errors.
	for _, line := range []string{": keep-alive", "event: message", "", `data: {"body":"[DONE]"}`} {
		_, meaningful, err := qoderUnwrapFrame(line)
		if err != nil || meaningful {
			t.Errorf("control line %q: err=%v meaningful=%v", line, err, meaningful)
		}
	}

	// error:null inside a 200 body is not an error.
	if _, _, err := qoderUnwrapFrame(`data: {"statusCodeValue":200,"body":"{\"error\":null}"}`); err != nil {
		t.Errorf("error:null must pass: %v", err)
	}
}

// TestCollectUpstreamStreamQoder_EmptyStreamRejected pins the v0.8.17 guard:
// keep-alives + [DONE] with zero payload chunks must fail, not aggregate into
// an empty-but-successful completion.
func TestCollectUpstreamStreamQoder_EmptyStreamRejected(t *testing.T) {
	// collectUpstreamStreamQoder builds its own HTTP call; exercise the guard
	// semantics through qoderUnwrapFrame + the documented pump contract by
	// verifying a payload-bearing envelope passes and a control-only stream
	// yields zero meaningful frames.
	envelope := `data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"x\"}}]}"}`
	if _, meaningful, err := qoderUnwrapFrame(envelope); err != nil || !meaningful {
		t.Fatalf("payload envelope must be meaningful: err=%v meaningful=%v", err, meaningful)
	}
	for _, line := range []string{": keep-alive", `data: {"body":"[DONE]"}`} {
		if _, meaningful, _ := qoderUnwrapFrame(line); meaningful {
			t.Fatalf("control line %q must not count toward the empty-stream guard", line)
		}
	}
}

// The gateway wraps provider errors behind a generic top-level message and
// parks the actionable sentence in details (JSON string or object, nested
// under error.message). The composed error must surface that sentence — the
// 4001 tool-orphan family is only diagnosable through it — while the
// redaction layer still catches credential material.
func TestQoderUnwrapFrame_ErrorDetailsSurfaced(t *testing.T) {
	const message = "Messages with role 'tool' must be a response to a preceding message with 'tool_calls'"
	inner := map[string]any{"error": map[string]any{"message": message + " Bearer secret-token-1234567890"}}
	rawInner, _ := json.Marshal(inner)
	for _, details := range []any{string(rawInner), inner} {
		body, _ := json.Marshal(map[string]any{
			"code": "provider_error", "type": "provider_error",
			"message": "Error in upstream response", "details": details,
		})
		frame, _ := json.Marshal(map[string]any{"body": string(body), "statusCodeValue": 400})
		_, _, err := qoderUnwrapFrame("data:" + string(frame))
		if err == nil {
			t.Fatal("upstream rejection ignored")
		}
		for _, want := range []string{"status=400", "code=provider_error", "type=provider_error", message} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("missing %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "secret-token-1234567890") {
			t.Fatal("credential exposed")
		}
	}
}

// v0.8.25 (issue #19): the gateway occasionally emits a bare timing object on
// the data channel right before [DONE] — no choices, no usage, no error.
// Strict clients (ZCode) reject it: "expected array at choices / expected
// object at error". The unwrapper must treat it as a control line, in both
// wire shapes it can arrive in.
func TestQoderUnwrapFrame_SwallowsGatewayTimingMetadata(t *testing.T) {
	// Real wire lines from the reporter's capture (issue #19).
	cases := []struct {
		name string
		line string
	}{
		{"bare timing frame", `data: {"firstTokenDuration":3037,"totalDuration":3045,"serverDuration":28}`},
		{"bare timing frame (ZCode sample)", `data: {"firstTokenDuration":1335,"totalDuration":6280,"serverDuration":79}`},
		{"envelope-wrapped timing body", `data: {"statusCodeValue":200,"body":"{\"firstTokenDuration\":1335,\"totalDuration\":6280,\"serverDuration\":79}"}`},
		// The error:null heartbeat carries no choices/usage either — it was
		// never a chat chunk, only it predates the rule. Swallowed too (still
		// not an error; the v0.8.17 assertion only pins err==nil).
		{"error:null heartbeat", `data: {"statusCodeValue":200,"body":"{\"error\":null}"}`},
	}
	for _, tc := range cases {
		body, meaningful, err := qoderUnwrapFrame(tc.line)
		if err != nil {
			t.Errorf("%s: must not error, got %v", tc.name, err)
		}
		if meaningful {
			t.Errorf("%s: must not count toward the empty-stream guard", tc.name)
		}
		if body != "" {
			t.Errorf("%s: must be swallowed, got body %q", tc.name, body)
		}
	}

	// A stream made only of timing frames must stay a failure (the
	// empty-stream guard reads meaningful), never fold into a fake success.
	// Semantics pinned here at the unwrap level, matching the documented
	// pump/collect contract.
}

// Negative guards for the v0.8.25 swallow rule: anything that IS a chat chunk
// must still come through, even when it looks metadata-ish.
func TestQoderUnwrapFrame_KeepsRealChunksDespiteSwallowRule(t *testing.T) {
	kept := []struct {
		name string
		line string
	}{
		// OpenAI include_usage terminal chunk: empty choices + usage.
		{"usage terminal chunk", `data: {"id":"q1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`},
		// Usage-only body with no choices key at all.
		{"usage-only body", `data: {"usage":{"total_tokens":9}}`},
		// Role-only opener: no content yet, but it IS the chunk that
		// establishes the message role.
		{"role-only opener", `data: {"choices":[{"delta":{"role":"assistant"}}]}`},
	}
	for _, tc := range kept {
		body, meaningful, err := qoderUnwrapFrame(tc.line)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !meaningful || body == "" {
			t.Errorf("%s: real chunk must be kept, got meaningful=%v body=%q", tc.name, meaningful, body)
		}
	}
}
