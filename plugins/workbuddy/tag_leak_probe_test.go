// tag_leak_probe_test.go — 2026-10-22 探针用例：逃逸的裸思考标签要被记下。
package main

import "testing"

// 变体开标签的两种命运：前导完整块被正常剥除（不置位，内容没漏）；
// 正文中间的变体标签逃过剥标签器（置位，这才是"漏"）。
func TestThinkLeakProbeFlagsVariants(t *testing.T) {
	// 前导完整块（变体写法）→ 正常剥除，不置位。
	s := newThoughtTagScrubber()
	out := s.feed("<think >思考内容</think>") + s.flush()
	if !s.scrubbed || s.leakTag != "" {
		t.Fatalf("闭合的前导块应被剥除且不算漏：out=%q scrubbed=%v leak=%q", out, s.scrubbed, s.leakTag)
	}
	if out != "" {
		t.Fatalf("剥除后不应有残留输出：%q", out)
	}
	s = newThoughtTagScrubber()
	out = s.feed("<THOUGHT>思考内容</THOUGHT>") + s.flush()
	if !s.scrubbed || s.leakTag != "" {
		t.Fatalf("大写变体应被剥除且不算漏：out=%q scrubbed=%v leak=%q", out, s.scrubbed, s.leakTag)
	}
	// 正文中间的变体标签 → 逃过剥标签器 → 置位。
	s = newThoughtTagScrubber()
	out = s.feed("正文<think\n>内联思考") + s.flush()
	if s.leakTag != "think" {
		t.Fatalf("中途变体应置位探针：out=%q leak=%q", out, s.leakTag)
	}
}

// 干净内容与合法 HTML（<summary>、<thead>）不得误报。
func TestThinkLeakProbeIgnoresCleanContent(t *testing.T) {
	for _, frag := range []string{
		"正常的中文回答，没有任何标签。",
		"<details><summary>展开</summary>内容</details>",
		"<thead><tr><th>列</th></tr></thead>",
		"<code>&lt;think&gt;</code>",
	} {
		s := newThoughtTagScrubber()
		out := s.feed(frag)
		out += s.flush()
		if s.leakTag != "" {
			t.Fatalf("frag %q 不该置位探针（误报），输出=%q", frag, out)
		}
		if out == "" {
			t.Fatalf("frag %q 内容被吃掉了：%q", frag, out)
		}
	}
}

// reportThinkLeak：有漏打一行、无漏静默（不直接可测 println，这里只验选择逻辑）。
func TestReportThinkLeakPicksLeakedScrubber(t *testing.T) {
	state := map[int]*thoughtTagScrubber{0: newThoughtTagScrubber()}
	reportThinkLeak(state, "m", "uid", "p") // 全干净：不 panic 即可
	state[1] = &thoughtTagScrubber{leakTag: "think"}
	reportThinkLeak(state, "m", "uid-1234567890", "p") // 命中：uid 取尾 8 位
}
