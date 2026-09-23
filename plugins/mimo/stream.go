// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), and collecting it synchronously
// (collectUpstreamStream).
//
// Both mimo lanes emit standard OpenAI SSE frames (`data: {chunk}` …
// `data: [DONE]`) with reasoning_content / tool_calls deltas passing through.
// Upstream errors can still arrive as 200-OK bodies carrying an `error`
// field — mimoUnwrapFrame catches those so a 200 envelope never folds into a
// silent fake success (the guard symmetry the qoder/workbuddy plugins learned
// the hard way).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	body, err := streamErrorFrame(streamID, redactSecrets(message))
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamEmit, body)
}

// streamErrorFrame renders the host.stream.emit request for a terminal error.
// The message must travel in the RPC top-level "error" field, never inside
// "payload": the host turns req.Error into the chunk's Err (feeding its
// failure-classification / cooldown layer and surfacing a real terminal error
// to the client), while a payload-embedded {"error":...} blob is just another
// data chunk — the SSE translator drops the unframed line, the client sees a
// truncated stream, and our wording never reaches the classifier.
// Raw upstream bodies may carry cookies/sk material — callers pass the
// message through redactSecrets first.
func streamErrorFrame(streamID, message string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
}

var streamCloseOnce sync.Map // streamID -> sync.Once

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	actual, _ := streamCloseOnce.LoadOrStore(streamID, &sync.Once{})
	once := actual.(*sync.Once)
	once.Do(func() {
		body, _ := json.Marshal(map[string]any{"stream_id": streamID})
		_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
	})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
func pumpUpstreamStream(ctx context.Context, sa *storedAuth, route chatRoute, body string, cancel context.CancelFunc, streamID string, sseFramed bool) {
	buildReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, route.endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		route.applyHeaders(httpReq, sa, body)
		return httpReq, nil
	}
	// Always close the host stream exactly once on every exit path.
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()
	if cancel != nil {
		defer cancel()
	}

	stream, statusCode, _, err := sendChat(buildReq)
	if err != nil {
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := readAllHost(stream)
		errBody := string(errPayload)
		streamEmitError(streamID, chatUpstreamError(statusCode, errBody).Error())
		return
	}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenPayload := false
	for scanner.Scan() {
		bodyStr, meaningful, frameErr := mimoUnwrapFrame(scanner.Text())
		if frameErr != nil {
			streamEmitError(streamID, frameErr.Error())
			return
		}
		seenPayload = seenPayload || meaningful
		if bodyStr == "" || bodyStr == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort; do not fake success.
			return
		}
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame.
	if err := scanner.Err(); err != nil {
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error (unexpected EOF): %v", err))
		return
	}
	// Empty-stream guard: a 200 envelope stream with zero payload is an
	// upstream failure, not a silent success.
	if !seenPayload {
		streamEmitError(streamID, emptyStreamError().Error())
	}
}

// collectUpstreamStream is the synchronous fallback (no async stream id):
// drain the upstream SSE, return the cleaned chunks as a slice.
func collectUpstreamStream(sa *storedAuth, route chatRoute, body string, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, int, error) {
	buildReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequest(http.MethodPost, route.endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		route.applyHeaders(httpReq, sa, body)
		return httpReq, nil
	}
	stream, statusCode, _, err := sendChat(buildReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		payload, _ := readAllHost(stream)
		return nil, statusCode, upstreamStatusError(statusCode, chatUpstreamError(statusCode, string(payload)))
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	seenPayload := false
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		bodyStr, meaningful, frameErr := mimoUnwrapFrame(scanner.Text())
		if frameErr != nil {
			return chunks, 0, frameErr
		}
		seenPayload = seenPayload || meaningful
		if bodyStr == "" || bodyStr == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: json.RawMessage(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, 0, upstreamReadError(err)
	}
	// Empty-stream guard: a 200 envelope stream with zero payload chunks is
	// an upstream failure, not a silent success.
	if !seenPayload {
		return chunks, 0, emptyStreamError()
	}
	return chunks, 0, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// mimoUnwrapFrame inspects one upstream SSE line BEFORE forwarding.
// Returns (bodyStr, meaningful, err):
//   - err != nil: upstream error frame — abort the stream.
//   - meaningful: the line carried a real completion payload (empty-stream
//     guard input).
//   - bodyStr: the unwrapped JSON body ("" for control/non-data lines).
func mimoUnwrapFrame(line string) (string, bool, error) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "event:") {
		if strings.Contains(strings.ToLower(trimmed), "error") {
			return "", false, fmt.Errorf("mimo upstream error event")
		}
		return "", false, nil
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return "", false, nil
	}
	if payload == "[DONE]" {
		return "[DONE]", false, nil
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		// Non-JSON data line — pass through untouched; the client decides.
		return payload, true, nil
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return "", false, fmt.Errorf("mimo upstream error: %s", truncateRedacted(payload, 200))
	}
	return payload, true, nil
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (strict clients interpret these as a truncated tool call). Other
// empty-but-legal values are preserved: content:"" is a valid delta (pure
// tool-call chunk) and the role-only first chunk must survive so clients can
// establish the message role.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if v, present := delta["function_call"]; present && isEmptyValue(v) {
				delete(delta, "function_call")
				changed = true
			}
			if v, present := delta["tool_calls"]; present {
				if arr, isArr := v.([]any); isArr && len(arr) == 0 {
					delete(delta, "tool_calls")
					changed = true
				}
			}
			for _, noise := range []string{"extra_fields", "refusal"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason).
			if len(delta) == 0 {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// decodeNonStreamCompletion validates a non-streaming upstream completion
// body. The gateway answers a stream=false POST with a single chat.completion
// JSON object; an `error` field in a 200 body is an upstream failure, not a
// silent success.
func decodeNonStreamCompletion(payload []byte, model string) ([]byte, error) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return nil, emptyStreamError()
	}
	var obj struct {
		Error   json.RawMessage `json:"error"`
		Choices []any           `json:"choices"`
	}
	_ = json.Unmarshal([]byte(trimmed), &obj)
	if len(obj.Error) > 0 && string(obj.Error) != "null" {
		return nil, fmt.Errorf("mimo upstream error: %s", truncateRedacted(trimmed, 200))
	}
	// Verify it parses as a JSON object and carries the completion shape; if
	// not (e.g. a plain-text gateway error), surface it verbatim (redacted).
	var probe map[string]any
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return nil, fmt.Errorf("mimo upstream error: non-JSON body: %s", truncateRedacted(trimmed, 200))
	}
	if len(obj.Choices) == 0 {
		return nil, fmt.Errorf("mimo upstream error: body missing/empty choices: %s", truncateRedacted(trimmed, 200))
	}
	return []byte(trimmed), nil
}

// isEmptyValue reports whether v is a "zero" SSE field that should be
// stripped from outgoing chunks.
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
