// context_projection.go — 请求侧上下文投影（opt-in，默认关；2026-10-18 用户定）。
//
// 为什么要有这一层：超大上下文请求打到 WorkBuddy 上游时，上游会自己把
// usage 记成 0/1（思考其实发生了，数字没留下）；成熟反代的做法是先把
// 请求投影成一个更小的形状再发。老师的默认同样是"不动"（preserve），
// 激进投影是逃生开关——所以这里也做成开关：plugins.configs.workbuddy
// 的 context_projection: on 才启用，默认关闭时请求体一个字节都不变。
//
// 做法是规则式（不调模型做摘要），常量照抄成熟反代的实测值：
//   - 保尾：最后 ≤8 条消息、累计 ≤7000 字符；tool 消息向前扩上配对的
//     assistant tool_calls，避免拆散工具对。
//   - 更早的历史：压成一行一条的摘要，≤10 行、≤2200 字符。
//   - system：首条保留（截断到 1200 字符）；其后的 system 折进摘要。
//   - tool 输出：超 1600 字符且超 16 行时，保头 10 行 + 尾 6 行。
//   - 自由文本：超限时保头 2/3、保尾 1/3（用户 3200 / 助手 1800 /
//     工具参数 900 字符）。
//
// 触发门槛：只有 messages 序列化后超过 projectionTriggerChars 才动——
// 正常请求永远不会走进这段逻辑（即使开关开着）。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	projectionTriggerChars = 150000
	maxSystemGuidanceChars = 1200
	maxUserChars           = 3200
	maxAssistantChars      = 1800
	maxToolOutputChars     = 1600
	maxToolArgsChars       = 900
	maxHistorySummaryChars = 2200
	maxHistoryItems        = 10
	maxTailMessages        = 8
	maxTailChars           = 7000
)

// projectContextInPlace applies the rule-based projection to obj["messages"].
// Returns true when the body was modified. Nil/absent messages, small bodies
// and a disabled switch are all no-ops.
func projectContextInPlace(obj map[string]any) bool {
	if obj == nil || !loadedContextProjection() {
		return false
	}
	rawMsgs, ok := obj["messages"].([]any)
	if !ok || len(rawMsgs) <= maxTailMessages {
		return false
	}
	msgs := make([]map[string]any, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		if mm, ok := m.(map[string]any); ok {
			msgs = append(msgs, mm)
		}
	}
	if len(msgs) == 0 || messagesChars(msgs) < projectionTriggerChars {
		return false
	}

	// 1. system 先分离：首条保留（截断），其余折进历史摘要。
	var systems []map[string]any
	var rest []map[string]any
	for _, m := range msgs {
		if roleIs(m, "system") {
			systems = append(systems, m)
			continue
		}
		rest = append(rest, m)
	}
	if len(systems) == 0 {
		// 上游（Global）要求首条是 system；这里不凭空造，交给既有
		// ensureSystemMessageInPlace 处理。
		rest = msgs
	}

	// 2. 保尾。
	tailStart := len(rest)
	tailChars := 0
	for i := len(rest) - 1; i >= 0; i-- {
		n := messageChars(rest[i])
		if len(rest)-i > maxTailMessages {
			break
		}
		if tailChars+n > maxTailChars && len(rest)-i > 1 {
			break
		}
		tailChars += n
		tailStart = i
	}
	// tool 配对扩展：tail 首条是 tool 结果时，先把前面连续的 tool 段整体收进来，
	// 再收那段结果对应的 assistant(tool_calls)。只前移一格会在多工具并行结果
	// （assistant → tool1 → tool2 → tool3）上留下没有前置 assistant 的 tool
	// 消息，上游会直接 400。
	for tailStart > 0 && roleIs(rest[tailStart], "tool") {
		for tailStart > 0 && roleIs(rest[tailStart-1], "tool") {
			tailStart--
		}
		if tailStart > 0 && assistantHasToolCalls(rest[tailStart-1]) {
			tailStart--
		}
		break
	}
	// 最新一条用户消息若落在 tail 之外，单独保留（否则模型看不到问题）。
	anchorIdx := -1
	if tailStart > 0 {
		for i := tailStart - 1; i >= 0; i-- {
			if roleIs(rest[i], "user") {
				anchorIdx = i
				break
			}
		}
	}

	// 3. 被省略的历史 → 规则摘要。首个 system 之外的指令同样属于"被省略的
	// 历史"，不能静默丢弃——折进摘要（原注释就是这么承诺的）。
	var historyLines []string
	// 注意：systems 可能为空（请求里一条 system 都没有）——空切片 s[1:] 会 panic。
	if len(systems) > 1 {
		for _, m := range systems[1:] {
			if line := historyLine(m); line != "" {
				historyLines = append(historyLines, line)
			}
		}
	}
	for i, m := range rest[:tailStart] {
		if i == anchorIdx {
			continue
		}
		if line := historyLine(m); line != "" {
			historyLines = append(historyLines, line)
		}
	}
	if len(historyLines) == 0 {
		return false
	}
	summary := summarizeHistory(historyLines)

	// 4. 组装：system（截断）→ 历史摘要 system → anchor user → tail（逐条修剪）。
	projected := make([]any, 0, len(systems)+len(rest)-tailStart+2)
	if len(systems) > 0 {
		first := cloneMap(systems[0])
		trimMessageContent(first, maxSystemGuidanceChars)
		projected = append(projected, first)
	}
	projected = append(projected, map[string]any{
		"role":    "system",
		"content": "【较早会话摘要 / earlier context summary】\n" + summary,
	})
	if anchorIdx >= 0 {
		u := cloneMap(rest[anchorIdx])
		trimMessageContent(u, maxUserChars)
		projected = append(projected, u)
	}
	for _, m := range rest[tailStart:] {
		c := cloneMap(m)
		trimForProjection(c)
		projected = append(projected, c)
	}

	obj["messages"] = projected
	return true
}

