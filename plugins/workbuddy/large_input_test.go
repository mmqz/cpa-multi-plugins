package main

import (
	"strings"
	"testing"
)

// v0.9.15 大输入韧性：11128 渠道风控分类 / 过大词族扩展 / 413 无条件归类 /
// developer→system 归一与孤儿 tool 清理 / 生命周期守卫。

func TestIsPromptTooLargeExtended(t *testing.T) {
	// 413 无条件归类（含 HTML/空体，无业务信封）
	if !isPromptTooLong(413, `<html>Request Entity Too Large</html>`) {
		t.Error("bare 413 should be prompt-too-long")
	}
	if !isPromptTooLong(413, "") {
		t.Error("empty-body 413 should be prompt-too-long")
	}
	// 过大词族扩展（非 413 状态码 + 词命中）
	hits := []string{
		`{"msg":"This model's maximum context length is 131072 tokens"}`,
		`{"error":"context length exceeded"}`,
		`{"msg":"请求 token 数 too many tokens"}`,
		`{"msg":"输入过长，请压缩上下文"}`,
		`{"msg":"上下文过长"}`,
		"maximum context length exceeded",
	}
	for _, body := range hits {
		if !isPromptTooLong(400, body) {
			t.Errorf("isPromptTooLong(400,%q)=false want true", body)
		}
	}
	// 非过大 400 不误伤
	if isPromptTooLong(400, `{"code":11102,"msg":"service info not found"}`) {
		t.Error("11102 must not match too-long")
	}
	if isPromptTooLong(500, "maximum context length") {
		t.Error("5xx too-long wording should stay server-class (not 4xx-gated)")
	}
}

func TestIsChannelRiskControl(t *testing.T) {
	if !isChannelRiskControl(400, `{"code":11128,"msg":"请求被渠道风控拦截"}`) {
		t.Error("json 11128 should match")
	}
	if !isChannelRiskControl(403, `{"code": 11128, "msg":"blocked"}`) {
		t.Error("envelope-variant 11128 (spaces) should match")
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{200, `{"code":11128}`}, {400, `{"code":11102}`}, {400, `plain error`}} {
		if isChannelRiskControl(tc.status, tc.body) {
			t.Errorf("isChannelRiskControl(%d,%q)=true want false", tc.status, tc.body)
		}
	}
}

func TestTranslateChatUpstreamError11128Copy(t *testing.T) {
	err := translateChatUpstreamErrorFull(400, `{"code":11128,"msg":"blocked"}`, nil, nil)
	msg := err.Error()
	for _, want := range []string{"11128", "渠道风控", "与账号状态无关"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg missing %q: %s", want, msg)
		}
	}
	// 11102 优先级不变（不被 11128/过长改写）
	err2 := translateChatUpstreamErrorFull(400, `{"code":11102,"msg":"model [x] service info not found"}`, nil, nil)
	if !strings.Contains(err2.Error(), "11102") {
		t.Errorf("11102 copy lost: %s", err2.Error())
	}
}

func TestTranslateChatUpstreamErrorTooLargeCopy(t *testing.T) {
	err := translateChatUpstreamErrorFull(413, `<html></html>`, nil, nil)
	for _, want := range []string{"提示词过长", "与账号无关", "缩短上下文"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("msg missing %q: %s", want, err.Error())
		}
	}
}

func TestNormalizeHistoryInPlace(t *testing.T) {
	// developer 归一 + 无名 tool_call 剔除 + 空 assistant 占位丢弃 + 孤儿 tool 成对清理
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "developer", "content": "be brief"},
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "ok1", "type": "function", "function": map[string]any{"name": "ls", "arguments": "{}"}},
				map[string]any{"id": "bad1", "type": "function", "function": map[string]any{"name": "", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "ok1", "content": "out"},
			map[string]any{"role": "tool", "tool_call_id": "bad1", "content": "orphan"},
		},
	}
	if !normalizeHistoryInPlace(obj) {
		t.Fatal("expected changed=true")
	}
	msgs := obj["messages"].([]any)
	// developer(→system) + user + assistant(保留 ok1) + tool(ok1) = 4
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" {
		t.Errorf("msg[0].role=%v want system", m["role"])
	}
	tcs := msgs[2].(map[string]any)["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Errorf("kept tool_calls=%d want 1", len(tcs))
	}
}

func TestNormalizeHistoryCleanRequestUnchanged(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "s"},
		map[string]any{"role": "user", "content": "u"},
	}
	obj := map[string]any{"messages": msgs}
	if normalizeHistoryInPlace(obj) {
		t.Error("clean history must not be flagged changed")
	}
	if len(obj["messages"].([]any)) != 2 {
		t.Error("clean history must pass through unchanged")
	}
}

func TestReconcileGuardsSkipPromptTooLong(t *testing.T) {
	// 413/过长 body 不触发积分 reconcile（词表碰撞防护）。
	// reconcileAfterExecutorError 对非命中场景本就静默返回——这里验证
	// 守卫分支吞掉硬积分词碰撞体后不 panic 且不改变行为。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reconcileAfterExecutorError panicked: %v", r)
		}
	}()
	reconcileAfterExecutorError("nonexistent-auth-id", 413, `<html>quota exceeded too long</html>`)
	reconcileByUID("nonexistent-uid", 400, `{"msg":"prompt is too long, quota exceeded"}`)
}

// v0.9.16: 过长文案的 11115 码提及条件化——413 裸 HTML 不再硬提一个
// body 里不存在的码；11115 信封保留码提及。
func TestTranslateChatUpstreamErrorTooLargeCopyPreciseCode(t *testing.T) {
	err := translateChatUpstreamErrorFull(413, `<html></html>`, nil, nil)
	if strings.Contains(err.Error(), "11115") {
		t.Errorf("413 HTML copy should not claim code 11115: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "413/context limit exceeded") {
		t.Errorf("413 detail missing: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "缩短上下文") || !strings.Contains(err.Error(), "与账号无关") {
		t.Errorf("guidance missing: %s", err.Error())
	}
	err2 := translateChatUpstreamErrorFull(400, `{"code":11115,"msg":"prompt is too long"}`, nil, nil)
	if !strings.Contains(err2.Error(), "code 11115 prompt is too long") {
		t.Errorf("11115 detail missing: %s", err2.Error())
	}
}
