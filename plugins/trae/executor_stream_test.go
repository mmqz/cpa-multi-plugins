// executor_stream_test.go — v0.12.30: locks the executor.execute_stream RPC
// envelope contract. The host JSON-decodes the c-shared plugin's reply into
// rpcExecutorStreamResponse whose Chunks is a SLICE; marshaling
// pluginapi.ExecutorStreamResponse (Chunks is a receive-only channel) fails
// with "json: unsupported type: <-chan pluginapi.ExecutorStreamChunk" and
// 503'd every streaming call. handleExecStream / intlhandleExecStream must
// only ever return streamResponse.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStreamResponseEnvelopeMarshals(t *testing.T) {
	// pluginapi.ExecutorStreamChunk has no json tags: Payload rides as a
	// base64 string under the Go field name — exactly the wire format the
	// host produces/consumes with the SAME struct (rpcExecutorStreamResponse),
	// and the format workbuddy's streamResponse already ships in production.
	frame := []byte("data: {\"id\":\"chatcmpl-1\"}\n\n")
	done := []byte("data: [DONE]\n\n")
	raw, err := json.Marshal(streamResponse{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
		Chunks: []pluginapi.ExecutorStreamChunk{
			{Payload: frame},
			{Payload: done},
		},
	})
	if err != nil {
		t.Fatalf("streamResponse must marshal: %v", err)
	}
	var probe struct {
		Headers map[string][]string             `json:"headers"`
		Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("host-side decode failed: %v", err)
	}
	if len(probe.Chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(probe.Chunks))
	}
	if string(probe.Chunks[0].Payload) != string(frame) || string(probe.Chunks[1].Payload) != string(done) {
		t.Errorf("chunk payloads did not round-trip: %q / %q", probe.Chunks[0].Payload, probe.Chunks[1].Payload)
	}
}

func TestStreamResponseAsyncReplyHasNoChunks(t *testing.T) {
	// The async path (stream_id present) replies immediately with empty
	// chunks; the host then consumes host.stream.emit frames. The envelope
	// must still marshal (chunks omitted) — this is the v0.12.30 hot path.
	raw, err := json.Marshal(streamResponse{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
	})
	if err != nil {
		t.Fatalf("async streamResponse must marshal: %v", err)
	}
	if strings.Contains(string(raw), "chunks") {
		t.Errorf("async reply should omit chunks, got %s", raw)
	}
}

func TestExecutorStreamResponseTypeIsUnmarshalable(t *testing.T) {
	// Documents the original bug: the channel-typed Chunks field makes
	// pluginapi.ExecutorStreamResponse impossible to JSON-encode.
	ch := make(chan pluginapi.ExecutorStreamChunk, 1)
	close(ch)
	_, err := json.Marshal(pluginapi.ExecutorStreamResponse{Chunks: ch})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("expected unsupported-type marshal error, got %v", err)
	}
}

func TestConvertSOLOStreamChunksAreBareJSON(t *testing.T) {
	// v0.12.36 regression: the host SSE-writer frames every chunk payload as
	// "data: %s\n\n" (openai_handlers.go), so pre-framed "data: {...}\n\n"
	// payloads reached clients as "data: data: {...}" — deepseek-harness died
	// with "Unexpected token 'd' ... is not valid JSON". Chunks must be bare
	// JSON with no SSE framing and no plugin-side "[DONE]" (the host appends
	// its own when the channel closes).
	body := "event: output\ndata: {\"response\":\"你好\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	var chunks [][]byte
	for c := range convertSOLOStreamToOpenAI(strings.NewReader(body), "m1", nil) {
		chunks = append(chunks, c)
	}
	if len(chunks) < 3 {
		t.Fatalf("want >=3 chunks (role/content/finish/usage), got %d", len(chunks))
	}
	for i, c := range chunks {
		s := string(c)
		if strings.HasPrefix(s, "data:") {
			t.Errorf("chunk %d carries SSE prefix: %q", i, s)
		}
		if strings.Contains(s, "\n\n") || strings.Contains(s, "[DONE]") {
			t.Errorf("chunk %d is SSE-framed or carries [DONE]: %q", i, s)
		}
		var obj map[string]any
		if err := json.Unmarshal(c, &obj); err != nil {
			t.Errorf("chunk %d is not bare JSON: %v (%q)", i, err, s)
		}
	}
	// The upstream reported token_usage, so a final usage chunk must follow.
	last := string(chunks[len(chunks)-1])
	if !strings.Contains(last, "usage") {
		t.Errorf("last chunk should carry usage, got %s", last)
	}
}

func TestSplitSSEBodyChunks(t *testing.T) {
	// v0.12.36: the INTL path used to emit the whole raw SSE body as ONE
	// chunk payload — the host framed it as "data: data: {...}" and the
	// plugin-side [DONE] duplicated the host's own. Splitting must yield
	// bare JSON per chunk, dropping blanks and [DONE].
	raw := "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	chunks := splitSSEBodyChunks([]byte(raw))
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	if string(chunks[0].Payload) != `{"a":1}` || string(chunks[1].Payload) != `{"b":2}` {
		t.Errorf("payloads: %q / %q", chunks[0].Payload, chunks[1].Payload)
	}
}