// loadedContextProjection is defined in usage_config.go (config snapshot).

// messagesChars estimates the serialized size of a message list.
func messagesChars(msgs []map[string]any) int {
	total := 0
	for _, m := range msgs {
		total += messageChars(m)
	}
	return total
}

func messageChars(m map[string]any) int {
	raw, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return len(raw)
}

func roleIs(m map[string]any, role string) bool {
	r, _ := m["role"].(string)
	return strings.EqualFold(strings.TrimSpace(r), role)
}

func assistantHasToolCalls(m map[string]any) bool {
	if !roleIs(m, "assistant") {
		return false
	}
	calls, ok := m["tool_calls"].([]any)
	return ok && len(calls) > 0
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// trimForProjection trims one tail message in place: tool output gets the
// line-aware treatment, user/assistant text the head/tail character clip.
func trimForProjection(m map[string]any) {
	switch {
	case roleIs(m, "tool"):
		trimContentWith(m, maxToolOutputChars, clipToolOutput)
	case roleIs(m, "user"):
		trimContentWith(m, maxUserChars, clipText)
	default:
		trimMessageContent(m, maxAssistantChars)
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			if args, ok := fn["arguments"].(string); ok && len(args) > maxToolArgsChars {
				fn["arguments"] = clipText(args, maxToolArgsChars)
			}
		}
	}
}

// trimContentWith clips string / text-part content with the given clipper.
func trimContentWith(m map[string]any, limit int, clip func(string, int) string) {
	switch c := m["content"].(type) {
	case string:
		if len(c) > limit {
			m["content"] = clip(c, limit)
		}
	case []any:
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok && len(t) > limit {
				part["text"] = clip(t, limit)
			}
		}
	}
}

// trimMessageContent clips a message's textual content (string or text parts).
func trimMessageContent(m map[string]any, limit int) {
	switch c := m["content"].(type) {
	case string:
		if len(c) > limit {
			m["content"] = clipText(c, limit)
		}
	case []any:
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok && len(t) > limit {
				part["text"] = clipText(t, limit)
			}
		}
	}
}

// clipText keeps the head 2/3 and the tail 1/3 of s, with an explicit marker
// in the middle. Never invents content. Runs on runes so Chinese/emoji are
// never cut mid-character.
func clipText(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	omitted := len(r) - head - tail
	return string(r[:head]) + fmt.Sprintf("\n[... omitted %d chars ...]\n", omitted) + string(r[len(r)-tail:])
}

// clipTail keeps the last limit runes (for the tail slice of a tool output).
func clipTail(s string, limit int) string {
	r := []rune(s)
	if limit <= 0 || len(r) <= limit {
		return s
	}
	return "[... omitted head ...]\n" + string(r[len(r)-limit:])
}

// clipToolOutput keeps the head 10 lines + tail 6 lines of a long tool output
// (the mature-proxy rule); short-but-large single-line outputs fall back to
// clipText. Line counts alone do not bound the size — the kept head/tail are
// clipped again so one huge line cannot defeat the char budget.
func clipToolOutput(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 16 {
		head := strings.Join(lines[:10], "\n")
		tail := strings.Join(lines[len(lines)-6:], "\n")
		headLimit := limit * 2 / 3
		tailLimit := limit - headLimit
		return clipText(head, headLimit) + fmt.Sprintf("\n[... omitted %d lines ...]\n", len(lines)-16) +
			clipTail(tail, tailLimit)
	}
	return clipText(s, limit)
}

// historyLine renders one omitted message as a single summary line.
func historyLine(m map[string]any) string {
	role, _ := m["role"].(string)
	text := messageText(m)
	switch {
	case strings.EqualFold(role, "assistant") && assistantHasToolCalls(m):
		names := toolCallNames(m)
		if names != "" {
			if text != "" {
				return "助手调用工具 " + names + "：" + briefText(text, 120)
			}
			return "助手调用工具 " + names
		}
		return "助手：" + briefText(text, 160)
	case strings.EqualFold(role, "tool"):
		return "工具返回：" + briefText(text, 160)
	case strings.EqualFold(role, "user"):
		return "用户：" + briefText(text, 160)
	case strings.EqualFold(role, "assistant"):
		return "助手：" + briefText(text, 160)
	case strings.EqualFold(role, "system"):
		return "系统指令：" + briefText(text, 160)
	default:
		if text != "" {
			return role + "：" + briefText(text, 160)
		}
	}
	return ""
}

// summarizeHistory joins history lines within the summary caps (10 lines /
// 2200 chars), collapsing the remainder into one count line.
func summarizeHistory(lines []string) string {
	var b strings.Builder
	used := 0
	kept := 0
	for i, line := range lines {
		if kept >= maxHistoryItems || used+len(line) > maxHistorySummaryChars {
			b.WriteString(fmt.Sprintf("…… 另有 %d 条更早消息已折叠\n", len(lines)-i))
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		used += len(line)
		kept++
	}
	return strings.TrimRight(b.String(), "\n")
}

func toolCallNames(m map[string]any) string {
	calls, ok := m["tool_calls"].([]any)
	if !ok {
		return ""
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// messageText renders a message's text content for the summary line.
func messageText(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok && strings.TrimSpace(t) != "" {
				parts = append(parts, strings.TrimSpace(t))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// briefText collapses whitespace and clips to n chars.
func briefText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
