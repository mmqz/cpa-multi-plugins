package main

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// issue #36 (onlyjco): the intl/IDE upstream maps its internal end_turn to
// finish_reason:"stop" even after emitting tool_calls deltas, so a
// tool-calling agent loop sees stop+tool_calls and breaks. Every executor
// path must correct the terminal marker.

// TestFoldedFinishReasonToolCallsMatrix pins the fold decision table.
func TestFoldedFinishReasonToolCallsMatrix(t *testing.T) {
	cases := []struct {
		name   string
		finish string
		calls  int
		want   string
	}{
		{"stop with calls", "stop", 1, "tool_calls"},
		{"absent with calls", "", 2, "tool_calls"},
		{"correct already", "tool_calls", 1, "tool_calls"},
		{"length kept", "length", 1, "length"},
		{"filter kept", "content_filter", 1, "content_filter"},
		{"stop without calls", "stop", 0, "stop"},
		{"absent without calls", "", 0, "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldedFinishReason(tc.finish, tc.calls); got != tc.want {
				t.Fatalf("foldedFinishReason(%q,%d) = %q, want %q", tc.finish, tc.calls, got, tc.want)
			}
		})
	}
}

// TestAggregateCompletionRewritesStopWithToolCalls: the non-stream fold of
// the issue's exact symptom — tool_call deltas followed by a terminal
// finish_reason:"stop" — must produce finish_reason:"tool_calls".
func TestAggregateCompletionRewritesStopWithToolCalls(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "m", "")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason must be corrected to tool_calls: %s", s)
	}
	if strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("raw stop must not survive: %s", s)
	}
	if !strings.Contains(s, "call_1") {
		t.Fatalf("tool call must survive: %s", s)
	}
}

// TestAggregateCompletionKeepsLengthWithToolCalls: truncation semantics are
// informative — "length" passes through untouched (and damaged calls are
// dropped per dropTruncatedToolCalls).
func TestAggregateCompletionKeepsLengthWithToolCalls(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_ok\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_cut\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"a\\\":\\\"half-writ\"}}]},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "m", "")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("length must survive: %s", s)
	}
	if strings.Contains(s, "call_cut") || !strings.Contains(s, "call_ok") {
		t.Fatalf("dropTruncatedToolCalls semantics regressed: %s", s)
	}
}

// TestAggregateCompletionProperToolCallsUnchanged: an upstream already
// speaking the correct dialect round-trips byte-identically (no rewrite).
func TestAggregateCompletionProperToolCallsUnchanged(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "m", "")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !strings.Contains(string(out), `"finish_reason":"tool_calls"`) {
		t.Fatalf("correct finish_reason must survive: %s", string(out))
	}
}

// fakeSink collects emitted stream payloads.
type fakeSink struct {
	chunks  [][]byte
	errText string
}

func (s *fakeSink) emit(payload []byte) error {
	s.chunks = append(s.chunks, append([]byte(nil), payload...))
	return nil
}

func (s *fakeSink) emitError(message string) { s.errText = message }

// runPump drives pumpStreamFrames with a fake sink and returns the emitted
// chunks (joined) plus the terminal flags.
func runPump(t *testing.T, sse string) (string, bool, bool) {
	t.Helper()
	sink := &fakeSink{}
	scanner := bufio.NewScanner(strings.NewReader(sse))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seen, handled := pumpStreamFrames(scanner, sink, nil, false, &sseUsageCollector{}, "req-m", "up-m", "uid", time.Now(), nil, "")
	var b strings.Builder
	for _, c := range sink.chunks {
		b.Write(c)
		b.WriteByte('\n')
	}
	return b.String(), seen, handled
}

// issue36SSE is the async-path shape of the report: tool deltas then a
// terminal "stop".
const issue36SSE = "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func TestPumpRewritesStopWithToolCalls(t *testing.T) {
	out, seen, handled := runPump(t, issue36SSE)
	if !seen || handled {
		t.Fatalf("flags: seen=%v handled=%v", seen, handled)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("async pump must rewrite the terminal stop: %s", out)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("raw stop must not survive: %s", out)
	}
}

func TestPumpKeepsStopWithoutToolCalls(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, _, _ := runPump(t, sse)
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("plain stop must survive: %s", out)
	}
}

func TestPumpSameChunkToolCallsAndStop(t *testing.T) {
	// The terminal chunk itself may carry the last tool fragment AND the
	// finish marker — one pass must both mark and rewrite.
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_z\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, _, _ := runPump(t, sse)
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("co-located tool_calls+stop must rewrite: %s", out)
	}
}

func TestPumpPerChoiceIsolation(t *testing.T) {
	// Choice 0 tool calls + stop; choice 1 plain stop: only choice 0 is
	// rewritten (per-choice accumulator, not a global flag).
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]},\"finish_reason\":null},{\"index\":1,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"},{\"index\":1,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, _, _ := runPump(t, sse)
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("choice 0 must be rewritten: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("choice 1 stop must survive: %s", out)
	}
}

func TestAggregateSSERewritesStopWithToolCalls(t *testing.T) {
	// The synchronous collector path shares the correction.
	chunks, err := aggregateSSEWithCollector(strings.NewReader(issue36SSE), false, &sseUsageCollector{}, "", "", "")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var b strings.Builder
	for _, c := range chunks {
		b.Write(c.Payload)
		b.WriteByte('\n')
	}
	if !strings.Contains(b.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("sync collector must rewrite the terminal stop: %s", b.String())
	}
}

func TestFixFinishReasonToolCallsDegenerateInputs(t *testing.T) {
	// Non-JSON and non-choice payloads return byte-for-byte.
	for _, in := range []string{"", "not-json", `{"object":"list"}`, `{"choices":"not-array"}`} {
		seen := map[int]bool{}
		if got := fixFinishReasonToolCalls(in, seen); got != in {
			t.Fatalf("degenerate input %q altered to %q", in, got)
		}
	}
	// nil toolSeen guard.
	if got := fixFinishReasonToolCalls(`{}`, nil); got != `{}` {
		t.Fatalf("nil map must no-op: %q", got)
	}
	// Unchanged input returns byte-for-byte (no re-marshal churn).
	in := `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	seen := map[int]bool{}
	if got := fixFinishReasonToolCalls(in, seen); got != in {
		t.Fatalf("unchanged chunk re-marshaled: %q", got)
	}
}

// --- end of tests ---
