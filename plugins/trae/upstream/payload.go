// payload.go OpenAI → SOLO llm_utils_chat 请求体改写。
package upstream

import (
	"encoding/json"
	"strings"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function:"solo_work_lite", stream:true,
//	         config_name:<model>, model:<model>}
//
// 改写规则（SPEC §4.4）：
//  1. messages: content 字符串 → [{"type":"text","text":...}]；已是数组 → 透传
//  2. stream: 强制 true（非流式由服务端聚合）
//  3. model → config_name + model
//  4. function: 固定 "solo_work_lite"
//  5. tools/tool_choice: 归一化（"none" 删 tools；auto/required 保留；function 提取 name）
func PrepareBody(src []byte, variant string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = FunctionFor(variant)

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)

			// assistant 消息回传 tool_calls: OpenAI function → 上游 function_call
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						// OpenAI: function{name, arguments} → 上游 SOLO: function_call{name, arguments}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						// 上游要求 FunctionCall.Name 必填: 无 name 的 tool_call 剔除
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}

			if !present || content == nil {
				continue // 无 content 的消息(如纯 tool_calls assistant 已转换完)保留字段但跳过 content 改写
			}
			switch c := content.(type) {
			case string:
				m["content"] = []any{
					map[string]any{"type": "text", "text": c},
				}
			default:
				// 已是数组 → 透传（兼容多模态，未实测，保守透传）
			}
		}
	}

	model, _ := obj["model"].(string)
	model = SanitizeModelName(model, variant)
	if model == "" {
		model = DefaultConfigName
	}

	normalizeToolChoice(obj)
	normalizeTools(obj)

	// v0.12.37: whitelist passthrough. The upstream llm_utils_chat contract is
	// minimal — messages/function/stream/config_name/model plus the OpenAI tool
	// and sampling fields a working community gateway (Ttungx/trae-solo-local-api)
	// proves are tolerated ("SOLO llm_utils_chat 只稳定接受 messages、function 和
	// config_name"; its sampling forward list is our allowlist). Everything else
	// the client sends — reasoning_effort/thinking/stream_options/response_format/
	// user/metadata/... — is DROPPED: the upstream has no native thinking params,
	// and agent-specific fields have triggered stream errors (4023 "model is
	// unknown" for agent_type/device_id/ide_version per the same source).
	out := make(map[string]any, 16)
	if msgs, ok := obj["messages"]; ok {
		out["messages"] = msgs
	}
	out["function"] = FunctionFor(variant)
	out["stream"] = true
	out["config_name"] = model
	out["model"] = model
	if tools, ok := obj["tools"]; ok {
		out["tools"] = tools
	}
	if tc, ok := obj["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	for _, k := range []string{"temperature", "top_p", "max_tokens",
		"presence_penalty", "frequency_penalty", "seed", "n"} {
		if v, ok := obj[k].(float64); ok {
			out[k] = v
		}
	}
	switch stop := obj["stop"].(type) {
	case string:
		out["stop"] = stop
	case []any:
		out["stop"] = stop
	}
	b, err := json.Marshal(out)
	if err != nil {
		return src
	}
	return b
}

// SanitizeModelName strips the plugin's client-facing credential-namespace
// suffix from a model id before the id is sent upstream as config_name.
//
// main.go namespaces every advertised model id by credential variant
// ("-solo" for SOLO credentials, "-intl" for Intl) so the host can never
// route a chat request across credential classes. The upstream catalog only
// knows the BARE config name ("Doubao-Seed-2.1-Turbo"): a namespaced id sent
// verbatim was rejected inside the SSE stream with event:error biz_code=4001
// "We're sorry, the param is invalid." — this failed EVERY SOLO-variant chat
// call while the request log still showed the stream as successful (the
// transport succeeded; only the model was unknown).
//
// Only ONE suffix occurrence is stripped (our namespacing appends exactly
// one), so a genuine upstream model ending in "-solo" still round-trips
// (advertised "x-solo-solo" → upstream "x-solo").
func SanitizeModelName(model, variant string) string {
	m := strings.TrimSpace(model)
	if variant == "solo" {
		m = strings.TrimSuffix(m, "-solo")
	}
	m = strings.TrimSuffix(m, "-intl")
	return strings.TrimSpace(m)
}

// DefaultConfigName 默认模型（glm-5.2，实测可用）。
const DefaultConfigName = "glm-5.2"

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none" / {"type":"none"} → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 把 OpenAI tools 转为 SOLO 上游格式。
// 实测上游 Go struct: FunctionDefinition.tools[].function.parameters 是 string 类型
// （OpenAI 标准是 object）→ 需把 parameters 对象序列化为 JSON 字符串。
// 同时 tools 条目若不是 map 或缺 function，整体剔除（避免上游反序列化失败）。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
