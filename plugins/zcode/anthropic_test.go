// anthropic_test.go covers the start-plan translation layer: the OpenAI→
// Anthropic request translator, the start-plan body transform (official
// system blocks / context prefix / cache markers / metadata.user_id), the
// response translators (non-stream + SSE state machine), the provider
// business-code error renderer, and the executor wiring against an
// httptest start-plan gateway.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func openAIRequest(t *testing.T, extra map[string]any, messages ...map[string]any) []byte {
	t.Helper()
	body := map[string]any{"model": "glm-5.3-flash", "messages": messages}
	for k, v := range extra {
		body[k] = v
	}
	return mustMarshal(t, body)
}

func decodeAnthropic(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("translated body not JSON: %v\n%s", err, raw)
	}
	return obj
}

func TestTranslateOpenAIToAnthropicBasic(t *testing.T) {
	payload := openAIRequest(t, map[string]any{
		"temperature": 0.7,
		"top_p":       0.9,
	}, map[string]any{"role": "system", "content": "be terse"},
		map[string]any{"role": "system", "content": "second system"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
		map[string]any{"role": "user", "content": "bye"})
	body, userSystem, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// The joined system text comes back as ONE user block (the caller appends
	// it after the official gateway blocks).
	if len(userSystem) != 1 || userSystem[0].Text != "be terse\n\nsecond system" {
		t.Fatalf("userSystem = %+v", userSystem)
	}
	if got := body["model"]; got != "glm-5.3-flash" {
		t.Fatalf("model = %v", got)
	}
	if got := body["stream"]; got != false {
		t.Fatalf("stream = %v, want false", got)
	}
	// max_tokens defaults to the catalog ceiling (glm-5.3-flash → 128000).
	if got := body["max_tokens"]; got != int64(128000) {
		t.Fatalf("max_tokens = %v (%T), want int64(128000)", got, got)
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (system extracted)", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[0]["content"] != "hello" {
		t.Fatalf("first message = %+v", msgs[0])
	}
}

func TestTranslateOpenAIToAnthropicSystemJoin(t *testing.T) {
	payload := openAIRequest(t, nil,
		map[string]any{"role": "system", "content": "alpha"},
		map[string]any{"role": "system", "content": "beta"},
		map[string]any{"role": "user", "content": "hi"})
	_, userSystem, err := translateOpenAIToAnthropic(payload, "glm-5.2", false)
	_ = userSystem
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// The joined system string is checked through the full wrapper below;
	// here just verify the multi-system request doesn't leak system messages.
	body, _, _ := translateOpenAIToAnthropic(payload, "glm-5.2", false)
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

func TestTranslateStopSequences(t *testing.T) {
	cases := []struct {
		name string
		stop any
		want []any
	}{
		{"string", "END", []any{"END"}},
		{"array", []string{"A", "B"}, []any{"A", "B"}},
		{"empty string", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{}
			if tc.stop != nil {
				extra["stop"] = tc.stop
			}
			payload := openAIRequest(t, extra, map[string]any{"role": "user", "content": "x"})
			body, _, err := translateOpenAIToAnthropic(payload, "glm-5-turbo", false)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			got, present := body["stop_sequences"]
			if tc.want == nil {
				if present {
					t.Fatalf("stop_sequences present: %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("stop_sequences missing, want %v", tc.want)
			}
			arr := got.([]string)
			if len(arr) != len(tc.want) {
				t.Fatalf("stop_sequences = %v", arr)
			}
			for i := range tc.want {
				if arr[i] != tc.want[i] {
					t.Fatalf("stop_sequences = %v", arr)
				}
			}
		})
	}
}

func TestTranslateToolCallsAndCoalescing(t *testing.T) {
	payload := mustMarshal(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": "",
				"tool_calls": []map[string]any{
					{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Hangzhou"}`}},
					{"id": "call_2", "type": "function", "function": map[string]any{"name": "get_time", "arguments": `{}`}},
				}},
			{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
			{"role": "tool", "tool_call_id": "call_2", "content": "noon"},
			{"role": "user", "content": "thanks"},
		},
	})
	body, _, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (user / assistant+tools / coalesced-tool-results / user)", len(msgs))
	}
	asst := msgs[1]
	if asst["role"] != "assistant" {
		t.Fatalf("role = %v", asst["role"])
	}
	blocks := asst["content"].([]map[string]any)
	if len(blocks) != 2 || blocks[0]["type"] != "tool_use" || blocks[1]["type"] != "tool_use" {
		t.Fatalf("assistant blocks = %+v (empty text must not emit a text block)", blocks)
	}
	if blocks[0]["input"].(map[string]any)["city"] != "Hangzhou" {
		t.Fatalf("tool_use input = %+v", blocks[0]["input"])
	}
	// Two consecutive tool results coalesce into ONE user turn.
	coalesced := msgs[2]
	_ = coalesced
	if coalesced["role"] != "user" {
		t.Fatalf("coalesced role = %v", coalesced["role"])
	}
	results := coalesced["content"].([]map[string]any)
	if len(results) != 2 || results[0]["type"] != "tool_result" || results[0]["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result blocks = %+v", results)
	}
}

