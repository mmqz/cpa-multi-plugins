// anthropic.go translates the host's OpenAI chat-completions payload into the
// start-plan gateway's Anthropic-messages body and applies the start-plan
// body transformations the real ZCode client always performs.
//
// Upstream contract (TriDefender/zcode-api, mirrored file by file):
//   - src/translator/openai-to-anthropic.ts — the request translator
//     (message/tool coalescing, thinking compat, GLM-5.3 effort channel,
//     catalog max_tokens defaults)
//   - src/proxy/body-transformer.ts — start-plan system-block prepend,
//     context_prefix user turn, cache_control canonicalization,
//     metadata.user_id injection
//   - src/provider/reasoning.ts — the GLM-5.3 effort/budget pairing (the
//     Anthropic upstream ignores bare reasoning_effort for this family;
//     output_config.effort is the only channel that changes anything)
//
// The gateway rejects start-plan bodies without the official ZCode system
// blocks with biz 3012 "method not allowed", so the system prepend is not
// optional polish — it is the admission ticket.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// anthropicVersion is the gateway's required anthropic-version header value.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is the fallback when the OpenAI request omits max_tokens
// and the model is not in the catalog.
const defaultMaxTokens = 4096

// glm53MinThinkingBudget is the floor below which a thinking budget stops
// being useful (measured live by the reference project) — doubles as the SDK
// default budget forced whenever thinking is enabled without one.
const glm53MinThinkingBudget = 1024

// glm53ThinkingBudgets pairs each effort level with the catalog budget.
// Sending output_config.effort without a matching thinking.budget_tokens
// leaves the upstream at its own near-zero default.
var glm53ThinkingBudgets = map[string]int{"low": 8000, "high": 16000, "max": 32000}

// glm53DefaultEffort is ZCode's catalog defaultLevel for the family.
const glm53DefaultEffort = "max"

// startPlanModelIDs is the model set the start-plan gateway officially
// serves (GLM-5.3-Flash / 5.2 / 5-Turbo — claimed trial plans included).
var startPlanModelIDs = map[string]struct{}{
	"glm-5.3-flash": {},
	"glm-5.2":       {},
	"glm-5-turbo":   {},
}

// modelSpec mirrors the catalog row fields the translator needs.
type modelSpec struct {
	maxOutputTokens int64
	reasoning       bool
}

// catalogSpecs is the pinned GLM catalog (models.ts) — same rows as
// zcodeModels() in models.go, keyed for the translator.
var catalogSpecs = map[string]modelSpec{
	"glm-4.5-air":   {maxOutputTokens: 98304, reasoning: true},
	"glm-4.6":       {maxOutputTokens: 131072, reasoning: true},
	"glm-4.6v":      {maxOutputTokens: 32768},
	"glm-4.7":       {maxOutputTokens: 131072, reasoning: true},
	"glm-5":         {maxOutputTokens: 64000, reasoning: true},
	"glm-5-turbo":   {maxOutputTokens: 64000, reasoning: true},
	"glm-5v-turbo":  {maxOutputTokens: 131072},
	"glm-5.1":       {maxOutputTokens: 64000, reasoning: true},
	"glm-5.2":       {maxOutputTokens: 128000, reasoning: true},
	"glm-5.3":       {maxOutputTokens: 128000, reasoning: true},
	"glm-5.3-flash": {maxOutputTokens: 128000, reasoning: true},
}

func catalogLookup(model string) (modelSpec, bool) {
	s, ok := catalogSpecs[strings.ToLower(strings.TrimSpace(model))]
	return s, ok
}

// isGlm53Model matches the GLM-5.3 family (glm-5.3, glm-5.3-flash) like the
// reference pattern /glm-5\.3(?![0-9])/i — a trailing digit (glm-5.30) must
// not match; glm-5 / glm-5.1 / glm-5.2 never match.
func isGlm53Model(model string) bool {
	lower := strings.ToLower(model)
	i := strings.Index(lower, "glm-5.3")
	if i < 0 {
		return false
	}
	rest := lower[i+len("glm-5.3"):]
	return rest == "" || rest[0] < '0' || rest[0] > '9'
}

