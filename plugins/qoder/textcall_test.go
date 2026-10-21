// textcall_test.go — 文字版工具调用认领的测试（v0.8.22，2026-10-17）。
// 样本照实测那一次抄：模型只写了块尾巴，函数名靠参数对工具清单认领。
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// leakedSample 是实测泄漏的等价样本（命令换成简短英文，形状一字不改）。
const leakedSample = "Calling tools.\n" +
	"<parameter name=\"arguments\">{\"cmd\": \"cd 'D:/proj'; git log -1 --stat\", \"max_output_tokens\": 2500}\n" +
	"</parameter>\n" +
	"</function>\n" +
	"</tool_call>\n\n"

func testSpecs() []toolSpec {
	return []toolSpec{
		{Name: "exec_command", Required: []string{"cmd"},
			Props: []string{"cmd", "max_output_tokens", "workdir", "yield_time_ms"}},
		{Name: "view_image", Required: []string{"path"}, Props: []string{"path"}},
	}
}

func TestSalvageRealLeakSample(t *testing.T) {
	cleaned, calls := salvageText(leakedSample, testSpecs())
	if len(calls) != 1 {
		t.Fatalf("认领失败：calls=%d（实测样本必须认出来）", len(calls))
	}
	if cleaned != "" {
		t.Errorf("正文该清干净，实际=%q", cleaned)
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn["name"] != "exec_command" {
		t.Errorf("名字该按参数认领成 exec_command，实际=%v", fn["name"])
	}
	args, _ := fn["arguments"].(string)
	var parsed map[string]any
	if json.Unmarshal([]byte(args), &parsed) != nil {
		t.Fatalf("参数不是合法 JSON: %q", args)
	}
	if parsed["cmd"] != "cd 'D:/proj'; git log -1 --stat" {
		t.Errorf("命令没带对: %v", parsed["cmd"])
	}
	if parsed["max_output_tokens"] != float64(2500) {
		t.Errorf("数字参数被改坏了: %v", parsed["max_output_tokens"])
	}
}

func TestSalvagePerParameterForm(t *testing.T) {
	text := "<tool_call>\n<function=exec_command>\n<parameter name=\"cmd\">echo hi</parameter>\n</tool_call>"
	cleaned, calls := salvageText(text, testSpecs())
	if len(calls) != 1 || cleaned != "" {
		t.Fatalf("逐参数写法也该认出来：calls=%d cleaned=%q", len(calls), cleaned)
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn["name"] != "exec_command" {
		t.Errorf("显式 <function=…> 该直接用，实际=%v", fn["name"])
	}
	if fn["arguments"] != `{"cmd":"echo hi"}` {
		t.Errorf("逐参数该拼成对象，实际=%v", fn["arguments"])
	}
}

func TestSalvageLeavesUnknownAndMidTextAlone(t *testing.T) {
	// 1) 参数对不上任何工具 → 原样放过（不猜）
	odd := "Calling tools.\n<parameter name=\"zzz\">1</parameter>\n</tool_call>"
	if cleaned, calls := salvageText(odd, testSpecs()); len(calls) != 0 || cleaned != odd {
		t.Errorf("认不出名字就该原样放过：calls=%d cleaned=%q", len(calls), cleaned)
	}
	// 2) 开头是正常话、后面才提到这套标记 → 不动（只在消息开头生效）
	mid := "先说明一下。\n<parameter name=\"cmd\">echo hi</parameter>\n</tool_call>"
	if _, calls := salvageText(mid, testSpecs()); len(calls) != 0 {
		t.Errorf("中途出现的不该认领")
	}
	// 3) 块没写完 → 原样放过
	partial := "Calling tools.\n<parameter name=\"arguments\">{\"cmd\": \"echo hi\"}"
	if _, calls := salvageText(partial, testSpecs()); len(calls) != 0 {
		t.Errorf("块没写完不该认领")
	}
}

// 把样本拆成几块喂给守卫：应当攒住、最后换成一块标准调用，原文一个字不出门。
func TestGuardConvertsAcrossChunks(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	pieces := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Calling to"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ols.\n<parameter name=\"arguments\">{\"cmd\": \"echo hi\"}</parameter>\n</function>\n"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"</tool_call>\n"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
	}
	var out []string
	for _, p := range pieces {
		out = append(out, g.Filter(p)...)
	}
	out = append(out, g.Flush()...)
	joined := strings.Join(out, "\n")
	for _, leak := range []string{"<parameter", "Calling to", "</tool_call>", "</function>"} {
		if strings.Contains(joined, leak) {
			t.Errorf("原文（%s）不该出门：%s", leak, joined)
		}
	}
	var found bool
	for _, o := range out {
		var obj map[string]any
		if json.Unmarshal([]byte(o), &obj) != nil {
			continue
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any) // 只看 delta 里的调用，不看 finish_reason
		if len(tcs) == 0 {
			continue
		}
		found = true
		call, _ := tcs[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if fn["name"] != "exec_command" {
			t.Errorf("标准调用块里名字不对：%v", fn["name"])
		}
		if args, _ := fn["arguments"].(string); !strings.Contains(args, "echo hi") {
			t.Errorf("参数没带对：%q", args)
		}
	}
	if !found {
		t.Errorf("没有产出标准调用块：%s", joined)
	}
}

// 已经出过标准调用 → 一个字都不改写（一次调用绝不能跑两遍）。
func TestGuardNeverRewritesAfterStructuredCall(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}}]}`
	_ = g.Filter(first)
	leak := `{"choices":[{"index":0,"delta":{"content":"Calling tools.\n<parameter name=\"arguments\">{\"cmd\": \"echo hi\"}</parameter>\n</tool_call>"}}]}`
	out := g.Filter(leak)
	if len(out) != 1 || out[0] != leak {
		t.Errorf("出过标准调用后必须原样透传，实际=%v", out)
	}
}

// 块一直没写完（流结束）→ 攒着的原文原样吐回，不丢字。
func TestGuardFlushesIncompleteBlock(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	_ = g.Filter(`{"choices":[{"index":0,"delta":{"content":"Calling tools.\n<parameter name=\"arguments\">{\"cmd\": \"echo hi\"}"}}]}`)
	out := g.Flush()
	if len(out) != 1 || !strings.Contains(out[0], "<parameter") {
		t.Errorf("没写完的块该原样吐回，实际=%v", out)
	}
}

// 裸前缀也是泄漏：模型先说一句 "Calling tools."，随后才发标准工具调用。
// 这句话必须被吞掉；真正有内容的正文仍要原样保留。
func TestGuardDropsBareCallingToolsBeforeStructuredCall(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	pieces := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Calling"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":" tools.\n\n"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	var out []string
	for _, piece := range pieces {
		out = append(out, g.Filter(piece)...)
	}
	out = append(out, g.Flush()...)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Calling tools") {
		t.Fatalf("裸前缀不该出门：%s", joined)
	}
	var found bool
	for _, o := range out {
		var obj map[string]any
		if json.Unmarshal([]byte(o), &obj) != nil {
			continue
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		if tcs, _ := d["tool_calls"].([]any); len(tcs) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("标准调用丢了：%s", joined)
	}
}

// 旧会话里已经有过 "." 占位词；新历史不再注入它，但当前流仍可能回显。
// 只拦这个孤立占位，正常正文里的句号不受影响。
func TestGuardDropsDotPlaceholderBeforeStructuredCall(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	pieces := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	var out []string
	for _, piece := range pieces {
		out = append(out, g.Filter(piece)...)
	}
	out = append(out, g.Flush()...)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, `"content":"."`) || strings.Contains(joined, `"text":"."`) {
		t.Fatalf("孤立点号占位不该出门：%s", joined)
	}
	var found bool
	for _, o := range out {
		var obj map[string]any
		if json.Unmarshal([]byte(o), &obj) != nil {
			continue
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		if tcs, _ := d["tool_calls"].([]any); len(tcs) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("标准调用丢了：%s", joined)
	}
}

// reasoning_content 必须原样透传；守卫只处理 content/tool_calls，不碰草稿通道。
func TestGuardLeavesReasoningContentAlone(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	chunk := `{"choices":[{"index":0,"delta":{"reasoning_content":"我先检查配置"}}]}`
	out := g.Filter(chunk)
	if len(out) != 1 || out[0] != chunk {
		t.Fatalf("reasoning_content 被改动：%v", out)
	}
}

// ── 2026-10-22：中途认领（正文中间完整闭合的文字版调用）────────────────────

// 正文中段的完整块（同一块内完整出现）：前缀正文保留、调用换成标准结构、
// XML 原文不漏。
func TestMidScanClaimsCompleteMidBlock(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	outs := g.Filter(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"先说明一下。\n<tool_call>\n<function=exec_command>\n<parameter name=\"cmd\">echo hi</parameter>\n</tool_call>"}}]}`)
	outs = append(outs, g.Flush()...)
	joined := strings.Join(outs, "")
	if !strings.Contains(joined, "先说明一下。") {
		t.Errorf("前缀正文丢了：%s", joined)
	}
	if strings.Contains(joined, "<tool_call>") {
		t.Errorf("XML 原文漏出去了：%s", joined)
	}
	if !strings.Contains(joined, "call_salvage_") {
		t.Errorf("没换成标准调用：%s", joined)
	}
	if !strings.Contains(joined, "echo hi") {
		t.Errorf("参数内容丢了：%s", joined)
	}
}