func TestTranslateToolsAndToolChoice(t *testing.T) {
	payload := mustMarshal(t, map[string]any{
		"model":       "glm-5.3-flash",
		"messages":    []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "description": "d", "parameters": map[string]any{"type": "object"}}}},
		"tool_choice": "required",
	})
	body, _, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	tools := body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "f" || tools[0]["description"] != "d" {
		t.Fatalf("tools = %+v", tools)
	}
	if _, hasSchema := tools[0]["input_schema"]; !hasSchema {
		t.Fatalf("input_schema missing: %+v", tools[0])
	}
	if _, hasType := tools[0]["type"]; hasType {
		t.Fatalf("anthropic tools must not carry openai type: %+v", tools[0])
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["type"] != "any" {
		t.Fatalf("tool_choice = %+v, want any", choice)
	}

	// tool_choice none → tools dropped entirely.
	payloadNone := mustMarshal(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		"tool_choice": "none",
	})
	bodyNone, _, _ := translateOpenAIToAnthropic(payloadNone, "glm-5.3-flash", false)
	if _, has := bodyNone["tools"]; has {
		t.Fatalf("tools must be dropped on tool_choice=none")
	}

	// named function choice.
	payloadFn := mustMarshal(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "f"}},
	})
	bodyFn, _, _ := translateOpenAIToAnthropic(payloadFn, "glm-5.3-flash", false)
	choiceFn := bodyFn["tool_choice"].(map[string]any)
	if choiceFn["type"] != "tool" || choiceFn["name"] != "f" {
		t.Fatalf("tool_choice = %+v", choiceFn)
	}
}

func TestImageContentTranslation(t *testing.T) {
	payload := mustMarshal(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is this"},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}},
			{"type": "image_url", "image_url": map[string]any{"url": "ftp://weird"}},
		}}},
	})
	body, _, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	blocks := body["messages"].([]map[string]any)[0]["content"].([]map[string]any)
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	img0 := blocks[1]
	if img0["type"] != "image" {
		t.Fatalf("block 1 = %+v", img0)
	}
	src0 := img0["source"].(map[string]any)
	if src0["type"] != "base64" || src0["media_type"] != "image/png" || src0["data"] != "aGVsbG8=" {
		t.Fatalf("base64 source = %+v", src0)
	}
	src1 := blocks[2]["source"].(map[string]any)
	if src1["type"] != "url" || src1["url"] != "https://example.com/cat.png" {
		t.Fatalf("url source = %+v", src1)
	}
	if blocks[3]["type"] != "text" || blocks[3]["text"] != "ftp://weird" {
		t.Fatalf("exotic URL must degrade to text: %+v", blocks[3])
	}
}

// ---------------------------------------------------------------------------
// Thinking / reasoning translation
// ---------------------------------------------------------------------------

func TestThinkingGLM53EffortChannel(t *testing.T) {
	payload := openAIRequest(t, map[string]any{"temperature": 0.5, "top_p": 0.9, "max_tokens": 1000},
		map[string]any{"role": "user", "content": "x"})
	// applyThinkingTranslation runs INSIDE translateOpenAIToAnthropic — the
	// production path is single-pass, so assert on its output directly.
	body, _, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %+v", thinking)
	}
	if thinking["budget_tokens"] != 32000 {
		t.Fatalf("budget = %v, want 32000 (max effort)", thinking["budget_tokens"])
	}
	oc := body["output_config"].(map[string]any)
	if oc["effort"] != "max" {
		t.Fatalf("effort = %v, want max (catalog default)", oc["effort"])
	}
	if _, has := body["temperature"]; has {
		t.Fatalf("temperature must be voided with thinking enabled")
	}
	if _, has := body["top_p"]; has {
		t.Fatalf("top_p must be voided with thinking enabled")
	}
	// max_tokens += budget (1000 + 32000 = 33000).
	if body["max_tokens"] != int64(33000) {
		t.Fatalf("max_tokens = %v, want 33000", body["max_tokens"])
	}
}

func TestThinkingGLM53ExplicitDisabled(t *testing.T) {
	payload := openAIRequest(t, map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	}, map[string]any{"role": "user", "content": "x"})
	body, _, _ := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %+v", thinking)
	}
	if _, has := body["output_config"]; has {
		t.Fatalf("disabled thinking must not carry output_config")
	}
}

func TestThinkingReasoningEffortNone(t *testing.T) {
	payload := openAIRequest(t, map[string]any{"reasoning_effort": "none", "temperature": 0.3, "top_p": 0.8},
		map[string]any{"role": "user", "content": "x"})
	body, _, _ := translateOpenAIToAnthropic(payload, "glm-5-turbo", false)
	if body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("reasoning_effort none must map to disabled: %+v", body["thinking"])
	}
	// No thinking + temperature + top_p both set → top_p voided.
	if _, has := body["top_p"]; has {
		t.Fatalf("top_p must be voided when temperature is set without thinking")
	}
}

