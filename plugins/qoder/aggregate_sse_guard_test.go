package main

import (
	"strings"
	"testing"
)

// v0.12.76: aggregateQoderSSE (non-stream path) unwrapped gateway envelopes
// without the qoderUnwrapFrame guard — a 200-OK error envelope
// (statusCodeValue>=400 / body carrying {"error":...}) was folded into a
// synthetic empty-content completion (silent fake success), asymmetric with
// the two streaming paths. These tests pin the unwrap-level guard.

func TestAggregateQoderSSERejectsErrorEnvelope(t *testing.T) {
	input := "data:{\"statusCodeValue\":500,\"body\":\"{\\\"error\\\":{\\\"message\\\":\\\"boom\\\"}}\"}\n\n"
	_, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "qoder upstream error") {
		t.Fatalf("expected qoder upstream error, got %v", err)
	}
}

func TestAggregateQoderSSERejectsBodyError(t *testing.T) {
	// 200 envelope whose inner body carries an error object still fails.
	input := "data:{\"statusCodeValue\":200,\"body\":\"{\\\"error\\\":{\\\"message\\\":\\\"insufficient credits\\\"}}\"}\n\n"
	_, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "qoder upstream error") {
		t.Fatalf("expected qoder upstream error for inner error body, got %v", err)
	}
}

func TestAggregateQoderSSERejectsEmptyStream(t *testing.T) {
	input := "event:finish\ndata:{\"body\":\"[DONE]\"}\n\n"
	_, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "empty_stream") {
		t.Fatalf("expected empty_stream error, got %v", err)
	}
}

func TestAggregateQoderSSEFoldsNormalChunks(t *testing.T) {
	input := strings.Join([]string{
		"data:{\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"q1\\\",\\\"model\\\":\\\"glm-4.6\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"he\\\"}}]}\",\"headers\":{}}",
		": keep-alive",
		"data:{\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"q1\\\",\\\"model\\\":\\\"glm-4.6\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"llo\\\"},\\\"finish_reason\\\":\\\"stop\\\"}]}\",\"headers\":{}}",
		"data:{\"body\":\"[DONE]\"}",
		"event:finish",
		"",
	}, "\n")
	out, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "\"content\":\"hello\"") {
		t.Fatalf("content not folded: %s", out)
	}
	if !strings.Contains(string(out), "\"finish_reason\":\"stop\"") {
		t.Fatalf("finish missing: %s", out)
	}
}

// v0.8.25 (issue #19): the gateway's timing event
// ({"firstTokenDuration":N,"totalDuration":N,"serverDuration":N}) arrives on
// the data channel before [DONE] on some backend routes. It must never leak
// into the folded completion — the reporter's strict client (ZCode) died on
// "expected array at choices / expected object at error".
func TestAggregateQoderSSETimingEventDoesNotLeak(t *testing.T) {
	input := strings.Join([]string{
		"data:{\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"q1\\\",\\\"model\\\":\\\"glm-4.6\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"he\\\"}}]}\",\"headers\":{}}",
		"data:{\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"q1\\\",\\\"model\\\":\\\"glm-4.6\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"llo\\\"},\\\"finish_reason\\\":\\\"stop\\\"}]}\",\"headers\":{}}",
		// Real wire line from the reporter's capture (issue #19).
		"data: {\"firstTokenDuration\":3037,\"totalDuration\":3045,\"serverDuration\":28}",
		"data:{\"body\":\"[DONE]\"}",
		"event:finish",
		"",
	}, "\n")
	out, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "\"content\":\"hello\"") {
		t.Fatalf("content not folded: %s", out)
	}
	if strings.Contains(string(out), "firstTokenDuration") || strings.Contains(string(out), "totalDuration") {
		t.Fatalf("timing metadata leaked into the completion: %s", out)
	}
}

// A data channel that only ever carried timing frames carried no completion:
// the empty-stream guard must still fire (meaningful stays false), not fold
// into a synthetic success.
func TestAggregateQoderSSERejectsTimingOnlyStream(t *testing.T) {
	input := strings.Join([]string{
		"data: {\"firstTokenDuration\":1335,\"totalDuration\":6280,\"serverDuration\":79}",
		"data:{\"body\":\"[DONE]\"}",
		"event:finish",
		"",
	}, "\n")
	_, err := aggregateQoderSSE(strings.NewReader(input), "glm-4.6")
	if err == nil || !strings.Contains(err.Error(), "empty_stream") {
		t.Fatalf("expected empty_stream error for timing-only stream, got %v", err)
	}
}
