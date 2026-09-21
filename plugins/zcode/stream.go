// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
//
// The coding-plan OpenAI-compatible gateway emits standard OpenAI SSE frames
// (`data: {chunk}` … `data: [DONE]`). Upstream errors can still arrive as
// 200-OK bodies carrying an `error` field — zcodeUnwrapFrame catches those so
// a 200 envelope never folds into a silent fake success (the guard symmetry
// the qoder/workbuddy plugins learned the hard way).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// Raw upstream bodies may carry Bearer/JWT — callers pass the message through
// redactSecrets first.
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
// route selects the wire dialect: coding-plan OpenAI frames pass through the
// existing unwrap/clean path; start-plan anthropic events go through the
// translation state machine (anthropic_sse.go) first, so the host only ever
// sees OpenAI chunk JSON.
func pumpUpstreamStream(ctx context.Context, sa *storedAuth, route chatRoute, body string, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, cooldownModel string) {
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

	stream, statusCode, respHeaders, err := sendChatWithSigning(sa, buildReq)
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		errBody := string(errPayload)
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errBody)
		if authID != "" {
			recordUpstreamFailure(authID, cooldownModel, statusCode, errBody)
		}
		if authUID != "" {
			go reconcileByUID(authUID, statusCode, errBody)
		}
		streamEmitError(streamID, routeChatError(route, sa, statusCode, respHeaders, errBody).Error())
		return
	}
	collector := &sseUsageCollector{}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenPayload := false

	// Anthropic dialect: translate events first, then run the translated
	// OpenAI chunks through the same clean/emit path.
	var anthropicState *anthropicSSEState
	var pendingEvent string
	if route.anthropic {
		anthropicState = newAnthropicSSEState(upstreamModel)
	}

	for scanner.Scan() {
		if anthropicState != nil {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "event:") {
				pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			outs, terr := anthropicState.anthropicTranslateEvent(pendingEvent, []byte(payload))
			pendingEvent = ""
			if terr != nil {
				publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, terr.Error())
				if authID != "" {
					recordUpstreamFailure(authID, cooldownModel, 0, terr.Error())
				}
				streamEmitError(streamID, terr.Error())
				return
			}
			for _, out := range outs {
				if out == "" {
					continue
				}
				seenPayload = true
				collector.feed(out)
				cleaned := cleanChunkJSON(out)
				if cleaned == "" {
					continue
				}
				if sseFramed {
					cleaned = "data: " + cleaned
				}
				if err := streamEmit(streamID, []byte(cleaned)); err != nil {
					publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error())
					return
				}
			}
			continue
		}
		bodyStr, meaningful, frameErr := zcodeUnwrapFrame(scanner.Text())
		if frameErr != nil {
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, frameErr.Error())
			// Envelope errors also feed the cooldown table — every other
			// failure site does; without this a streaming-path 429 envelope
			// never cools the (auth, model) pair.
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
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error (unexpected EOF): %v", err))
		return
	}
	// Empty-stream guard: a 200 envelope stream with zero payload is an
	// upstream failure, not a silent success (all three paths symmetric).
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

// collectUpstreamStream is the synchronous fallback (no async stream id):
// drain the upstream SSE, return the cleaned chunks as a slice. The
// collector, when non-nil, observes the chunks for usage extraction. The
// anthropic dialect is translated to OpenAI chunks before collection.
func collectUpstreamStream(body string, sa *storedAuth, route chatRoute, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	buildReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequest(http.MethodPost, route.endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		route.applyHeaders(httpReq, sa, body)
		return httpReq, nil
	}
	stream, statusCode, respHeaders, err := sendChatWithSigning(sa, buildReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		return nil, statusCode, upstreamStatusError(statusCode, routeChatError(route, sa, statusCode, respHeaders, string(payload)))
	}
	var anthropicState *anthropicSSEState
	var pendingEvent string
	if route.anthropic {
		anthropicState = newAnthropicSSEState("")
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	seenPayload := false
	drain := func(outs []string) {
		for _, out := range outs {
			if out == "" {
				continue
			}
			seenPayload = true
			if collector != nil {
				collector.feed(out)
			}
			cleaned := cleanChunkJSON(out)
			if cleaned == "" {
				continue
			}
			if sseFramed {
				cleaned = "data: " + cleaned
			}
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: json.RawMessage(cleaned)})
		}
	}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if anthropicState != nil {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "event:") {
				pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			outs, terr := anthropicState.anthropicTranslateEvent(pendingEvent, []byte(payload))
			pendingEvent = ""
			if terr != nil {
				return chunks, 0, terr
			}
			drain(outs)
			continue
		}
		bodyStr, meaningful, frameErr := zcodeUnwrapFrame(scanner.Text())
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
	if anthropicState != nil {
		if fin := anthropicState.flushFinal(); fin != "" {
			drain([]string{fin})
		}
	}
	// Empty-stream guard: a 200 envelope stream with zero payload chunks is
	// an upstream failure, not a silent success.
	if !seenPayload {
		return chunks, 0, emptyStreamError()
	}
	return chunks, 0, nil
}

// routeChatError renders one upstream chat failure for the client, in the
// dialect the route speaks: start-plan failures get the provider business
// code treatment (3007 captcha / 1261 context / auth semantics) and the JWT
// 401 gets the re-login hint; coding-plan failures keep the existing generic
// rendering. The response headers ride along because the captcha challenge's
// primary variant is a response header.
func routeChatError(route chatRoute, sa *storedAuth, status int, headers http.Header, body string) error {
	if route.anthropic {
		var captchaHeader string
		if headers != nil {
			captchaHeader = strings.TrimSpace(headers.Get("x-aliyun-captcha-verify-param"))
		}
		if status == http.StatusUnauthorized {
			return fmt.Errorf("start-plan JWT 被网关拒绝 (401)：请重新登录 — %s", truncateRedacted(body, 200))
		}
		return startPlanBizError(status, body, captchaHeader)
	}
	return chatUpstreamError(status, body)
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

// decodeNonStreamCompletion validates and normalizes a non-streaming upstream
// completion body. The gateway answers a stream=false POST with a single
// chat.completion JSON object; an `error` field in a 200 body is an upstream
// failure, not a silent success.
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
		return nil, fmt.Errorf("zcode upstream error: %s", truncateRedacted(trimmed, 200))
	}
	// Verify it parses as a JSON object and carries the completion shape; if
	// not (e.g. a plain-text gateway error), surface it verbatim (redacted).
	var probe map[string]any
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return nil, fmt.Errorf("zcode upstream error: non-JSON body: %s", truncateRedacted(trimmed, 200))
	}
	if len(obj.Choices) == 0 {
		return nil, fmt.Errorf("zcode upstream error: body missing/empty choices: %s", truncateRedacted(trimmed, 200))
	}
	return []byte(trimmed), nil
}

// zcodeUnwrapFrame inspects one upstream SSE line BEFORE translation.
// Returns (bodyStr, meaningful, err):
//   - err != nil: upstream error frame — abort the stream, record failure.
//   - meaningful: the line carried a real completion payload (empty-stream
//     guard input).
//   - bodyStr: the unwrapped JSON body ("" for control/non-data lines).
func zcodeUnwrapFrame(line string) (string, bool, error) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "event:") {
		if strings.Contains(strings.ToLower(trimmed), "error") {
			return "", false, fmt.Errorf("zcode upstream error event")
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
		return "", false, fmt.Errorf("zcode upstream error: %s", truncateRedacted(payload, 200))
	}
	return payload, true, nil
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