func TestThinkingExplicitBudgetRespected(t *testing.T) {
	payload := openAIRequest(t, map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048},
	}, map[string]any{"role": "user", "content": "x"})
	body, _, _ := translateOpenAIToAnthropic(payload, "glm-5.3-flash", false)
	thinking := body["thinking"].(map[string]any)
	if thinking["budget_tokens"] != 2048 {
		t.Fatalf("budget = %v, want explicit 2048", thinking["budget_tokens"])
	}
	if body["output_config"].(map[string]any)["effort"] == nil {
		t.Fatalf("output_config.effort must still ride along")
	}
}

// ---------------------------------------------------------------------------
// Start-plan body transform
// ---------------------------------------------------------------------------

func TestApplyStartPlanTransformShape(t *testing.T) {
	payload := openAIRequest(t, map[string]any{
		"tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{}}}},
	}, map[string]any{"role": "user", "content": "hi"})
	body, userSystem, err := translateOpenAIToAnthropic(payload, "glm-5.3-flash", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local)
	applyStartPlanTransform(body, userSystem, "glm-5.3-flash", "zai", "device-mid-1", now)

	// System: exactly 3 official blocks + no user system → 3 blocks.
	system := body["system"].([]systemBlock)
	if len(system) != 3 {
		t.Fatalf("system blocks = %d, want 3", len(system))
	}
	if system[0].Text != zcodeSystem.CLIPrefix {
		t.Fatalf("block 1 = %.60q, want cliPrefix", system[0].Text)
	}
	for i, b := range system {
		if b.Type != "text" || b.CacheControl == nil || b.CacheControl.Type != "ephemeral" {
			t.Fatalf("block %d = %+v, want ephemeral-marked text", i+1, b)
		}
	}
	// Block 3 carries the dynamic sections + powered-by line.
	if !strings.Contains(system[2].Text, "zai-api/glm-5.3-flash") {
		t.Fatalf("powered-by line missing: %.200q", system[2].Text)
	}
	if !strings.Contains(system[2].Text, "Context management") {
		t.Fatalf("dynamic sections missing: %.200q", system[2].Text)
	}

	// First message = context_prefix user turn with the local date.
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (prefix + user)", len(msgs))
	}
	prefix := msgs[0]
	if prefix["role"] != "user" {
		t.Fatalf("prefix role = %v", prefix["role"])
	}
	prefixText := prefix["content"].([]map[string]any)[0]["text"].(string)
	if !strings.HasPrefix(prefixText, "<system-reminder>") || !strings.HasSuffix(prefixText, "</system-reminder>") {
		t.Fatalf("prefix reminder wrappers missing: %.80q", prefixText)
	}
	if !strings.Contains(prefixText, "Today's date is 2026-09-21.") {
		t.Fatalf("currentDate missing: %.200q", prefixText)
	}

	// Last non-system message's last block gains the ephemeral marker.
	lastBlock := msgs[1]["content"].([]map[string]any)[0]
	if lastBlock["cache_control"].(map[string]string)["type"] != "ephemeral" {
		t.Fatalf("last-message cache marker missing: %+v", lastBlock)
	}

	// Tools lose any cache_control.
	tool := body["tools"].([]map[string]any)[0]
	if _, has := tool["cache_control"]; has {
		t.Fatalf("tools cache_control must be stripped")
	}

	// metadata.user_id blob.
	meta := body["metadata"].(map[string]any)
	uid, _ := meta["user_id"].(string)
	var parsed map[string]any
	if json.Unmarshal([]byte(uid), &parsed) != nil {
		t.Fatalf("user_id not JSON: %q", uid)
	}
	if parsed["device_id"] != "device-mid-1" || parsed["account_uuid"] != "" || parsed["session_id"] != "" {
		t.Fatalf("user_id blob = %+v", parsed)
	}
}

func TestBuildAnthropicMetadataUserIdShape(t *testing.T) {
	got := buildAnthropicMetadataUserId("dm-1")
	want := `{"device_id":"dm-1","account_uuid":"","session_id":""}`
	if got != want {
		t.Fatalf("user_id = %s, want %s", got, want)
	}
}

func TestSystemPromptAssetLoaded(t *testing.T) {
	if zcodeSystem.CLIPrefix == "" {
		t.Fatal("cliPrefix empty")
	}
	if len(zcodeSystem.Stable) != 2 {
		t.Fatalf("stable sections = %d, want 2", len(zcodeSystem.Stable))
	}
	if zcodeSystem.Environment.PoweredByLine == "" || zcodeSystem.ContextPrefix.CurrentDateLine == "" {
		t.Fatal("environment/contextPrefix sections empty")
	}
}

