package main

import (
	"strings"
	"testing"
)

// v0.12.76: the non-stream path (handleExecExecute → aggregateCompletion)
// used to bypass the frame guard entirely — an upstream error frame was
// silently swallowed and an empty 200 stream folded into a synthetic
// empty-content completion (silent fake success). These tests pin the
// aggregateCompletion-level guard so all three executor paths stay symmetric.

func TestAggregateCompletionRejectsErrorEvent(t *testing.T) {
	input := "event:error\n\n"
	if _, err := aggregateCompletion(strings.NewReader(input), "g-5"); err == nil {
		t.Fatalf("expected error for upstream error event, got nil")
	}
}

func TestAggregateCompletionRejects200ErrorBody(t *testing.T) {
	input := "data: {\"error\":{\"message\":\"quota exhausted\",\"code\":11103}}\n\n"
	_, err := aggregateCompletion(strings.NewReader(input), "g-5")
	if err == nil || !strings.Contains(err.Error(), "workbuddy upstream error") {
		t.Fatalf("expected workbuddy upstream error, got %v", err)
	}
}

func TestAggregateCompletionRejectsErrorCodeFrame(t *testing.T) {
	// Non-zero code with null error body is still an upstream error frame.
	input := "data: {\"code\":11101,\"error\":null}\n\n"
	_, err := aggregateCompletion(strings.NewReader(input), "g-5")
	if err == nil || !strings.Contains(err.Error(), "workbuddy upstream error") {
		t.Fatalf("expected workbuddy upstream error for non-zero code, got %v", err)
	}
}

func TestAggregateCompletionRejectsEmptyStream(t *testing.T) {
	input := ": keep-alive\ndata: [DONE]\n\n"
	_, err := aggregateCompletion(strings.NewReader(input), "g-5")
	if err == nil || !strings.Contains(err.Error(), "empty_stream") {
		t.Fatalf("expected empty_stream error, got %v", err)
	}
}

func TestAggregateCompletionFoldsNormalChunks(t *testing.T) {
	input := strings.Join([]string{
		"data: {\"id\":\"c1\",\"model\":\"g-5\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}",
		": keep-alive",
		"data: {\"id\":\"c1\",\"model\":\"g-5\",\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":null}]}",
		"data: {\"id\":\"c1\",\"model\":\"g-5\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}",
		"data: [DONE]",
		"",
	}, "\n")
	out, err := aggregateCompletion(strings.NewReader(input), "g-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "\"content\":\"hello\"") {
		t.Fatalf("content not folded: %s", out)
	}
	if !strings.Contains(string(out), "\"finish_reason\":\"stop\"") {
		t.Fatalf("finish missing: %s", out)
	}
	if !strings.Contains(string(out), "\"total_tokens\":5") {
		t.Fatalf("usage missing: %s", out)
	}
}
