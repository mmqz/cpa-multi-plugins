// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names (human-friendly aliases plus
// legacy keys from the pre-merge CN/Intl plugins) to the upstream keys the
// Qoder gateway recognises. Unknown names pass through unchanged (the server
// silently routes them to auto).
//
// Upstream chat-scene catalog as of 2026-09: auto, ultimate, performance,
// efficient, qmodel_38max, qfmodel, qmodel_latest, qmodel, kmodel_latest,
// kmodel, gmodel, gfmodel, dmodel, dfmodel, mmodel (issue #8 — a stale table
// here meant a requested rename silently routed to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qoder-ultimate", "ultimate":
		return "ultimate"
	case "qoder-performance", "performance":
		return "performance"
	case "qoder-efficient", "efficient":
		return "efficient"
	// Qwen3.8-Max. qmodel_preview / qwen3.8-max-preview are the retired pre-0.12 keys.
	case "qwen3.8-max", "qwen3.8-max-preview", "qmodel_38max", "qmodel_preview":
		return "qmodel_38max"
	case "qwen3.8-flash", "qfmodel":
		return "qfmodel"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-flash", "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	// GLM-5.3. gm51model (GLM-5.2) was retired upstream.
	case "glm-5.3", "glm-5.2", "gmodel", "gm51model":
		return "gmodel"
	case "glm-5.3-flash", "gfmodel":
		return "gfmodel"
	case "kimi-k3", "kmodel_latest":
		return "kmodel_latest"
	case "kimi-k2.8-preview", "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m3", "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format. The
// client's fields ride along verbatim: role/content are decoded for routing
// and everything else (tool_calls, tool_call_id, name, structured content
// parts, ...) is preserved byte-for-byte, so multi-turn tool conversations
// and multimodal payloads survive the hop upstream.
type openAIMessage struct {
	Role       string
	Content    string
	contentSet bool
	// rawContent holds the original JSON when content was not a plain string.
	rawContent string
	// raw holds every other client member verbatim.
	raw map[string]json.RawMessage
}

func (m *openAIMessage) UnmarshalJSON(data []byte) error {
	m.Role, m.Content, m.contentSet, m.rawContent, m.raw = "", "", false, "", nil
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if role, ok := fields["role"]; ok {
		_ = json.Unmarshal(role, &m.Role)
	}
	if content, ok := fields["content"]; ok {
		m.contentSet = true
		var s string
		// 0.8.12: JSON null round-trips verbatim — unmarshaling null into a
		// string silently yields "" and would rewrite the member behind the
		// client's back (assistant messages carrying tool_calls commonly have
		// content:null; the protocol authority qoderwork2api preserves null
		// the same way via plain map decode).
		if trimmed := strings.TrimSpace(string(content)); trimmed == "null" {
			m.rawContent = string(content)
		} else if err := json.Unmarshal(content, &s); err == nil {
			m.Content = s
		} else {
			m.rawContent = string(content)
		}
		delete(fields, "content")
	}
	delete(fields, "role")
	if len(fields) > 0 {
		m.raw = fields
	}
	return nil
}

func (m openAIMessage) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.raw)+2)
	for k, v := range m.raw {
		out[k] = v
	}
	switch {
	case m.rawContent != "":
		out["content"] = json.RawMessage(m.rawContent)
	case m.contentSet:
		enc, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		out["content"] = enc
	}
	enc, err := json.Marshal(m.Role)
	if err != nil {
		return nil, err
	}
	out["role"] = enc
	return json.Marshal(out)
}

// assistantCarriesToolCalls reports whether the client message carries a
// non-empty tool_calls member. The verdict comes from the verbatim raw
// members, not from a typed field, so any tool_calls shape the client sent
// counts.
func assistantCarriesToolCalls(m openAIMessage) bool {
	var calls []json.RawMessage
	return json.Unmarshal(m.raw["tool_calls"], &calls) == nil && len(calls) > 0
}

// isEmptyAssistantBody reports whether the content member carries nothing
// upstream would keep: absent, null, blank, or an empty part array. Callers
// gate on Content being empty first, so rawContent is the authority for the
// non-string shapes — [] is empty, [{"type":"text",...}] is not.
func isEmptyAssistantBody(m openAIMessage) bool {
	raw := strings.TrimSpace(m.rawContent)
	if raw == "" || raw == "null" {
		return true
	}
	var parts []json.RawMessage
	return json.Unmarshal([]byte(raw), &parts) == nil && len(parts) == 0
}

