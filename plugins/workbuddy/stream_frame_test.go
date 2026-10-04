package main

import (
	"net/http"
	"strings"
	"testing"
)

// --- workBuddyStreamFrame: upstream error frames must surface (v0.9.29) ---

func TestWorkBuddyStreamFrame_ErrorFrames(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"sse event line", "event:error"},
		{"sse event line space", "event: error"},
		{"json error object", `{"error":{"message":"quota exceeded","code":11102},"choices":[]}`},
		{"json error string", `{"error":"internal upstream failure"}`},
		{"json nonzero code", `{"code":11102,"choices":[{"delta":{"content":"x"}}]}`},
	}
	for _, tc := range cases {
		_, _, err := workBuddyStreamFrame(tc.line)
		if err == nil {
			t.Errorf("%s: error frame must surface, got nil", tc.name)
		}
	}
}

func TestWorkBuddyStreamFrame_PayloadAndControl(t *testing.T) {
	// A normal completion chunk is meaningful and passes content through.
	content, meaningful, err := workBuddyStreamFrame(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	if err != nil || !meaningful || !strings.Contains(content, `"content":"hi"`) {
		t.Fatalf("normal chunk: err=%v meaningful=%v content=%q", err, meaningful, content)
	}

	// Transport control lines are neither content nor errors.
	for _, line := range []string{": keep-alive", "event: message", "", "data: [DONE]"} {
		content, meaningful, err := workBuddyStreamFrame(line)
		if err != nil {
			t.Errorf("control line %q: unexpected error %v", line, err)
		}
		if meaningful {
			t.Errorf("control line %q must not be meaningful", line)
		}
		if line != "data: [DONE]" && content != "" {
			t.Errorf("control line %q must yield empty content, got %q", line, content)
		}
	}

	// A null error field is not an error.
	if _, _, err := workBuddyStreamFrame(`data: {"error":null,"choices":[{"delta":{}}]}`); err != nil {
		t.Errorf("error:null must not be an error frame: %v", err)
	}
}

// --- empty-stream guard: a 200 stream with no payload is a failure ---

func TestAggregateSSE_EmptyStreamRejected(t *testing.T) {
	body := ": keep-alive\n\n" + "data: [DONE]\n"
	if _, err := aggregateSSEWithCollector(strings.NewReader(body), false, nil); err == nil {
		t.Fatalf("keep-alive + [DONE] with zero payload chunks must fail, got nil error")
	} else if !strings.Contains(err.Error(), "empty_stream") {
		t.Fatalf("empty-stream error should name the condition, got %v", err)
	}

	ok := `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n" + "data: [DONE]\n"
	chunks, err := aggregateSSEWithCollector(strings.NewReader(ok), false, nil)
	if err != nil {
		t.Fatalf("stream with a payload chunk must pass: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}

	// An error frame mid-stream aborts collection.
	bad := `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n" + `event:error` + "\n"
	if _, err := aggregateSSEWithCollector(strings.NewReader(bad), false, nil); err == nil {
		t.Fatalf("error frame must abort aggregation")
	}
}

// --- forceMaxThinking: the Hy3 "low" catalog level is preserved (v0.9.29) ---

func TestForceMaxThinking_Hy3LowPreserved(t *testing.T) {
	low := map[string]any{"model": "hy3-x", "reasoning_effort": "low"}
	if forceMaxThinking(low) {
		t.Fatalf("explicit hy3 low must be preserved, body now %v", low)
	}
	if low["reasoning_effort"] != "low" {
		t.Fatalf("hy3 low was rewritten to %v", low["reasoning_effort"])
	}

	// Non-low values keep the historical high pin.
	medium := map[string]any{"model": "hy3", "reasoning_effort": "medium"}
	if !forceMaxThinking(medium) || medium["reasoning_effort"] != "high" {
		t.Fatalf("hy3 medium should pin to high, got %v", medium["reasoning_effort"])
	}
	// hy4 family: even low still pins to high (only hy3/hy3-x advertise low).
	hy4 := map[string]any{"model": "hy4-preview", "reasoning_effort": "low"}
	if !forceMaxThinking(hy4) || hy4["reasoning_effort"] != "high" {
		t.Fatalf("hy4 low should still pin to high, got %v", hy4["reasoning_effort"])
	}
}

// --- backendHeaders: the WorkBuddy CLI identity fills only the blank case ---

func TestBackendHeaders_IDEDefaultFillsBlankOnly(t *testing.T) {
	cli := &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "copilot.tencent.com"}}
	req := &http.Request{Header: http.Header{}}
	backendHeaders(req, cli)
	if req.Header.Get("X-IDE-Name") != "WorkBuddy" || req.Header.Get("X-IDE-Type") != "WorkBuddy" || req.Header.Get("X-IDE-Version") != "5.5.6" {
		t.Fatalf("CLI-login CN account must carry the WorkBuddy identity, got %v", req.Header)
	}

	// An IDE-platform account keeps its adopted CodeBuddyIDE identity.
	ide := &storedAuth{Auth: storedTokens{AccessToken: "at", LoginPlatform: "ide"}}
	req2 := &http.Request{Header: http.Header{}}
	backendHeaders(req2, ide)
	if req2.Header.Get("X-IDE-Name") != "CodeBuddyIDE" {
		t.Fatalf("IDE-platform override must survive, got %q", req2.Header.Get("X-IDE-Name"))
	}

	// An Intl realm account keeps its CodeBuddy identity.
	intl := &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "codebuddy.ai"}}
	req3 := &http.Request{Header: http.Header{}}
	backendHeaders(req3, intl)
	if req3.Header.Get("X-IDE-Name") != "CodeBuddy" {
		t.Fatalf("Intl realm override must survive, got %q", req3.Header.Get("X-IDE-Name"))
	}
}