func TestEnvironmentSectionLines(t *testing.T) {
	env := startPlanEnvInfo{Cwd: "/work/x", Platform: "linux", Shell: "bash", OsVersion: "linux 6.1.0 amd64"}
	section := buildEnvironmentSection(env, "glm-5.2", "bigmodel")
	lines := strings.Split(section, "\n")
	if len(lines) != 8 {
		t.Fatalf("lines = %d, want 8 (7 base + powered-by)", len(lines))
	}
	if !strings.Contains(lines[2], "/work/x") || !strings.Contains(lines[6], "linux 6.1.0 amd64") {
		t.Fatalf("env lines = %v", lines)
	}
	if !strings.Contains(lines[7], "bigmodel-api/glm-5.2") {
		t.Fatalf("powered-by = %q", lines[7])
	}
	// No model → no powered-by line.
	section2 := buildEnvironmentSection(env, "", "zai")
	if strings.Contains(section2, "powered by") {
		t.Fatalf("powered-by must be conditional: %q", section2)
	}
}

// ---------------------------------------------------------------------------
// Response translation (non-stream)
// ---------------------------------------------------------------------------

func TestTranslateAnthropicResponseFull(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"id":    "msg_1",
		"type":  "message",
		"role":  "assistant",
		"model": "glm-5.3-flash",
		"content": []map[string]any{
			{"type": "thinking", "thinking": "let me think"},
			{"type": "text", "text": "Hello "},
			{"type": "text", "text": "world"},
			{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": map[string]any{"city": "HZ"}},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 50},
	})
	out, err := translateAnthropicResponseToOpenAI(body, "glm-5.3-flash")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var completion map[string]any
	if json.Unmarshal(out, &completion) != nil {
		t.Fatalf("completion not JSON: %s", out)
	}
	if completion["object"] != "chat.completion" || completion["model"] != "glm-5.3-flash" {
		t.Fatalf("envelope = %+v", completion)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "let me think" {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
	callsAny, ok := msg["tool_calls"].([]any)
	if !ok || len(callsAny) != 1 {
		t.Fatalf("tool_calls = %+v", msg["tool_calls"])
	}
	calls := callsAny[0].(map[string]any)
	if calls["id"] != "tu_1" {
		t.Fatalf("tool_calls = %+v", calls)
	}
	fn := calls["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"HZ"}` {
		t.Fatalf("function = %+v", fn)
	}
	usage := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(150) || usage["completion_tokens"] != float64(20) || usage["total_tokens"] != float64(170) {
		t.Fatalf("usage = %+v (prompt must include cache read)", usage)
	}
	details := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(50) {
		t.Fatalf("cached_tokens = %v", details["cached_tokens"])
	}
}

func TestTranslateAnthropicResponseVariants(t *testing.T) {
	// stop_reason mapping.
	cases := map[string]any{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls",
	}
	for anthropicStop, wantFinish := range cases {
		body := mustMarshal(t, map[string]any{
			"id": "m", "content": []map[string]any{{"type": "text", "text": "x"}},
			"stop_reason": anthropicStop, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
		out, err := translateAnthropicResponseToOpenAI(body, "glm-5.2")
		if err != nil {
			t.Fatalf("%s: %v", anthropicStop, err)
		}
		var c map[string]any
		_ = json.Unmarshal(out, &c)
		got := c["choices"].([]any)[0].(map[string]any)["finish_reason"]
		if got != wantFinish {
			t.Fatalf("stop_reason %s → finish %v, want %v", anthropicStop, got, wantFinish)
		}
	}

	// Empty text (tool-only turn) → content null.
	bodyToolOnly := mustMarshal(t, map[string]any{
		"id": "m", "content": []map[string]any{{"type": "tool_use", "id": "t", "name": "f", "input": map[string]any{}}},
		"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	out, err := translateAnthropicResponseToOpenAI(bodyToolOnly, "glm-5.2")
	if err != nil {
		t.Fatalf("tool-only: %v", err)
	}
	var c map[string]any
	_ = json.Unmarshal(out, &c)
	msg := c["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if content, present := msg["content"]; !present || content != nil {
		t.Fatalf("tool-only content = %v (%T), want null", content, content)
	}

	// Error envelope → error, never a fake completion.
	errBody := mustMarshal(t, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "bad"}})
	if _, err := translateAnthropicResponseToOpenAI(errBody, "glm-5.2"); err == nil {
		t.Fatal("error envelope must surface as error")
	}
}

// ---------------------------------------------------------------------------
// SSE state machine
// ---------------------------------------------------------------------------

type sseFixture struct {
	event string
	data  string
}

func runSSE(t *testing.T, model string, frames []sseFixture) []string {
	t.Helper()
	state := newAnthropicSSEState(model)
	var out []string
	for i, f := range frames {
		outs, err := state.anthropicTranslateEvent(f.event, []byte(f.data))
		if err != nil {
			t.Fatalf("frame %d (%s): %v", i, f.event, err)
		}
		out = append(out, outs...)
	}
	return out
}

func chunkDelta(out string) map[string]any {
	var chunk map[string]any
	_ = json.Unmarshal([]byte(out), &chunk)
	choices := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	return choices[0].(map[string]any)["delta"].(map[string]any)
}

func chunkFinish(out string) any {
	var chunk map[string]any
	_ = json.Unmarshal([]byte(out), &chunk)
	choices := chunk["choices"].([]any)
	return choices[0].(map[string]any)["finish_reason"]
}

func TestSSEFullStreamTranslation(t *testing.T) {
	frames := []sseFixture{
		{"message_start", `{"type":"message_start","message":{"id":"msg_9","model":"glm-5.3-flash","usage":{"input_tokens":42,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"f","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"ping", `{"type":"ping"}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30,"input_tokens":42}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	out := runSSE(t, "glm-5.3-flash", frames)
	if len(out) < 6 {
		t.Fatalf("chunks = %d", len(out))
	}
	// First chunk: role only.
	first := chunkDelta(out[0])
	if first["role"] != "assistant" || len(first) != 1 {
		t.Fatalf("first delta = %+v", first)
	}
	// Text deltas in order.
	var text strings.Builder
	var toolArgs strings.Builder
	var lastChunk string
	for _, c := range out {
		if d := chunkDelta(c); d != nil {
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if tcs, ok := d["tool_calls"].([]any); ok {
				tc := tcs[0].(map[string]any)
				if fn, ok := tc["function"].(map[string]any); ok {
					if s, ok := fn["arguments"].(string); ok {
						toolArgs.WriteString(s)
					}
				}
			}
		}
		if chunkFinish(c) != nil {
			lastChunk = c
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("text = %q", text.String())
	}
	if toolArgs.String() != `{"a":1}` {
		t.Fatalf("tool args = %q", toolArgs.String())
	}
	// The tool_use start chunk carries id+name and its own index.
	var sawToolStart bool
	for _, c := range out {
		if d := chunkDelta(c); d != nil {
			if tcs, ok := d["tool_calls"].([]any); ok {
				tc := tcs[0].(map[string]any)
				if tc["id"] == "tu_1" && tc["type"] == "function" {
					sawToolStart = true
					if tc["index"] != float64(0) {
						t.Fatalf("tool index = %v, want 0 (text blocks consume no tool index)", tc["index"])
					}
				}
			}
		}
	}
	if !sawToolStart {
		t.Fatal("tool_use start chunk missing")
	}
	// Final chunk: finish_reason tool_calls + usage folded.
	if chunkFinish(lastChunk) != "tool_calls" {
		t.Fatalf("finish = %v", chunkFinish(lastChunk))
	}
	var lastObj map[string]any
	_ = json.Unmarshal([]byte(lastChunk), &lastObj)
	usage := lastObj["usage"].(map[string]any)
	if usage["completion_tokens"] != float64(30) || usage["prompt_tokens"] != float64(42) {
		t.Fatalf("final usage = %+v", usage)
	}
	// message_stop after a finish must NOT emit another chunk (no trailing
	// choices:[] usage chunk).
}

func TestSSETruncatedStreamFallback(t *testing.T) {
	state := newAnthropicSSEState("glm-5.3-flash")
	_, err := state.anthropicTranslateEvent("message_start", []byte(`{"type":"message_start","message":{"id":"m"}}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Stream ends without message_delta/message_stop → flushFinal emits stop.
	fin := state.flushFinal()
	if fin == "" {
		t.Fatal("flushFinal must emit the terminal chunk")
	}
	if chunkFinish(fin) != "stop" {
		t.Fatalf("fallback finish = %v", chunkFinish(fin))
	}
	if again := state.flushFinal(); again != "" {
		t.Fatal("flushFinal must be idempotent")
	}
}

func TestSSEThinkingAndSignatureDeltas(t *testing.T) {
	frames := []sseFixture{
		{"message_start", `{"type":"message_start","message":{"id":"m"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"xx"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
	}
	out := runSSE(t, "glm-5.3-flash", frames)
	var reasoning, content string
	for _, c := range out {
		if d := chunkDelta(c); d != nil {
			if s, ok := d["reasoning_content"].(string); ok {
				reasoning += s
			}
			if s, ok := d["content"].(string); ok {
				content += s
			}
		}
	}
	if reasoning != "hmm" || content != "ok" {
		t.Fatalf("reasoning=%q content=%q", reasoning, content)
	}
	// signature_delta must not emit a chunk (2 deltas only).
	if len(out) != 4 { // role + thinking + text + finish
		t.Fatalf("chunks = %d, want 4 (signature_delta silent)", len(out))
	}
}

func TestSSEUsageZeroOverwrite(t *testing.T) {
	state := newAnthropicSSEState("glm-5.3-flash")
	_, _ = state.anthropicTranslateEvent("message_start", []byte(`{"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"output_tokens":5}}}`))
	// A present 0 wins over the earlier 5 (overwrite semantics).
	_, _ = state.anthropicTranslateEvent("message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`))
	var chunk map[string]any
	fin := state.chunkJSON(map[string]any{}, "stop", true)
	_ = json.Unmarshal([]byte(fin), &chunk)
	if chunk["usage"].(map[string]any)["completion_tokens"] != float64(0) {
		t.Fatalf("usage = %+v, want completion_tokens 0 (explicit zero overwrites)", chunk["usage"])
	}
}

func TestSSEErrorEvent(t *testing.T) {
	state := newAnthropicSSEState("glm-5.3-flash")
	if _, err := state.anthropicTranslateEvent("error", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)); err == nil {
		t.Fatal("error event must surface as error")
	}
	// Malformed JSON data lines are skipped silently.
	if outs, err := state.anthropicTranslateEvent("message_start", []byte(`not-json`)); err != nil || len(outs) != 0 {
		t.Fatalf("malformed frame = (%v, %v)", outs, err)
	}
}

// ---------------------------------------------------------------------------
// Business error codes / captcha challenge
// ---------------------------------------------------------------------------

func TestExtractBizErrorCode(t *testing.T) {
	cases := map[string]string{
		`{"code":3007,"msg":"captcha"}`:                   "3007",
		`{"code":"1261","msg":"context"}`:                 "1261",
		`{"error":{"code":1006,"message":"auth"}}`:        "1006",
		`{"error":{"type":"api_error","message":"boom"}}`: "",
		`{"code":0,"msg":"ok"}`:                           "0",
		`plain text`:                                      "",
		``:                                                "",
	}
	for body, want := range cases {
		if got := extractBizErrorCode(body); got != want {
			t.Fatalf("extract(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestStartPlanCaptchaChallengeDetection(t *testing.T) {
	// Header variant: param header on a non-2xx.
	if !startPlanCaptchaChallenge(403, "param-blob", "anything") {
		t.Fatal("header variant missed")
	}
	// In-body variant: HTTP 400 {"code":3007}.
	if !startPlanCaptchaChallenge(400, "", `{"code":3007,"msg":"need captcha"}`) {
		t.Fatal("in-body variant missed")
	}
	if !startPlanCaptchaChallenge(400, "", `{"code": 3007}`) {
		t.Fatal("spaced in-body variant missed")
	}
	// Negatives.
	if startPlanCaptchaChallenge(200, "param", "{}") {
		t.Fatal("2xx with header is not a challenge")
	}
	if startPlanCaptchaChallenge(400, "", `{"code":3001}`) {
		t.Fatal("3001 is not a captcha challenge")
	}
}

func TestStartPlanBizErrorMessages(t *testing.T) {
	if err := startPlanBizError(400, `{"code":3007}`, ""); err == nil || !strings.Contains(err.Error(), "验证码挑战") {
		t.Fatalf("3007 error = %v", err)
	}
	if err := startPlanBizError(400, `{"code":1261}`, ""); err == nil || !strings.Contains(err.Error(), "上下文") {
		t.Fatalf("1261 error = %v", err)
	}
	if err := startPlanBizError(429, `{"code":1302}`, ""); err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("1302 error = %v", err)
	}
	if err := startPlanBizError(500, `{"code":3008}`, ""); err == nil || !strings.Contains(err.Error(), "并发") {
		t.Fatalf("3008 error = %v", err)
	}
	// Unknown code → generic fallback wording.
	if err := startPlanBizError(500, `{"code":99999}`, ""); err == nil || strings.Contains(err.Error(), "验证码") {
		t.Fatalf("generic error = %v", err)
	}
}

func TestRouteChatErrorStartPlan401(t *testing.T) {
	route := chatRoute{endpoint: startPlanAnthropicEndpoint, anthropic: true, applyHeaders: applyStartPlanChatHeaders}
	err := routeChatError(route, nil, http.StatusUnauthorized, nil, `{"error":"jwt expired"}`)
	if err == nil || !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("401 error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Executor wiring against an httptest start-plan gateway
// ---------------------------------------------------------------------------

// executorRequestJSON builds one ExecutorRequest wire body: byte fields
// (Payload/StorageJSON/OriginalRequest) travel base64-encoded over the RPC.
func executorRequestJSON(t *testing.T, fields map[string]any, payload map[string]any, storage []byte) []byte {
	t.Helper()
	payloadRaw := mustMarshal(t, payload)
	fields["Payload"] = base64.StdEncoding.EncodeToString(payloadRaw)
	fields["StorageJSON"] = storage
	return mustMarshal(t, fields)
}

// startPlanAuthJSON is a start-plan credential as stored on disk.
func startPlanAuthJSON() []byte {
	raw, _ := json.Marshal(storedAuth{
		Auth: zcodeTokens{
			AccessToken: "resolved-key-placeholder", // parseStored requires it non-empty
			JWT:         "test-jwt",
			Provider:    providerZai,
			Plan:        planStart,
			DeviceMid:   "device-mid-1",
		},
		Account: zcodeAccount{UID: "user-1"},
	})
	return raw
}

// startPlanGateway is a recording httptest server speaking the anthropic
// dialect. It asserts the wire contract once and answers every call with the
// given status/headers/body.
type startPlanGateway struct {
	srv      *httptest.Server
	mu       chan struct{}
	requests []capturedRequest
}

type capturedRequest struct {
	path    string
	method  string
	headers http.Header
	body    string
}

func newStartPlanGateway(t *testing.T, status int, headers map[string]string, responseBody string) *startPlanGateway {
	t.Helper()
	g := &startPlanGateway{mu: make(chan struct{}, 1)}
	g.mu <- struct{}{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAll(r.Body)
		<-g.mu
		g.requests = append(g.requests, capturedRequest{path: r.URL.Path, method: r.Method, headers: r.Header.Clone(), body: string(raw)})
		g.mu <- struct{}{}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *startPlanGateway) calls() []capturedRequest {
	<-g.mu
	defer func() { g.mu <- struct{}{} }()
	out := make([]capturedRequest, len(g.requests))
	copy(out, g.requests)
	return out
}

const sampleAnthropicMessage = `{
  "id": "msg_gateway_1",
  "type": "message",
  "role": "assistant",
  "model": "glm-5.3-flash",
  "content": [{"type": "text", "text": "Hello from start-plan"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 11, "output_tokens": 7}
}`

func TestHandleExecExecuteStartPlan(t *testing.T) {
	g := newStartPlanGateway(t, http.StatusOK, nil, sampleAnthropicMessage)
	origEndpoint := startPlanAnthropicEndpoint
	startPlanAnthropicEndpoint = g.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { startPlanAnthropicEndpoint = origEndpoint }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":       "a1",
		"AuthProvider": providerName,
		"Model":        "zcode/glm-5.3-flash",
	}, map[string]any{
		"model":    "zcode/glm-5.3-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, startPlanAuthJSON())
	raw, err := handleExecExecute(req)
	if err != nil {
		t.Fatalf("handleExecExecute: %v", err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	var resp pluginapiExecutorResponseProbe
	if json.Unmarshal(env.Result, &resp) != nil || len(resp.Payload) == 0 {
		t.Fatalf("result = %s", env.Result)
	}
	// ExecutorResponse.Payload is []byte on the wire: a base64 string.
	payloadBytes, decErr := base64.StdEncoding.DecodeString(strings.Trim(string(resp.Payload), `"`))
	if decErr != nil {
		t.Fatalf("payload not base64: %v (%s)", decErr, resp.Payload)
	}
	var completion map[string]any
	if json.Unmarshal(payloadBytes, &completion) != nil {
		t.Fatalf("payload not JSON: %s", payloadBytes)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello from start-plan" {
		t.Fatalf("content = %v", msg["content"])
	}
	usage := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) {
		t.Fatalf("usage = %+v", usage)
	}

	// Wire contract: path, method, auth, anthropic-version, trace subset.
	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.path != "/api/v1/zcode-plan/anthropic/v1/messages" || c.method != http.MethodPost {
		t.Fatalf("request = %s %s", c.method, c.path)
	}
	if auth := c.headers.Get("Authorization"); auth != "Bearer test-jwt" {
		t.Fatalf("Authorization = %q", auth)
	}
	if v := c.headers.Get("anthropic-version"); v != anthropicVersion {
		t.Fatalf("anthropic-version = %q", v)
	}
	if c.headers.Get("x-zcode-session-type") != "main" {
		t.Fatalf("session-type = %q", c.headers.Get("x-zcode-session-type"))
	}
	if c.headers.Get("x-query-id") != "" || c.headers.Get("x-session-id") != "" {
		t.Fatal("start-plan must omit x-query-id/x-session-id")
	}
	if !strings.HasSuffix(c.headers.Get("User-Agent"), llmSDKUserAgentSuffix) {
		t.Fatalf("UA = %q", c.headers.Get("User-Agent"))
	}

	// Body contract: official system blocks + prefix turn + metadata.
	var sent map[string]any
	if json.Unmarshal([]byte(c.body), &sent) != nil {
		t.Fatalf("sent body not JSON: %.200s", c.body)
	}
	if sent["stream"] != false {
		t.Fatalf("stream = %v, want false", sent["stream"])
	}
	if _, has := sent["metadata"]; !has {
		t.Fatal("metadata.user_id missing")
	}
	systemBlocks := sent["system"].([]any)
	if len(systemBlocks) != 3 {
		t.Fatalf("system blocks = %d, want 3", len(systemBlocks))
	}
	firstBlock := systemBlocks[0].(map[string]any)
	if firstBlock["text"] != zcodeSystem.CLIPrefix {
		t.Fatalf("block1 = %v", firstBlock["text"])
	}
	msgs := sent["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "user" {
		t.Fatalf("first wire message = %v (context_prefix must lead)", firstMsg["role"])
	}
}

func TestHandleExecExecuteStartPlanErrors(t *testing.T) {
	origEndpoint := startPlanAnthropicEndpoint
	defer func() { startPlanAnthropicEndpoint = origEndpoint }()

	cases := []struct {
		name     string
		status   int
		headers  map[string]string
		body     string
		contains string
	}{
		{"captcha in-body 3007", http.StatusBadRequest, nil, `{"code":3007,"msg":"captcha"}`, "验证码挑战"},
		{"captcha header variant", http.StatusForbidden, map[string]string{"x-aliyun-captcha-verify-param": "p"}, "forbidden", "验证码挑战"},
		{"jwt rejected 401", http.StatusUnauthorized, nil, `{"error":"jwt"}`, "重新登录"},
		{"context exceeded", http.StatusBadRequest, nil, `{"code":1261,"msg":"too long"}`, "上下文"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newStartPlanGateway(t, tc.status, tc.headers, tc.body)
			startPlanAnthropicEndpoint = g.srv.URL
			req := executorRequestJSON(t, map[string]any{
				"AuthID": "a1",
				"Model":  "glm-5.3-flash",
			}, map[string]any{"model": "glm-5.3-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}}}, startPlanAuthJSON())
			_, err := handleExecExecute(req)
			if err == nil {
				t.Fatalf("expected error for status %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want contains %q", err, tc.contains)
			}
		})
	}
}

const sampleAnthropicSSE = "event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_s","model":"glm-5.3-flash","usage":{"input_tokens":9,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi stream"}}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func TestHandleExecStreamStartPlanSyncFallback(t *testing.T) {
	g := newStartPlanGateway(t, http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, sampleAnthropicSSE)
	origEndpoint := startPlanAnthropicEndpoint
	startPlanAnthropicEndpoint = g.srv.URL + "/api/v1/zcode-plan/anthropic/v1/messages"
	defer func() { startPlanAnthropicEndpoint = origEndpoint }()

	req := executorRequestJSON(t, map[string]any{
		"AuthID":   "a1",
		"Model":    "glm-5.3-flash",
		"Stream":   true,
		"Metadata": map[string]any{"request_path": "/v1/chat/completions"},
	}, map[string]any{"model": "glm-5.3-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}}}, startPlanAuthJSON())
	raw, err := handleExecStream(req)
	if err != nil {
		t.Fatalf("handleExecStream: %v", err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("envelope = %s", raw)
	}
	var resp struct {
		Headers map[string][]string `json:"headers"`
		Chunks  []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"chunks"`
	}
	if json.Unmarshal(env.Result, &resp) != nil {
		t.Fatalf("result = %s", env.Result)
	}
	if len(resp.Chunks) < 3 {
		t.Fatalf("chunks = %d, want role+delta+finish", len(resp.Chunks))
	}
	var content strings.Builder
	var finish any
	for _, ch := range resp.Chunks {
		chunkBytes, decErr := base64.StdEncoding.DecodeString(strings.Trim(string(ch.Payload), `"`))
		if decErr != nil {
			t.Fatalf("chunk payload not base64: %v (%s)", decErr, ch.Payload)
		}
		var chunk map[string]any
		if json.Unmarshal(chunkBytes, &chunk) != nil {
			t.Fatalf("chunk not JSON: %s", chunkBytes)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk object = %v", chunk["object"])
		}
		choices := chunk["choices"].([]any)
		if len(choices) > 0 {
			c := choices[0].(map[string]any)
			if d, ok := c["delta"].(map[string]any)["content"].(string); ok {
				content.WriteString(d)
			}
			if c["finish_reason"] != nil {
				finish = c["finish_reason"]
			}
		}
	}
	if content.String() != "Hi stream" {
		t.Fatalf("content = %q", content.String())
	}
	if finish != "stop" {
		t.Fatalf("finish = %v", finish)
	}
	// The wire body must pin stream=true.
	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	var sent map[string]any
	if json.Unmarshal([]byte(calls[0].body), &sent) != nil {
		t.Fatalf("sent body not JSON")
	}
	if sent["stream"] != true {
		t.Fatalf("stream = %v, want true", sent["stream"])
	}
}

func TestFilterPlanModelsStartPlan(t *testing.T) {
	sa := &storedAuth{Auth: zcodeTokens{Plan: planStart, Provider: providerZai}}
	models := filterPlanModels(sa, zcodeModels())
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		ids[m.ID] = true
	}
	if len(models) != 3 {
		t.Fatalf("start-plan models = %v", ids)
	}
	for _, want := range []string{"glm-5.3-flash", "glm-5.2", "glm-5-turbo"} {
		if !ids[want] {
			t.Fatalf("missing %s in %v", want, ids)
		}
	}
	// coding-plan keeps everything.
	coding := &storedAuth{Auth: zcodeTokens{Plan: planCoding}}
	if got := filterPlanModels(coding, zcodeModels()); len(got) != len(zcodeModels()) {
		t.Fatalf("coding-plan models filtered: %d", len(got))
	}
}

// readAll drains an io.ReadCloser (test helper without importing io twice).
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

// pluginapiExecutorResponseProbe mirrors the executor response envelope shape
// the plugin emits (Payload is base64-encoded []byte in JSON).
type pluginapiExecutorResponseProbe struct {
	Payload json.RawMessage     `json:"payload"`
	Headers map[string][]string `json:"headers"`
}
