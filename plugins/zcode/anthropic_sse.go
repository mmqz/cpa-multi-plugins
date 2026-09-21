// anthropic_sse.go translates the start-plan gateway's Anthropic responses
// back into the OpenAI shapes the CPA host consumes: the single non-stream
// message JSON, and the SSE event stream (message_start / content_block_*
// / message_delta / message_stop → chat.completion.chunk frames).
//
// Reference: TriDefender/zcode-api src/translator/openai-to-anthropic.ts
// (translateResponseAnthropicToOpenAI + anthropicUsageToOpenAI) and
// src/translator/sse-translator.ts (translateEvent state machine). Behavior
// notes preserved from the reference:
//   - usage folds with OVERWRITE semantics (the upstream re-reports absolute
//     totals; a later 0 wins, an omitted field keeps its previous value);
//   - the finish chunk carries the usage (a trailing choices:[] usage chunk
//     is never emitted — some clients index into it blindly);
//   - message_stop only emits a fallback finish when message_delta did not.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// mustJSONString encodes s as a JSON string literal (panic-free: encoding of
// a string cannot fail).
func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// anthropicUsage is the upstream usage block (three mutually exclusive input
// buckets + output). The counters are pointers so mergeUsage can keep the
// reference's overwrite semantics: a present 0 must win over an earlier
// non-zero, an absent field keeps its previous value.
type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens,omitempty"`
	OutputTokens             *int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
}

// usageValue dereferences an optional counter (absent = 0).
func usageValue(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// anthropicUsageToOpenAI converts Anthropic usage to OpenAI's cache-inclusive
// semantics: prompt = input + cache_read + cache_creation, with the cache
// read surfaced through prompt_tokens_details. Presence-preserving: an
// upstream explicitly reporting 0 cache reads stays distinguishable from one
// reporting no cache breakdown.
func anthropicUsageToOpenAI(u *anthropicUsage) map[string]any {
	if u == nil {
		u = &anthropicUsage{}
	}
	prompt := usageValue(u.InputTokens) + usageValue(u.CacheReadInputTokens) + usageValue(u.CacheCreationInputTokens)
	output := usageValue(u.OutputTokens)
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": output,
		"total_tokens":      prompt + output,
	}
	if u.CacheReadInputTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.CacheReadInputTokens}
	}
	return usage
}

// openAIChunkBase builds the common chat.completion chunk envelope.
func openAIChunkBase(id, model string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": timeNowUnix(),
		"model":   model,
		"choices": []any{},
	}
}