// 认不出来的块（参数对不上任何工具）→ 整块原样保留，一个字不丢。
func TestMidScanPassesThroughUnknownBlock(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	outs := g.Filter(`{"choices":[{"index":0,"delta":{"content":"正文开头。\n<tool_call>\n<parameter name=\"zzz\">1</parameter>\n</tool_call>"}}]}`)
	outs = append(outs, g.Flush()...)
	joined := strings.Join(outs, "")
	if !strings.Contains(joined, "<tool_call>") || !strings.Contains(joined, "zzz") {
		t.Errorf("认领失败的块该原样保留：%s", joined)
	}
}

// 跨 chunk 的整块：开标记跨界到达时不认领（fail-open），文字原样透传，
// 一个字不丢——跨块拼接由调用方的流缓冲保证，网关不做跨块拼图。
func TestMidScanPassesCrossBlockThrough(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	var outs []string
	outs = append(outs, g.Filter(`{"choices":[{"index":0,"delta":{"content":"开头。"}}]}`)...)
	outs = append(outs, g.Filter(`{"choices":[{"index":0,"delta":{"content":"<tool_"}}]}`)...)
	outs = append(outs, g.Filter(`{"choices":[{"index":0,"delta":{"content":"call>\n<function=exec_command>\n<parameter name=\"cmd\">echo hi</parameter>\n</tool_call>"}}]}`)...)
	outs = append(outs, g.Flush()...)
	joined := strings.Join(outs, "")
	if !strings.Contains(joined, "开头。") {
		t.Errorf("前缀正文丢了：%s", joined)
	}
	if !strings.Contains(joined, "<tool_") || !strings.Contains(joined, "call>") || !strings.Contains(joined, "echo hi") {
		t.Errorf("跨界块该原样透传一个字不丢：%s", joined)
	}
}

// 纯正文（没有文字版调用）：原样透传，一个字不丢。
func TestMidScanLeavesPlainTextAlone(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	outs := g.Filter(`{"choices":[{"index":0,"delta":{"content":"正常的短正文"}}]}`)
	outs = append(outs, g.Flush()...)
	joined := strings.Join(outs, "")
	if !strings.Contains(joined, "正常的短正文") {
		t.Errorf("纯正文被改动了：%s", joined)
	}
}

// 出过标准调用之后：中途扫描一律不插手（一次调用绝不能跑两遍）。
func TestMidScanDisabledAfterStructuredCall(t *testing.T) {
	g := newTextCallGuard(testSpecs())
	_ = g.Filter(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}}]}`)
	leak := `{"choices":[{"index":0,"delta":{"content":"中间。\n<tool_call>\n<function=exec_command>\n<parameter name=\"cmd\">echo hi</parameter>\n</tool_call>"}}]}`
	out := g.Filter(leak)
	joined := strings.Join(out, "")
	if !strings.Contains(joined, "<tool_call>") {
		t.Errorf("出过标准调用后必须原样透传：%s", joined)
	}
}
