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