// normalizeGlm53Effort maps OpenAI reasoning_effort onto the three legal
// GLM-5.3 effort levels per Z.AI's official mapping table, rounding UP
// (medium→high). Unrecognized values fall back to the catalog default (max).
func normalizeGlm53Effort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "light", "low":
		return "low"
	case "medium", "high":
		return "high"
	case "xhigh", "max", "ultra":
		return "max"
	default:
		return glm53DefaultEffort
	}
}

// buildAnthropicMetadataUserId renders the metadata.user_id blob the real
// client attaches to EVERY anthropic-kind request (bundle `E2e`/`UIo`):
//
//	{"device_id": deviceMid, "account_uuid": "", "session_id": ""}
//
// account_uuid is ALWAYS "" (hardcoded empty in the real bundle — never the
// account uuid). session_id strips the sess_/subagent_agent_ prefixes and is
// "" when no session context exists (our per-request trace UUIDs are not
// client sessions, so we pass "").
func buildAnthropicMetadataUserId(deviceMid string) string {
	return fmt.Sprintf(`{"device_id":%s,"account_uuid":"","session_id":""}`, mustJSONString(strings.TrimSpace(deviceMid)))
}

// openAIMessage is the permissive OpenAI chat message shape the translator
// consumes (content may be a string or a parts array; tool fields optional).
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type openAIToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// translateOpenAIToAnthropicBody is the full start-plan body pipeline:
// OpenAI payload → Anthropic messages body → start-plan transform (official
// system blocks, context_prefix turn, cache_control canonicalization,
// metadata.user_id). stream pins the body's stream flag.
func translateOpenAIToAnthropicBody(payload []byte, upstreamModel string, stream bool, provider, deviceMid string, now time.Time) ([]byte, error) {
	base, userSystem, err := translateOpenAIToAnthropic(payload, upstreamModel, stream)
	if err != nil {
		return nil, err
	}
	applyStartPlanTransform(base, userSystem, upstreamModel, provider, deviceMid, now)
	out, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// translateOpenAIToAnthropic performs the format translation proper and
// returns the anthropic body (system NOT yet replaced) plus the request's own
// system blocks for the caller to append after the official ones.
func translateOpenAIToAnthropic(payload []byte, upstreamModel string, stream bool) (map[string]any, []systemBlock, error) {
	if len(payload) == 0 {
		return nil, nil, fmt.Errorf("empty chat payload")
	}
	var req struct {
		Messages        []openAIMessage `json:"messages"`
		MaxTokens       *int64          `json:"max_tokens"`
		Temperature     *float64        `json:"temperature"`
		TopP            *float64        `json:"top_p"`
		Stop            json.RawMessage `json:"stop"`
		Tools           []openAIToolDef `json:"tools"`
		ToolChoice      json.RawMessage `json:"tool_choice"`
		ReasoningEffort string          `json:"reasoning_effort"`
		Thinking        map[string]any  `json:"thinking"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, nil, fmt.Errorf("payload parse: %w", err)
	}

	var systemTexts []string
	var nonSystem []openAIMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemTexts = append(systemTexts, messageText(m))
			continue
		}
		nonSystem = append(nonSystem, m)
	}
	messages := translateMessagesWithToolCoalescing(nonSystem)

	maxTokens := int64(defaultMaxTokens)
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	} else if spec, ok := catalogLookup(upstreamModel); ok {
		maxTokens = spec.maxOutputTokens
	}

	body := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if len(systemTexts) > 0 {
		body["system"] = strings.Join(systemTexts, "\n\n")
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if stop := stopSequences(req.Stop); len(stop) > 0 {
		body["stop_sequences"] = stop
	}

	applyThinkingTranslation(body, upstreamModel, req.ReasoningEffort, req.Thinking)

	if len(req.Tools) > 0 && !toolChoiceIsNone(req.ToolChoice) {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{"name": t.Function.Name}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
				tool["input_schema"] = t.Function.Parameters
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if choice := translateToolChoice(req.ToolChoice); choice != nil {
		body["tool_choice"] = choice
	}

	userSystem := []systemBlock{}
	if sys, ok := body["system"].(string); ok && strings.TrimSpace(sys) != "" {
		userSystem = []systemBlock{{Type: "text", Text: sys}}
	}
	delete(body, "system")
	return body, userSystem, nil
}

// applyStartPlanTransform mutates the translated body into the shape the
// start-plan gateway expects (body-transformer.ts with startPlan: true,
// format: anthropic): official system blocks, context_prefix user turn,
// cache_control canonicalization, tools marker strip, metadata.user_id.
func applyStartPlanTransform(body map[string]any, userSystem []systemBlock, currentModel, provider, deviceMid string, now time.Time) {
	body["system"] = buildStartPlanSystem(userSystem, currentModel, resolveEnvPromptInfo(), provider)
	if msgs, ok := body["messages"].([]map[string]any); ok && len(msgs) > 0 {
		prefix := buildContextPrefixMessage(now)
		body["messages"] = append([]map[string]any{prefix}, msgs...)
	}
	if tools, ok := body["tools"].([]map[string]any); ok {
		for _, t := range tools {
			delete(t, "cache_control")
		}
	}
	applyAnthropicCacheControl(body)
	body["metadata"] = map[string]any{"user_id": buildAnthropicMetadataUserId(deviceMid)}
}

// applyAnthropicCacheControl mirrors the bundle's two-phase pass (`zsi` clear
// + `Fsi` mark): strip stale cache_control from every content block of every
// non-system message, then mark the LAST content block of the LAST
// non-system message {type:"ephemeral"} (string content converts to a block
// array first). The top-level system field is untouched.
func applyAnthropicCacheControl(body map[string]any) {
	raw, ok := body["messages"].([]map[string]any)
	if !ok || len(raw) == 0 {
		return
	}
	for _, msg := range raw {
		if roleString(msg) == "system" {
			continue
		}
		clearCacheControlMarkers(msg)
	}
	for i := len(raw) - 1; i >= 0; i-- {
		msg := raw[i]
		if roleString(msg) == "system" {
			continue
		}
		markLastBlockEphemeral(msg)
		return
	}
}

// clearCacheControlMarkers deletes cache_control from every block of a
// message whose content is a block array (both in-memory map shapes).
func clearCacheControlMarkers(msg map[string]any) {
	switch blocks := msg["content"].(type) {
	case []any:
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok {
				delete(block, "cache_control")
			}
		}
	case []map[string]any:
		for _, block := range blocks {
			delete(block, "cache_control")
		}
	}
}

// markLastBlockEphemeral marks the message's last content block with the
// ephemeral cache breakpoint, converting plain-string content to a block
// array first (the previous behavior the bundle keeps).
func markLastBlockEphemeral(msg map[string]any) {
	switch content := msg["content"].(type) {
	case string:
		msg["content"] = []map[string]any{
			{"type": "text", "text": content, "cache_control": map[string]string{"type": "ephemeral"}},
		}
	case []map[string]any:
		if len(content) == 0 {
			return
		}
		last := content[len(content)-1]
		if _, marked := last["cache_control"]; !marked {
			last["cache_control"] = map[string]string{"type": "ephemeral"}
		}
	case []any:
		if len(content) == 0 {
			return
		}
		if last, ok := content[len(content)-1].(map[string]any); ok {
			if _, marked := last["cache_control"]; !marked {
				last["cache_control"] = map[string]string{"type": "ephemeral"}
			}
		}
	}
}

// translateMessagesWithToolCoalescing converts non-system OpenAI messages
// into Anthropic messages: consecutive role:"tool" messages coalesce into a
// single user turn of tool_result blocks (the parallel-tool-results shape
// Anthropic expects).
func translateMessagesWithToolCoalescing(messages []openAIMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	i := 0
	for i < len(messages) {
		m := messages[i]
		if m.Role == "tool" && m.ToolCallID != "" {
			results := make([]map[string]any, 0, 4)
			for i < len(messages) {
				tool := messages[i]
				if tool.Role != "tool" || tool.ToolCallID == "" {
					break
				}
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": tool.ToolCallID,
					"content":     toolResultContent(tool),
				})
				i++
			}
			out = append(out, map[string]any{"role": "user", "content": results})
			continue
		}
		out = append(out, translateMessageOpenAIToAnthropic(m))
		i++
	}
	return out
}

// translateMessageOpenAIToAnthropic converts one message: an assistant turn
// with tool_calls becomes text + tool_use blocks; anything else maps
// user/assistant onto itself.
func translateMessageOpenAIToAnthropic(msg openAIMessage) map[string]any {
	if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
		blocks := make([]map[string]any, 0, len(msg.ToolCalls)+1)
		if text := messageText(msg); text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range msg.ToolCalls {
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": parseToolArguments(tc.Function.Arguments),
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}
	}
	role := "user"
	if msg.Role == "assistant" {
		role = "assistant"
	}
	return map[string]any{"role": role, "content": translateContentOpenAIToAnthropic(msg)}
}

// translateContentOpenAIToAnthropic maps OpenAI content (string or parts) to
// Anthropic content: text passes through; image_url parts become base64 or
// url image blocks (exotic URLs degrade to a text block carrying the URL
// verbatim rather than a block the upstream would reject); unknown part
// types degrade to empty text blocks.
func translateContentOpenAIToAnthropic(msg openAIMessage) any {
	if len(msg.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return text
	}
	var parts []map[string]any
	if json.Unmarshal(msg.Content, &parts) != nil {
		return ""
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p["type"] {
		case "text":
			text, _ := p["text"].(string)
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		case "image_url":
			if iu, ok := p["image_url"].(map[string]any); ok {
				if url, _ := iu["url"].(string); url != "" {
					blocks = append(blocks, imageURLToAnthropicBlock(url))
					continue
				}
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
	}
	return blocks
}

// imageURLToAnthropicBlock maps an OpenAI image_url string onto an Anthropic
// image block: data: base64 URLs become base64 sources, http(s) URLs become
// url sources, anything else degrades to a text block. detail is dropped.
func imageURLToAnthropicBlock(url string) map[string]any {
	if mediaType, data, ok := parseDataURL(url); ok {
		return map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
		}
	}
	lower := strings.ToLower(url)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
	}
	return map[string]any{"type": "text", "text": url}
}

// parseDataURL splits `data:<media>;base64,<payload>`.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	semi := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semi < 0 || comma < semi+1 {
		return "", "", false
	}
	if rest[semi+1:comma] != "base64" {
		return "", "", false
	}
	return rest[:semi], rest[comma+1:], true
}

// toolResultContent renders a tool result's content: plain text when every
// part is text (the common shape), else per-part blocks.
func toolResultContent(msg openAIMessage) any {
	if len(msg.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return text
	}
	var parts []map[string]any
	if json.Unmarshal(msg.Content, &parts) != nil {
		return ""
	}
	allText := true
	for _, p := range parts {
		if p["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		joined := make([]string, 0, len(parts))
		for _, p := range parts {
			s, _ := p["text"].(string)
			joined = append(joined, s)
		}
		return strings.Join(joined, "")
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p["type"] == "text" {
			s, _ := p["text"].(string)
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
			continue
		}
		if p["type"] == "image_url" {
			if iu, ok := p["image_url"].(map[string]any); ok {
				if url, _ := iu["url"].(string); url != "" {
					blocks = append(blocks, imageURLToAnthropicBlock(url))
					continue
				}
			}
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// parseToolArguments parses a tool call's JSON arguments into an object;
// malformed or non-object payloads degrade to {} (never a rejected request).
func parseToolArguments(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(trimmed), &parsed) != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// messageText extracts a message's text content (string or text parts joined).
func messageText(msg openAIMessage) string {
	if len(msg.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return text
	}
	var parts []map[string]any
	if json.Unmarshal(msg.Content, &parts) != nil {
		return ""
	}
	joined := make([]string, 0, len(parts))
	for _, p := range parts {
		if p["type"] == "text" {
			s, _ := p["text"].(string)
			joined = append(joined, s)
		}
	}
	return strings.Join(joined, "")
}

// stopSequences normalizes the OpenAI stop field (string | array) onto
// Anthropic's stop_sequences.
func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		out := make([]string, 0, len(many))
		for _, s := range many {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// toolChoiceIsNone reports tool_choice === "none" (tools must be dropped).
func toolChoiceIsNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "none"
	}
	return false
}

// translateToolChoice maps OpenAI tool_choice onto Anthropic's:
// auto→auto, required→any, {function:{name}}→{type:"tool",name}. none never
// reaches here (tools are dropped); unknown shapes return nil (omit).
func translateToolChoice(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "function" && obj.Function.Name != "" {
		return map[string]any{"type": "tool", "name": obj.Function.Name}
	}
	return nil
}

// applyThinkingTranslation applies the request's reasoning controls
// (translateThinking / translateGlm53Reasoning + applyAnthropicThinkingCompat
// in openai-to-anthropic.ts):
//   - GLM-5.3 family → the output_config.effort channel paired with a
//     thinking budget (bare reasoning_effort is ignored upstream); an
//     explicit thinking:{type:"disabled"} forwards as-is.
//   - other reasoning models → thinking enabled with the SDK default 1024
//     budget when the request doesn't say otherwise.
//   - compat post-pass (exactly like the SDK): thinking enabled →
//     temperature/top_k/top_p voided, missing budget defaulted to 1024,
//     max_tokens += budget clamped to the model ceiling; no thinking +
//     temperature + top_p both set → top_p voided.
func applyThinkingTranslation(body map[string]any, model, reasoningEffort string, thinking map[string]any) {
	var thinkingValue map[string]any
	var outputConfig map[string]any
	if isGlm53Model(model) {
		if t, ok := thinking["type"].(string); ok && t == "disabled" {
			thinkingValue = map[string]any{"type": "disabled"}
		} else {
			effort := normalizeGlm53Effort(reasoningEffort)
			budget := glm53ThinkingBudgets[effort]
			if thinking != nil {
				switch thinking["type"] {
				case "enabled", "adaptive":
					if b := budgetFromThinking(thinking); b > 0 {
						budget = b
					}
				}
			}
			if spec, ok := catalogLookup(model); ok {
				if floatBudget := float64(budget); floatBudget > float64(spec.maxOutputTokens)-1 {
					budget = int(math.Floor(float64(spec.maxOutputTokens) - 1))
				}
			}
			thinkingValue = map[string]any{"type": "enabled", "budget_tokens": budget}
			outputConfig = map[string]any{"effort": effort}
		}
	} else {
		explicitDisabled := false
		if t, ok := thinking["type"].(string); ok {
			explicitDisabled = t == "disabled"
		}
		if reasoningEffort == "none" && !explicitDisabled {
			thinkingValue = map[string]any{"type": "disabled"}
		} else if thinking != nil {
			switch thinking["type"] {
			case "disabled":
				thinkingValue = map[string]any{"type": "disabled"}
			case "enabled", "adaptive":
				tv := map[string]any{"type": thinking["type"]}
				if b := budgetFromThinking(thinking); b > 0 {
					tv["budget_tokens"] = b
				}
				if d, ok := thinking["display"].(bool); ok && thinking["type"] == "adaptive" {
					tv["display"] = d
				}
				thinkingValue = tv
			}
		} else if spec, ok := catalogLookup(model); ok && spec.reasoning {
			thinkingValue = map[string]any{"type": "enabled", "budget_tokens": glm53MinThinkingBudget}
		}
	}

	// Compat post-pass — thinking enabled/adaptive:
	if thinkingValue != nil {
		enabled := false
		if t, _ := thinkingValue["type"].(string); t == "enabled" || t == "adaptive" {
			enabled = true
		}
		if enabled {
			budget := 0
			if b, ok := thinkingValue["budget_tokens"].(int); ok && b > 0 {
				budget = b
			} else {
				budget = glm53MinThinkingBudget
				thinkingValue["budget_tokens"] = budget
			}
			delete(body, "temperature")
			delete(body, "top_k")
			delete(body, "top_p")
			if maxTokens, ok := body["max_tokens"].(int64); ok {
				total := maxTokens + int64(budget)
				if spec, ok := catalogLookup(model); ok && total > spec.maxOutputTokens {
					total = spec.maxOutputTokens
				}
				body["max_tokens"] = total
			}
		} else if _, hasTemp := body["temperature"]; hasTemp {
			if _, hasTopP := body["top_p"]; hasTopP {
				delete(body, "top_p")
			}
		}
	} else if _, hasTemp := body["temperature"]; hasTemp {
		if _, hasTopP := body["top_p"]; hasTopP {
			delete(body, "top_p")
		}
	}
	body["thinking"] = thinkingValue
	if outputConfig != nil {
		body["output_config"] = outputConfig
	}
}

// budgetFromThinking extracts budget_tokens/budgetTokens (floored) from an
// explicit thinking object; fractional budgets floor BEFORE the positivity
// test so 0.5 can never produce budget_tokens: 0.
func budgetFromThinking(thinking map[string]any) int {
	for _, key := range []string{"budget_tokens", "budgetTokens"} {
		if v, ok := thinking[key].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			floored := int(math.Floor(v))
			if floored > 0 {
				return floored
			}
		}
	}
	return 0
}
