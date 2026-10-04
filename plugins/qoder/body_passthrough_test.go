package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 0.8.11 large-input resilience:
//   - verbatim message passthrough (tool_calls/tool_call_id/structured content)
//   - slim mode: client system present -> template system+tools dropped
//     (upstream-verified 2026-09-18: neither required, ~10K -> ~60 tokens)
//   - client tools forwarded verbatim
//   - oversized-input rejections render actionable, account-innocent copy

func TestOpenAIMessageVerbatimRoundTrip(t *testing.T) {
	raw := `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}],"tool_call_id":null}`
	var m openAIMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != "assistant" {
		t.Errorf("role = %q", m.Role)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if _, ok := back["tool_calls"]; !ok {
		t.Error("tool_calls lost in roundtrip")
	}
	if _, ok := back["tool_call_id"]; !ok {
		t.Error("explicit null member dropped")
	}
	if v, _ := back["role"].(string); v != "assistant" {
		t.Errorf("role roundtrip = %v", v)
	}
}

func TestOpenAIMessageNullContentRoundTrip(t *testing.T) {
	// 0.8.12: content:null (the common assistant+tool_calls shape) must
	// round-trip as null, not collapse to "" (verbatim contract).
	raw := `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`
	var m openAIMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	v, ok := back["content"]
	if !ok {
		t.Fatal("content member dropped")
	}
	if v != nil {
		t.Errorf("content null drifted to %v", v)
	}
	if _, ok := back["tool_calls"]; !ok {
		t.Error("tool_calls lost in roundtrip")
	}
}

func TestOpenAIMessageStructuredContentRoundTrip(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"阅读这段"},{"type":"text","text":"代码"}]}`
	var m openAIMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"type":"text"`) || !strings.Contains(string(out), "阅读这段") {
		t.Errorf("structured content not preserved: %s", out)
	}
	if got := messageTextContent(m); got != "阅读这段\n代码" {
		t.Errorf("messageTextContent = %q", got)
	}
}

func TestExtractLatestUserPromptStructured(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Content: "first"},
		{Role: "user", rawContent: `[{"type":"text","text":"second"},{"type":"image_url"}]`, contentSet: true},
	}
	if got := extractLatestUserPrompt(msgs); got != "second" {
		t.Errorf("extractLatestUserPrompt = %q, want second", got)
	}
}

func slimRequest() *openAIRequest {
	return &openAIRequest{
		Model: "qfmodel",
		Messages: []openAIMessage{
			mustMsg(`{"role":"system","content":"you are a coding harness"}`),
			mustMsg(`{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}`),
			mustMsg(`{"role":"user","content":"继续"}`),
		},
	}
}

func mustMsg(raw string) openAIMessage {
	var m openAIMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic("static message fixture must parse: " + err.Error())
	}
	return m
}

func TestBuildQoderBodySlimPassthrough(t *testing.T) {
	raw, err := buildQoderBody(slimRequest(), "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json: %v", err)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (no template system injected)", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if v, _ := first["role"].(string); v != "system" {
		t.Errorf("messages[0].role = %v, want client system", v)
	}
	if v, _ := first["content"].(string); v != "you are a coding harness" {
		t.Errorf("messages[0].content = %q — template system must be dropped in slim mode", v)
	}
	second, _ := msgs[1].(map[string]any)
	if _, ok := second["tool_calls"]; !ok {
		t.Error("assistant tool_calls lost in passthrough")
	}
	if _, ok := m["tools"]; ok {
		t.Error("template tools must be dropped in slim mode without client tools")
	}
	// reasoning chain stays on (0.8.10 semantics)
	mc, _ := m["model_config"].(map[string]any)
	if v, _ := mc["is_reasoning"].(bool); !v {
		t.Error("slim mode lost is_reasoning=true")
	}
}

func TestBuildQoderBodyClientToolsOverride(t *testing.T) {
	req := slimRequest()
	req.Tools = json.RawMessage(`[{"type":"function","function":{"name":"client_tool","parameters":{}}}]`)
	raw, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1 client tool", len(tools))
	}
	tf, _ := tools[0].(map[string]any)
	fn, _ := tf["function"].(map[string]any)
	if v, _ := fn["name"].(string); v != "client_tool" {
		t.Errorf("client tool name = %v", v)
	}
}

func TestBuildQoderBodyBarePromptKeepsTemplate(t *testing.T) {
	req := &openAIRequest{Model: "qfmodel", Messages: []openAIMessage{{Role: "user", Content: "你好"}}}
	raw, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	msgs, _ := m["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("messages len = %d, want template system + user", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if v, _ := first["role"].(string); v != "system" {
		t.Errorf("bare prompt: messages[0].role = %v, want template system", v)
	}
	if _, ok := m["tools"]; !ok {
		t.Error("bare prompt: template tools must stay")
	}
}

func TestChatInputTooLargeMatrix(t *testing.T) {
	yes := []struct {
		status int
		body   string
	}{
		{413, `{"error":"request entity too large"}`},
		{400, `{"code":400,"msg":"prompt is too long: 210000 tokens > 180000"}`},
		{400, `{"message":"input tokens exceed the context length"}`},
		{200, `{"code":4001,"msg":"内容过长，请精简后重试"}`},
		{400, `{"error":"上下文超过最大限制"}`},
	}
	for _, c := range yes {
		if !chatInputTooLarge(c.status, c.body) {
			t.Errorf("chatInputTooLarge(%d, %q) = false, want true", c.status, c.body)
		}
	}
	no := []struct {
		status int
		body   string
	}{
		{400, `{"error":"invalid model key"}`},
		{402, `{"msg":"insufficient credit"}`},
		{429, `{"msg":"rate limit"}`},
		{500, `internal error`},
	}
	for _, c := range no {
		if chatInputTooLarge(c.status, c.body) {
			t.Errorf("chatInputTooLarge(%d, %q) = true, want false", c.status, c.body)
		}
	}
}

func TestChatUpstreamErrorCopy(t *testing.T) {
	err := chatUpstreamError(413, `{"error":"request entity too large"}`)
	if err == nil {
		t.Fatal("nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "输入过大") || !strings.Contains(msg, "与账号无关") {
		t.Errorf("oversized copy missing: %s", msg)
	}
	if strings.HasPrefix(msg, "upstream ") {
		t.Errorf("oversized error should lead with actionable copy: %s", msg)
	}
	plain := chatUpstreamError(500, "boom").Error()
	if !strings.HasPrefix(plain, "upstream 500") {
		t.Errorf("plain error shape changed: %s", plain)
	}
}

func TestRuneSafePrefix(t *testing.T) {
	s := strings.Repeat("码", 40)
	got := runeSafePrefix(s, 30)
	if len(got) != 30*3 {
		t.Errorf("runeSafePrefix split UTF-8: byte len %d", len(got))
	}
	if utf8Count(got) != 30 {
		t.Errorf("rune count = %d, want 30", utf8Count(got))
	}
	if runeSafePrefix("abc", 30) != "abc" {
		t.Error("short string must pass through")
	}
}

func utf8Count(s string) int {
	return len([]rune(s))
}