// messageTextContent renders one message's textual content for the
// chat_context.text mirror of the latest user prompt. Plain strings pass
// through; structured content arrays contribute their text parts.
func messageTextContent(m openAIMessage) string {
	if m.rawContent == "" {
		return m.Content
	}
	if !strings.HasPrefix(m.rawContent, "[") {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(m.rawContent), &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	// ReasoningEffort is the OpenAI-style thinking dial. Upstream thinking_config
	// accepts low/medium/xhigh (2026-09-18 probe); empty or invalid leaves the
	// parameters block untouched so upstream applies its own default (medium).
	ReasoningEffort string `json:"reasoning_effort"`
	// Tools is the client's OpenAI tools array, forwarded verbatim when present.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// extractLatestUserPrompt returns the textual content of the last user
// message (structured content arrays contribute their text parts).
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messageTextContent(messages[i])
		}
	}
	return ""
}

// runeSafePrefix truncates to n runes without splitting UTF-8 sequences
// (upstream truncates business.name to 30 runes the same way).
func runeSafePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// normalizeReasoningEffort validates the OpenAI-style reasoning_effort dial.
// v0.8.17 (fork libo0118/qoder-custom review): the catalog's
// thinking_config.enabled.efforts advertise per-model level sets that include
// "high" and "max" (e.g. DeepSeek-Flash low/high/max, Qwen3.8-Max
// low/medium/xhigh) — the old low/medium/xhigh whitelist silently dropped the
// high/max dials and callers got upstream defaults instead. Levels are
// normalized to the union of advertised values without conflating
// high/max/xhigh; anything else returns "" = inject nothing.
func normalizeReasoningEffort(s string) string {
	effort := strings.ToLower(strings.TrimSpace(s))
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config. 2026-09-19 upstream behavior: every request sends
	// is_reasoning=true + source="system" (the qwen3.8-flash reasoning chain).
	// Models that cannot think ignore these fields upstream (verified by the
	// reference proxy with a fabricated model key). source="system" is the real
	// reasoning trigger — without it the gateway never streams reasoning_content.
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		mc["is_reasoning"] = true
		if _, ok := mc["source"]; !ok {
			mc["source"] = "system"
		}
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
				mc["is_reasoning"] = true
			}
		}
	}

	// messages: slim passthrough when the client carries its own system prompt
	// (every harness does) — forward the conversation verbatim and drop the
	// 10657-token template system prompt plus template tools. Upstream verified
	// 2026-09-18 that neither is required (baseline prompt_tokens ~10K → ~60),
	// which directly extends the headroom before oversized requests fail.
	// Bare prompts without a system message keep the template for parity.
	slim := false
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			slim = true
			break
		}
	}
	var outMsgs []any
	if slim {
		delete(base, "tools")
	} else {
		if msgs, ok := base["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if role, _ := mm["role"].(string); role == "system" {
						outMsgs = append(outMsgs, m)
					}
				}
			}
		}
	}
	for _, m := range req.Messages {
		// Qoder discards assistant turns whose content is empty even
		// when they carry tool_calls, orphaning the tool result that
		// follows ("Messages with role 'tool' must be a response to a
		// preceding message with 'tool_calls'"). Give such turns a
		// minimal body — on the loop's value copy, never in the
		// caller's request. Everything else round-trips verbatim,
		// including the 0.8.12 content:null passthrough contract for
		// turns upstream is willing to keep.
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" &&
			assistantCarriesToolCalls(m) && isEmptyAssistantBody(m) {
			m.Content, m.rawContent, m.contentSet = "Calling tools.", "", true
		}
		outMsgs = append(outMsgs, m)
	}
	base["messages"] = outMsgs

	// Client tools win; template tools only ride along in template mode.
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		base["tools"] = req.Tools
	}

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		biz["name"] = runeSafePrefix(prompt, 30)
	}

	// Reasoning dial: inject upstream thinking parameters only when the client
	// supplied a valid reasoning_effort. Absent/invalid keeps upstream defaults;
	// the template parameters (max_tokens) are preserved either way.
	if effort := normalizeReasoningEffort(req.ReasoningEffort); effort != "" {
		params, _ := base["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
			base["parameters"] = params
		}
		params["enable_thinking"] = true
		params["reasoning_effort"] = effort
	}

	return json.Marshal(base)
}
