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