// translateAnthropicResponseToOpenAI converts a non-stream Anthropic message
// body into an OpenAI chat.completion body. Anthropic error envelopes
// ({type:"error", error:{...}}) surface as errors, never as fake completions.
func translateAnthropicResponseToOpenAI(body []byte, model string) ([]byte, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, emptyStreamError()
	}
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal([]byte(trimmed), &probe)
	if len(probe.Error) > 0 && string(probe.Error) != "null" {
		return nil, fmt.Errorf("zcode upstream error: %s", truncateRedacted(trimmed, 200))
	}
	var resp struct {
		ID         string           `json:"id"`
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      *anthropicUsage  `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
		return nil, fmt.Errorf("zcode upstream error: non-JSON body: %s", truncateRedacted(trimmed, 200))
	}
	var textParts []string
	var reasoningParts []string
	var toolCalls []map[string]any
	for _, block := range resp.Content {
		switch block["type"] {
		case "text":
			if s, _ := block["text"].(string); s != "" {
				textParts = append(textParts, s)
			}
		case "thinking":
			if s, _ := block["thinking"].(string); s != "" {
				reasoningParts = append(reasoningParts, s)
			}
		case "tool_use":
			args, _ := block["input"].(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			argsJSON, _ := json.Marshal(args)
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
		}
	}
	// An empty text join renders as JSON null (the reference: join("") || null).
	var content any = strings.Join(textParts, "")
	if content == "" {
		content = nil
	}
	message := map[string]any{"role": "assistant", "content": content}
	if len(reasoningParts) > 0 {
		message["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	completion := map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion",
		"created": timeNowUnix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapStopReasonToFinish(resp.StopReason),
		}},
		"usage": anthropicUsageToOpenAI(resp.Usage),
	}
	return json.Marshal(completion)
}

// mapStopReasonToFinish maps Anthropic stop_reason onto OpenAI finish_reason
// (end_turn|stop_sequence→stop, max_tokens→length, tool_use→tool_calls,
// unknown→null).
func mapStopReasonToFinish(stopReason string) any {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return nil
	}
}

// anthropicSSEState is one stream's translation state (sse-translator.ts
// TranslationState): the running usage snapshot, tool-call block index map,
// and the role/finish sentinels.
type anthropicSSEState struct {
	messageID      string
	model          string
	roleSent       bool
	finishSent     bool
	usage          anthropicUsage
	toolCallIndex  int
	blockToToolIdx map[int64]int
}

func newAnthropicSSEState(model string) *anthropicSSEState {
	return &anthropicSSEState{
		model:          model,
		blockToToolIdx: make(map[int64]int),
	}
}

// mergeUsage folds an incremental usage report into the snapshot with
// overwrite semantics: a PRESENT field (even 0) wins over an earlier value,
// an absent field keeps its previous value — the upstream re-reports
// absolute totals, so a later 0 is a real correction.
func (s *anthropicSSEState) mergeUsage(u *anthropicUsage) {
	if u == nil {
		return
	}
	if u.InputTokens != nil {
		s.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens != nil {
		s.usage.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		s.usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		s.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
}

// anthropicTranslateEvent folds one parsed SSE event into state and returns
// zero or more OpenAI chunk JSON strings. The event name comes from the SSE
// "event:" line; the data JSON carries its own "type" which the reference
// trusts over the line name — we do the same.
func (s *anthropicSSEState) anthropicTranslateEvent(eventType string, data []byte) ([]string, error) {
	if !json.Valid(data) {
		return nil, nil
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, nil
	}
	kind := envelope.Type
	if kind == "" {
		kind = eventType
	}
	switch kind {
	case "error":
		return nil, fmt.Errorf("zcode upstream error: %s", truncateRedacted(string(data), 200))
	case "message_start":
		return s.handleMessageStart(data), nil
	case "content_block_start":
		return s.handleContentBlockStart(data), nil
	case "content_block_delta":
		return s.handleContentBlockDelta(data), nil
	case "message_delta":
		return s.handleMessageDelta(data), nil
	case "message_stop":
		if s.finishSent {
			return nil, nil
		}
		s.finishSent = true
		return []string{s.chunkJSON(map[string]any{}, "stop", true)}, nil
	default: // ping, content_block_stop, unknown
		return nil, nil
	}
}

func (s *anthropicSSEState) handleMessageStart(data []byte) []string {
	var evt struct {
		Message struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
	}
	_ = json.Unmarshal(data, &evt)
	if evt.Message.ID != "" {
		s.messageID = evt.Message.ID
	}
	if evt.Message.Model != "" {
		s.model = evt.Message.Model
	}
	s.mergeUsage(evt.Message.Usage)
	if s.roleSent {
		return nil
	}
	s.roleSent = true
	return []string{s.chunkJSON(map[string]any{"role": "assistant"}, "", false)}
}

func (s *anthropicSSEState) handleContentBlockStart(data []byte) []string {
	var evt struct {
		Index        int64          `json:"index"`
		ContentBlock map[string]any `json:"content_block"`
	}
	if json.Unmarshal(data, &evt) != nil {
		return nil
	}
	if evt.ContentBlock["type"] != "tool_use" {
		return nil
	}
	id, _ := evt.ContentBlock["id"].(string)
	name, _ := evt.ContentBlock["name"].(string)
	myIndex := s.toolCallIndex
	s.toolCallIndex++
	s.blockToToolIdx[evt.Index] = myIndex
	return []string{s.chunkJSON(map[string]any{
		"tool_calls": []any{map[string]any{
			"index":    myIndex,
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}},
	}, "", false)}
}

func (s *anthropicSSEState) handleContentBlockDelta(data []byte) []string {
	var evt struct {
		Index int64          `json:"index"`
		Delta map[string]any `json:"delta"`
	}
	if json.Unmarshal(data, &evt) != nil {
		return nil
	}
	switch evt.Delta["type"] {
	case "text_delta":
		text, _ := evt.Delta["text"].(string)
		return []string{s.chunkJSON(map[string]any{"content": text}, "", false)}
	case "thinking_delta":
		thinking, _ := evt.Delta["thinking"].(string)
		return []string{s.chunkJSON(map[string]any{"reasoning_content": thinking}, "", false)}
	case "signature_delta":
		return nil
	case "input_json_delta":
		myIndex, ok := s.blockToToolIdx[evt.Index]
		if !ok {
			return nil
		}
		partial, _ := evt.Delta["partial_json"].(string)
		return []string{s.chunkJSON(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    myIndex,
				"function": map[string]any{"arguments": partial},
			}},
		}, "", false)}
	default:
		return nil
	}
}

func (s *anthropicSSEState) handleMessageDelta(data []byte) []string {
	var evt struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &evt) != nil {
		return nil
	}
	// Fold usage BEFORE the stop_reason branch: this is the only place the
	// real input/cache counts arrive, and the finish chunk carries them.
	s.mergeUsage(evt.Usage)
	if evt.Delta.StopReason == "" || s.finishSent {
		return nil
	}
	s.finishSent = true
	return []string{s.chunkJSON(map[string]any{}, mapStopReasonFinishStreaming(evt.Delta.StopReason), true)}
}

// chunkJSON renders one OpenAI chat.completion.chunk frame. withUsage
// attaches the converted usage snapshot (finish chunks only — a trailing
// choices:[] usage chunk is never emitted).
func (s *anthropicSSEState) chunkJSON(delta map[string]any, finish any, withUsage bool) string {
	id := s.messageID
	if id == "" {
		id = "chatcmpl-stream"
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	chunk := openAIChunkBase(id, s.model)
	chunk["choices"] = []any{choice}
	if withUsage {
		chunk["usage"] = anthropicUsageToOpenAI(&s.usage)
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return ""
	}
	return string(out)
}

// mapStopReasonFinishStreaming is the streaming-side stop mapping: unknown
// reasons degrade to "stop" (never null) so clients terminate cleanly.
func mapStopReasonFinishStreaming(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// anthropicSSEState_flushFinal renders the end-of-stream fallback: when the
// upstream ended without message_delta/message_stop (truncated stream), the
// client still needs a terminal chunk. Returns nil when a finish was sent.
func (s *anthropicSSEState) flushFinal() string {
	if s.finishSent {
		return ""
	}
	s.finishSent = true
	return s.chunkJSON(map[string]any{}, "stop", true)
}

// startPlanCaptchaChallenge detects the two 3007 captcha-challenge shapes
// the start-plan gateway throws at the hot path (captcha-retry.ts):
//  1. response-header variant — x-aliyun-captcha-verify-param on a non-2xx
//  2. in-body variant — HTTP 400 with {"code":3007} in the JSON body
//
// Our plugin has no solver (that is a third-party enhancement in the
// reference project), so a detected challenge becomes a clear terminal error.
func startPlanCaptchaChallenge(status int, headerValue, body string) bool {
	if headerValue != "" && status >= 300 {
		return true
	}
	if status == 400 {
		trimmed := strings.Join(strings.Fields(body), "")
		if strings.Contains(trimmed, `"code":3007`) {
			return true
		}
	}
	return false
}

// extractBizErrorCode tolerantly extracts the provider business code from an
// error body: top-level "code" (number or numeric string), "error.code",
// or a {"code":...} nested anywhere one level deep. Returns "" when absent.
func extractBizErrorCode(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || trimmed[0] != '{' {
		return ""
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return ""
	}
	if code := numericCode(probe["code"]); code != "" {
		return code
	}
	var nested struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(trimmed), &nested) == nil && len(nested.Error) > 0 {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested.Error, &inner) == nil {
			if code := numericCode(inner["code"]); code != "" {
				return code
			}
		}
	}
	return ""
}

// numericCode renders a JSON number/string code as its canonical digits.
func numericCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var num float64
	if json.Unmarshal(raw, &num) == nil {
		return trimFloatCode(num)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		t := strings.TrimSpace(str)
		if isDigits(t) {
			return t
		}
	}
	return ""
}

func trimFloatCode(f float64) string {
	if f != float64(int64(f)) {
		return ""
	}
	s := fmt.Sprintf("%d", int64(f))
	if isDigits(s) {
		return s
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// startPlanBizError renders a start-plan chat failure with actionable copy
// keyed off the provider business code (failure-provider-business-codes.ts):
//   - 3007 captcha challenge → no-solver terminal error
//   - 1261 context exceeded → the oversized-input guidance
//   - 1006 / 3007 auth semantics → re-login hint
//   - 1312 overload / 1302|1303|1305 rate limit → retryable note
//   - 3008-3010 concurrency caps → switch-model note
//
// The fallback is the generic rendered body.
func startPlanBizError(status int, body string, captchaHeader string) error {
	if startPlanCaptchaChallenge(status, captchaHeader, body) {
		return fmt.Errorf("start-plan 网关要求验证码挑战 (3007)：本插件未内置求解器，请稍后重试或改用 coding-plan 账号 — upstream %d: %s", status, truncateRedacted(body, 200))
	}
	code := extractBizErrorCode(body)
	switch code {
	case "1261":
		return fmt.Errorf("输入超出模型上下文窗口 (1261)：请压缩上下文/清理会话后重试 — upstream %d: %s", status, truncateRedacted(body, 200))
	case "1006":
		return fmt.Errorf("start-plan 鉴权失败 (1006)：请重新登录 — upstream %d: %s", status, truncateRedacted(body, 200))
	case "1312":
		return fmt.Errorf("上游过载 (1312)：可重试 — upstream %d: %s", status, truncateRedacted(body, 200))
	case "1302", "1303", "1305":
		return fmt.Errorf("触发限流 (%s)：可稍后重试 — upstream %d: %s", code, status, truncateRedacted(body, 200))
	case "3008", "3009", "3010", "1304", "1308", "1310", "1313":
		return fmt.Errorf("并发/限流上限 (%s)：请降低并发或切换模型 — upstream %d: %s", code, status, truncateRedacted(body, 200))
	case "3001", "3005", "3006", "1005":
		return fmt.Errorf("请求被上游拒绝 (%s)：请求参数或模型不可用 — upstream %d: %s", code, status, truncateRedacted(body, 200))
	}
	return chatUpstreamError(status, body)
}

// timeNowUnix is the created-timestamp source for translated chunks
// (Math.floor(Date.now()/1000) in the reference).
func timeNowUnix() int64 {
	return time.Now().Unix()
}

// roleString reads a message's role as a string regardless of shape.
func roleString(msg map[string]any) string {
	s, _ := msg["role"].(string)
	return s
}
