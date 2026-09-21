// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
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
// A-37 still applies: raw upstream bodies may carry Bearer/JWT — callers pass
// the message through redactSecrets first.
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
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
func pumpUpstreamStream(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, cooldownModel string) {
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

	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(errPayload))
		if authID != "" {
			recordUpstreamFailure(authID, cooldownModel, statusCode, string(errPayload))
		}
		if authUID != "" {
			go reconcileByUID(authUID, statusCode, string(errPayload))
		}
		streamEmitError(streamID, chatUpstreamError(statusCode, string(errPayload)).Error())
		return
	}
	collector := &sseUsageCollector{}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenPayload := false
	for scanner.Scan() {
		bodyStr, meaningful, frameErr := qoderUnwrapFrame(scanner.Text())
		if frameErr != nil {
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, frameErr.Error())
			// v0.12.76: envelope errors also feed the cooldown table — every
			// other failure site does; without this a streaming-path 429
			// envelope never cools the (auth, model) pair.
			if authID != "" {
				recordUpstreamFailure(authID, cooldownModel, 0, frameErr.Error())
			}
			streamEmitError(streamID, frameErr.Error())
			return
		}
		seenPayload = seenPayload || meaningful
		if bodyStr == "" || bodyStr == "[DONE]" {
			continue
		}
		collector.feed(bodyStr)
		cleaned := cleanChunkJSON(bodyStr)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort; do not report success.
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error())
			return
		}
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame and record the attempt as failed.
	if err := scanner.Err(); err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, err.Error())
		if authID != "" {
			recordUpstreamFailure(authID, cooldownModel, 0, err.Error())
		}
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", err))
		return
	}
	// v0.8.18: the async pump finally gets the same empty-stream guard the
	// synchronous collector has had since v0.8.17 — a 200 envelope stream
	// with zero payload is an upstream failure, not a silent success. (The
	// seenPayload tracking used to be dead code here.)
	if !seenPayload {
		errEmpty := emptyStreamError()
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, errEmpty.Error())
		if authID != "" {
			recordUpstreamFailure(authID, cooldownModel, 0, errEmpty.Error())
		}
		streamEmitError(streamID, errEmpty.Error())
		return
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
}

