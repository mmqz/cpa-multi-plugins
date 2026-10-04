package upstream

import (
	"encoding/json"
	"net/http"
	"testing"
)

// v0.12.50 大输入韧性：developer 归一 / 孤儿 tool 剔除 / 过大错误分类。

func TestPrepareBodyDeveloperRoleNormalized(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[
                {"role":"developer","content":"You are a coding agent."},
                {"role":"user","content":"hi"}]}`
	out := PrepareBody([]byte(src), "solo")
	var obj struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("messages=%d want 2", len(obj.Messages))
	}
	if obj.Messages[0].Role != "system" {
		t.Errorf("role[0]=%q want system", obj.Messages[0].Role)
	}
	// developer 的 content 仍走字符串→数组转换（同普通 system）。
	if len(obj.Messages[0].Content) != 1 || obj.Messages[0].Content[0]["type"] != "text" {
		t.Errorf("content[0]=%v want single text block", obj.Messages[0].Content)
	}
	if obj.Messages[1].Role != "user" {
		t.Errorf("role[1]=%q want user", obj.Messages[1].Role)
	}
}

func TestPrepareBodyOrphanToolResultsDropped(t *testing.T) {
	// 布局：assistant(tool a1) + tool(a1) 保留；tool(ghost) 悬空剔除；
	// 无名 tool_call 的 a2 被剔 → tool(a2) 成孤儿剔除；
	// tool_calls 全被剔且无 content 的 assistant 占位整条剔除。
	src := `{"model":"glm-5.2","messages":[
                {"role":"user","content":"run tools"},
                {"role":"assistant","tool_calls":[
                        {"id":"a1","type":"function","function":{"name":"ls","arguments":"{}"}},
                        {"id":"a2","type":"function","function":{"name":"","arguments":"{}"}}]},
                {"role":"tool","tool_call_id":"a1","content":"files"},
                {"role":"tool","tool_call_id":"a2","content":"orphan-by-nameless"},
                {"role":"tool","tool_call_id":"ghost","content":"orphan"},
                {"role":"user","content":"continue"}]}`
	out := PrepareBody([]byte(src), "solo")
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// user + assistant(保留 a1) + tool(a1) + user = 4
	if len(obj.Messages) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(obj.Messages), obj.Messages)
	}
	if obj.Messages[1]["role"] != "assistant" {
		t.Fatalf("msg[1].role=%v want assistant", obj.Messages[1]["role"])
	}
	tcs, _ := obj.Messages[1]["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("kept tool_calls=%d want 1", len(tcs))
	}
	if obj.Messages[2]["tool_call_id"] != "a1" {
		t.Errorf("msg[2].tool_call_id=%v want a1", obj.Messages[2]["tool_call_id"])
	}
}

func TestPrepareBodyEmptyAssistantPlaceholderDropped(t *testing.T) {
	// 无 name tool_call 且整条消息无 content → 占位剔除；带 content 保留。
	src := `{"model":"glm-5.2","messages":[
                {"role":"user","content":"go"},
                {"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"","arguments":"{}"}}]},
                {"role":"assistant","content":"plain reply"},
                {"role":"user","content":"end"}]}`
	out := PrepareBody([]byte(src), "solo")
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("messages=%d want 3 (placeholder dropped): %v", len(obj.Messages), obj.Messages)
	}
	if obj.Messages[1]["role"] != "assistant" {
		t.Errorf("msg[1].role=%v want assistant(plain reply)", obj.Messages[1]["role"])
	}
	// 无 tool_calls 的普通请求零改动（数量守恒）。
	src2 := `{"model":"glm-5.2","messages":[
                {"role":"system","content":"s"},{"role":"user","content":"u"}]}`
	out2 := PrepareBody([]byte(src2), "solo")
	var obj2 struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(out2, &obj2)
	if len(obj2.Messages) != 2 {
		t.Errorf("plain request messages=%d want 2", len(obj2.Messages))
	}
}

func TestClassifyInputTooLarge(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{http.StatusRequestEntityTooLarge, `<html>413 Request Entity Too Large</html>`, ErrInputTooLarge},
		{http.StatusRequestEntityTooLarge, ``, ErrInputTooLarge},
		// v0.12.51: 413 判定在最顶——body 带宽松 plan 字样也不得被劫持成
		// ErrPlanLimit（那会硬冷却健康账号 12h）
		{http.StatusRequestEntityTooLarge, `{"code":1005,"msg":"plan quota"}`, ErrInputTooLarge},
		{http.StatusRequestEntityTooLarge, `token limit 100500 exceeded for your plan`, ErrInputTooLarge},
		// 非 413 的宽松 plan 匹配保持原语义
		{http.StatusForbidden, `token limit 100500 exceeded for your plan`, ErrPlanLimit},
		{400, `{"code":4001,"msg":"prompt is too long: 200000 tokens > 131072 maximum context length"}`, ErrInputTooLarge},
		{400, `{"msg":"输入过长，请压缩上下文"}`, ErrInputTooLarge},
		{400, `{"msg":"context window exceeded"}`, ErrInputTooLarge},
		// 无过大文案的 400 仍归 ErrClient
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		// 401 无过大文案 → session dead（原语义不变）
		{401, `{"msg":"login required"}`, ErrSessionDead},
		// 429 优先级不变
		{429, `too many requests`, ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestMsgIndicatesInputTooLarge(t *testing.T) {
	hits := []string{
		"prompt is too long",
		"maximum context length exceeded",
		"This model's maximum context window is 131072 tokens",
		"HTTP 413 Request Entity Too Large",
		"输入过长，请压缩后重试",
		"上下文长度超出模型上限",
	}
	for _, s := range hits {
		if !MsgIndicatesInputTooLarge(s) {
			t.Errorf("MsgIndicatesInputTooLarge(%q)=false want true", s)
		}
	}
	misses := []string{"", "invalid param", "login required", "unauthorized"}
	for _, s := range misses {
		if MsgIndicatesInputTooLarge(s) {
			t.Errorf("MsgIndicatesInputTooLarge(%q)=true want false", s)
		}
	}
}

func TestSOLOStreamErrorKindInputTooLarge(t *testing.T) {
	if got := (&SOLOStreamError{Code: 0, Msg: "prompt is too long: 200000 > 131072"}).Kind(); got != ErrInputTooLarge {
		t.Errorf("Kind=%v want ErrInputTooLarge", got)
	}
	if got := (&SOLOStreamError{Code: 1005, Msg: "plan"}).Kind(); got != ErrPlanLimit {
		t.Errorf("Kind=%v want ErrPlanLimit", got)
	}
	// v0.12.79 (issue #9): 4001 非过大文案 = 模型不匹配（请求级，不冷却）。
	if got := (&SOLOStreamError{Code: 4001, Msg: "param is invalid"}).Kind(); got != ErrModelUnavailable {
		t.Errorf("Kind=%v want ErrModelUnavailable", got)
	}
}
