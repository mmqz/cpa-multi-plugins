package main

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// termSink records emitted payloads and the terminal error text.
type termSink struct {
	chunks  [][]byte
	errText string
}

func (s *termSink) emit(payload []byte) error {
	s.chunks = append(s.chunks, append([]byte(nil), payload...))
	return nil
}

func (s *termSink) emitError(message string) { s.errText = message }

func TestChunkCarriesFinish(t *testing.T) {
	yes := `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	no := `{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`
	blank := `{"choices":[{"delta":{},"finish_reason":""}]}`
	if !chunkCarriesFinish(yes) {
		t.Fatal("finish_reason=stop not detected")
	}
	if chunkCarriesFinish(no) || chunkCarriesFinish(blank) {
		t.Fatal("empty finish_reason treated as a deliberate end")
	}
	if !chunkCarriesFinish(`{"choices":[{"delta":{},"finish_reason":"length"}]}`) {
		t.Fatal("finish_reason=length must count as a deliberate end")
	}
	if !chunkCarriesFinish(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) {
		t.Fatal("finish_reason=tool_calls must count as a deliberate end")
	}
}

// The truncation error must stay in the transport-lifecycle family the host
// cooldown classifier already understands (the "unexpected EOF" marker
// emptyStreamError uses): an upstream cut is not the credential's fault.
func TestTruncatedStreamError_TransportLifecycle(t *testing.T) {
	msg := truncatedStreamError().Error()
	for _, want := range []string{"truncated_stream:", "unexpected EOF"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("truncatedStreamError() = %q, missing %q", msg, want)
		}
	}
	if strings.Contains(strings.ToLower(msg), "rate limit") || strings.Contains(strings.ToLower(msg), "quota") {
		t.Fatalf("transport error must not look like a quota failure: %q", msg)
	}
}

func TestAggregateSSE_TruncatedStreamRejected(t *testing.T) {
	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"half\"},\"finish_reason\":null}]}\n\n"
	if _, err := aggregateSSEWithCollector(strings.NewReader(cut), false, nil); err == nil {
		t.Fatal("payload stream without finish_reason/[DONE] must fail")
	} else if !strings.Contains(err.Error(), "truncated_stream") {
		t.Fatalf("want truncated_stream, got %v", err)
	}

	finished := cut + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	if _, err := aggregateSSEWithCollector(strings.NewReader(finished), false, nil); err != nil {
		t.Fatalf("finish_reason=stop must be accepted: %v", err)
	}

	done := cut + "data: [DONE]\n\n"
	if _, err := aggregateSSEWithCollector(strings.NewReader(done), false, nil); err != nil {
		t.Fatalf("[DONE] must be accepted: %v", err)
	}
}

func TestAggregateCompletion_TruncatedStreamRejected(t *testing.T) {
	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"half\"},\"finish_reason\":null}]}\n\n"
	if _, err := aggregateCompletion(strings.NewReader(cut), "m"); err == nil {
		t.Fatal("folded completion without finish_reason/[DONE] must fail")
	} else if !strings.Contains(err.Error(), "truncated_stream") {
		t.Fatalf("want truncated_stream, got %v", err)
	}
}

func TestPump_TruncatedStreamRejected(t *testing.T) {
	run := func(sse string) (bool, string) {
		sink := &termSink{}
		scanner := bufio.NewScanner(strings.NewReader(sse))
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		_, handled := pumpStreamFrames(scanner, sink, nil, false, &sseUsageCollector{}, "req-m", "up-m", "uid", time.Now(), nil)
		return handled, sink.errText
	}

	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"half\"},\"finish_reason\":null}]}\n\n"
	handled, errText := run(cut)
	if !handled || !strings.Contains(errText, "truncated_stream") {
		t.Fatalf("truncated pump: handled=%v err=%q", handled, errText)
	}

	done := cut + "data: [DONE]\n\n"
	if handled, errText = run(done); handled || errText != "" {
		t.Fatalf("[DONE] stream must pass: handled=%v err=%q", handled, errText)
	}

	finished := cut + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	if handled, errText = run(finished); handled || errText != "" {
		t.Fatalf("finish_reason=stop must pass: handled=%v err=%q", handled, errText)
	}
}