// collectUpstreamStreamQoder is the QoderWork-flavoured synchronous fallback
// (no async stream id): drain the upstream nested SSE, unwrap the inner
// OpenAI chunks, return them as a slice. The collector, when non-nil,
// observes the unwrapped inner chunks for usage extraction.
func collectUpstreamStreamQoder(encodedBody string, sa *storedAuth, modelKey string, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), strings.NewReader(encodedBody))
	if err != nil {
		return nil, 0, err
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, endpointChatFor(sa), modelKey, true); err != nil {
		return nil, 0, fmt.Errorf("cosy: %w", err)
	}
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		// 0.8.13: account-level statuses ride the error envelope so the host
		// cooldown layer stops re-picking a drained credential.
		return nil, statusCode, upstreamStatusError(statusCode, chatUpstreamError(statusCode, string(payload)))
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenPayload := false
	for scanner.Scan() {
		bodyStr, meaningful, frameErr := qoderUnwrapFrame(scanner.Text())
		if frameErr != nil {
			return chunks, 0, frameErr
		}
		seenPayload = seenPayload || meaningful
		if bodyStr == "" || bodyStr == "[DONE]" {
			continue
		}
		if collector != nil {
			collector.feed(bodyStr)
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
	// v0.8.17 empty-stream guard: a 200 envelope stream with zero payload
	// chunks is an upstream failure, not a silent success.
	if !seenPayload {
		return chunks, 0, emptyStreamError()
	}
	return chunks, 0, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (QoderWork emits these on the terminal chunk, and strict clients interpret
// them as a truncated tool call). Other empty-but-legal values are preserved:
// content:"" is a valid delta (pure tool-call chunk) and the role-only first
// chunk must survive so clients can establish the message role.
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
			// Upstream often pads terminal/noop deltas with empty noise fields
			// that clients ignore but pollute wire size / some parsers.
			for _, noise := range []string{"extra_fields", "refusal", "reasoning_content"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason): e.g. {"delta":{"function_call":null}}
			// reduced to {}. A delta with role/content:"" is meaningful and
			// never reaches this branch (those fields are preserved above).
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

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	// tool_calls arrive as streaming deltas: each chunk carries an index plus a
	// partial call (id/type/function.name on the first delta, argument text
	// fragments afterwards). Merge by index instead of appending raw fragments
	// so the folded completion holds whole calls.
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	var scanErr error

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	// A mid-stream read failure means the folded completion is truncated. The
	// host discards the payload entirely when the plugin returns an error
	// (sdk/api/handlers executeWithPluginExecutor), so fail fast here instead
	// of assembling a partial completion nobody can safely consume.
	if scanErr != nil {
		return nil, upstreamReadError(scanErr)
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-qoderwork"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// aggregateQoderSSE folds QoderWork's nested SSE stream into a single
// OpenAI chat.completion object. The gateway emits frames shaped:
//
//	data:{"headers":{...},"body":"<json-string>","statusCodeValue":200,...}
//
// where `body` is a JSON-encoded string that itself contains an OpenAI-style
// chunk: {"choices":[{"delta":{...}}],...}. We unwrap the outer envelope,
// then aggregate the inner chunks using the same logic as aggregateCompletion.
//
// Terminal frames: data:{"body":"[DONE]"} followed by an event:finish line
// with timing metadata (ignored).
func aggregateQoderSSE(r io.Reader, model string) ([]byte, error) {
	// Unwrap the nested SSE into an inner plain-text stream of OpenAI chunks,
	// then delegate to aggregateCompletion. We materialise the inner stream
	// into memory because the gateway's SSE is short-lived (one chat call)
	// and the inner stream is at most a few hundred KB.
	var inner strings.Builder
	seenPayload := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		// v0.12.76: route every line through qoderUnwrapFrame so a 200-OK
		// error envelope (statusCodeValue>=400 / body carrying {"error":...})
		// surfaces as an error instead of being folded into a synthetic
		// empty completion — the two streaming paths already do this.
		bodyStr, meaningful, frameErr := qoderUnwrapFrame(scanner.Text())
		if frameErr != nil {
			return nil, frameErr
		}
		if bodyStr == "[DONE]" {
			break
		}
		seenPayload = seenPayload || meaningful
		if bodyStr == "" {
			continue
		}
		// Re-emit the inner JSON as a standard "data:<json>\n" SSE frame so
		// aggregateCompletion's parser can consume it unchanged.
		inner.WriteString("data:")
		inner.WriteString(bodyStr)
		inner.WriteString("\n\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("qoder SSE read: %w", err)
	}
	// v0.8.18: third pump, same guard. Without it an empty 200 stream folds
	// into a synthetic chatcmpl-qoderwork completion with empty content and
	// finish_reason "stop" — a silent fake success on the non-stream path.
	if !seenPayload {
		return nil, emptyStreamError()
	}
	return aggregateCompletion(strings.NewReader(inner.String()), model)
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// concatenated (upstream may split it), and function.arguments text fragments
// are appended in arrival order.
func mergeToolCallDelta(merged, delta map[string]any) {
	for _, k := range []string{"id", "type"} {
		if _, present := merged[k]; !present {
			if v, ok := delta[k].(string); ok && v != "" {
				merged[k] = v
			}
		}
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	mfn, _ := merged["function"].(map[string]any)
	if mfn == nil {
		mfn = map[string]any{}
		merged["function"] = mfn
	}
	if v, ok := dfn["name"].(string); ok && v != "" {
		cur, _ := mfn["name"].(string)
		mfn["name"] = cur + v
	}
	if v, ok := dfn["arguments"].(string); ok && v != "" {
		cur, _ := mfn["arguments"].(string)
		mfn["arguments"] = cur + v
	}
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// qoderUnwrapFrame inspects one upstream SSE line BEFORE translation.
// v0.8.17 (fork libo0118/qoder-custom review): the pumps used to read only
// the envelope's "body" string and ignore both "statusCodeValue" and any
// "error" field inside the body — an upstream error delivered as a 200-OK
// envelope was silently swallowed and the stream ended looking successful.
// Returns (bodyStr, meaningful, err):
//   - err != nil: upstream error frame — abort the stream, record failure.
//   - meaningful: the line carried a real completion payload (empty-stream
//     guard input).
//   - bodyStr: the unwrapped inner body ("" for control/non-data lines).
func qoderUnwrapFrame(line string) (string, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "event:error" || trimmed == "event: error" {
		return "", false, fmt.Errorf("qoder upstream error event")
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	var outer struct {
		Body   json.RawMessage `json:"body"`
		Status int             `json:"statusCodeValue"`
	}
	if json.Unmarshal([]byte(payload), &outer) != nil {
		return "", false, nil
	}
	body := payload
	if len(outer.Body) > 0 {
		inner := string(outer.Body)
		if json.Unmarshal(outer.Body, &body) != nil {
			body = inner
		}
	}
	if strings.TrimSpace(body) == "[DONE]" {
		return "[DONE]", false, nil
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &chunk)
	if outer.Status >= 400 || (len(chunk.Error) > 0 && string(chunk.Error) != "null") {
		return "", false, fmt.Errorf("qoder upstream error: %s", truncateRedacted(body, 200))
	}
	return body, true, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isEmptyValue reports whether v is a "zero" SSE field that should be
// stripped from outgoing chunks. Used by cleanChunkJSON to drop the legacy
// function_call shell ({"name":"","arguments":""}) some upstreams emit on
// the terminal chunk — clients that strictly validate the delta will reject
// the shell as an invalid tool call.
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
