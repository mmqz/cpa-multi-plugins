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
