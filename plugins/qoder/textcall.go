// textcall.go — 文字版工具调用的认领（v0.8.22，2026-10-17）。
//
// 问题：Qwen 偶尔不按标准结构回工具调用，而是把调用**写成了正文文字**。
// 客户端只认标准结构，于是把这段文字原样贴进对话，而且——更要紧——这次调用
// **根本没执行**。实测原文（2026-10-17 02:59 会话，Q-Qwen 3.8 Flash）：
//
//	Calling tools.
//	<parameter name="arguments">{"cmd": "cd 'D:\\项目\\ide-checkin'; git log …", "max_output_tokens": 2500}
//	</parameter>
//	</function>
//	</tool_call>
//
// 注意：开头的 <tool_call> / <function=名字> 两段**它没写出来**——模型只写了
// 它模仿的那套形状的尾巴。所以函数名得从参数本身反推（拿客户端声明的 tools 对）。
//
// 规矩（一律保守）：
//   - 只看助手这条消息**开头**的文字（实测泄漏就发生在开头；中途出现的不理会，
//     免得把正常讨论误伤成调用）；
//   - 这一轮已经出现过标准调用 → 一概不再改写（一次调用绝不能执行两遍）；
//   - 名字反推不出来 → 原样放过（保持现状，不猜）；
//   - 攒着的文字在任何没走通的路径上都会**原样吐回**，一个字不丢。
package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// toolSpec 是"客户端声明的工具"的瘦身版：名字 + 必填参数 + 全部参数名。
// 只用来给缺名字的文字版调用认领一个身份。
type toolSpec struct {
	Name     string
	Required []string
	Props    []string
}

// parseToolSpecs 从客户端请求的 tools 字段里抽出工具清单。
// 结构不认识就返回空表（等于关掉认领，行为与从前一致）。
func parseToolSpecs(raw json.RawMessage) []toolSpec {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	specs := make([]toolSpec, 0, len(arr))
	for _, t := range arr {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			// Responses 风格：{type:"function", name:"…", parameters:{…}}
			fn = t
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		spec := toolSpec{Name: name}
		if params, ok := fn["parameters"].(map[string]any); ok {
			if req, ok := params["required"].([]any); ok {
				for _, r := range req {
					if s, ok := r.(string); ok {
						spec.Required = append(spec.Required, s)
					}
				}
			}
			if props, ok := params["properties"].(map[string]any); ok {
				for k := range props {
					spec.Props = append(spec.Props, k)
				}
				sort.Strings(spec.Props)
			}
		}
		specs = append(specs, spec)
	}
	return specs
}

// 文字版调用的"开头标记"。实测那一次是 "Calling tools."（模型把客户端界面上
// 那行标题也一起写进去了），其余三种是这套 XML 形状通常的开头。
var textCallMarkers = []string{"Calling tools", "<tool_call", "<function=", "<parameter name="}

// markerState 看这段文字还能不能长成一个调用开头：
// 0 = 不可能；1 = 还差一点（继续攒着看）；2 = 已经对上了。
func markerState(s string) int {
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" {
		return 1 // 还没看到实字，先攒着（避免把开头空白当结论）
	}
	best := 0
	low := strings.ToLower(t)
	for _, m := range textCallMarkers {
		ml := strings.ToLower(m)
		if strings.HasPrefix(low, ml) {
			return 2
		}
		if strings.HasPrefix(ml, low) {
			best = 1
		}
	}
	return best
}

// isBareCallingToolsMarker 判断攒下的文字是否只有 "Calling tools." 这句前缀。
// 只吞这一种裸标记；后面带了真正正文、或长成了 XML 调用，都不在这里猜。
func isBareCallingToolsMarker(s string) bool {
	t := strings.TrimSpace(s)
	return strings.EqualFold(t, "Calling tools") || strings.EqualFold(t, "Calling tools.")
}

// isInvisibleToolCallPlaceholder 识别旧版注入的孤立 "."，以及新版零宽占位符。
// 只吞这种无语义占位；正常正文里的句号、省略号或任何说明文字都不动。
func isInvisibleToolCallPlaceholder(s string) bool {
	t := strings.TrimSpace(s)
	return t == "." || t == "\u200b"
}

// blockEnd 在攒下的文字里找这个块的结尾；返回结束下标（含结尾标记）。
// 找到 </tool_call> 最稳；退一步认 </function>，再退一步认 </parameter>。
func blockEnd(s string) (int, bool) {
	for _, tail := range []string{"</tool_call>", "</function>", "</parameter>"} {
		if i := strings.Index(s, tail); i >= 0 {
			return i + len(tail), true
		}
	}
	return 0, false
}

// parseTextBlock 从块里抠出参数与函数名。
// 支持两种参数写法：
//   - <parameter name="arguments">{…JSON…}</parameter>   ← 实测这一种
//   - <parameter name="cmd">值</parameter>               ← 逐参数写法
//
// 另认 <function=名字> / <function name="名字"> 两种显式名字。
func parseTextBlock(block string) (params map[string]string, explicitName string) {
	params = map[string]string{}
	rest := block
	for {
		i := strings.Index(rest, "<parameter name=")
		if i < 0 {
			break
		}
		rest = rest[i+len("<parameter name="):]
		// 名字：允许 "name" 或 name
		rest = strings.TrimLeft(rest, " ")
		if strings.HasPrefix(rest, "\"") {
			rest = rest[1:]
		}
		j := strings.Index(rest, "\"")
		if j < 0 {
			break
		}
		key := rest[:j]
		rest = rest[j+1:]
		k := strings.Index(rest, ">")
		if k < 0 {
			break
		}
		rest = rest[k+1:]
		e := strings.Index(rest, "</parameter>")
		if e < 0 {
			params[key] = rest
			break
		}
		params[key] = rest[:e]
		rest = rest[e:]
	}
	for _, pat := range []string{"<function=", "<function name="} {
		if i := strings.Index(block, pat); i >= 0 {
			tail := block[i+len(pat):]
			tail = strings.TrimLeft(tail, " \"")
			if j := strings.IndexAny(tail, ">\""); j > 0 {
				explicitName = strings.TrimSpace(tail[:j])
			}
		}
	}
	return params, explicitName
}

// buildArgsJSON 把参数拼成客户端要的那段 JSON 文本。
// 只有一个 arguments/args/input 参数且本身是合法 JSON → 原样用（实测这一种）；
// 否则按参数名拼成对象，值能当 JSON 解析的就当 JSON，不行的当字符串。
func buildArgsJSON(params map[string]string) (string, bool) {
	if len(params) == 1 {
		for k, v := range params {
			switch k {
			case "arguments", "args", "input", "payload":
				trimmed := strings.TrimSpace(v)
				if json.Valid([]byte(trimmed)) {
					return trimmed, true
				}
			}
		}
	}
	if len(params) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for n, k := range keys {
		if n > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		raw := strings.TrimSpace(params[k])
		if json.Valid([]byte(raw)) {
			b.WriteString(raw)
			continue
		}
		vb, err := json.Marshal(params[k])
		if err != nil {
			return "", false
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String(), true
}

// resolveName 给缺名字的调用认领一个身份：拿参数名去对客户端声明的 tools，
// 必填项全被满足、且参数名集合最贴合的那个胜出；对不上就不认领。
func resolveName(explicit string, argsJSON string, specs []toolSpec) string {
	if explicit != "" {
		return explicit
	}
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args) == 0 {
		return ""
	}
	keys := map[string]bool{}
	for k := range args {
		keys[k] = true
	}
	best, bestScore, tie := "", -1, false
	for _, spec := range specs {
		if len(spec.Required) == 0 {
			continue
		}
		ok := true
		for _, r := range spec.Required {
			if !keys[r] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		score := len(spec.Required)*100 - abs(len(spec.Props)-len(keys))
		if score > bestScore {
			best, bestScore, tie = spec.Name, score, false
		} else if score == bestScore {
			tie = true
		}
	}
	if tie {
		return "" // 两个工具都说得通 → 不猜
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// salvageText 把一段文字里的文字版调用认领出来。
// 返回：清干净的文字（可能为空串）与调用（0 或 1 个，认不出名字就是 0 个）。
func salvageText(text string, specs []toolSpec) (string, []map[string]any) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if markerState(trimmed) != 2 {
		return text, nil
	}
	end, ok := blockEnd(trimmed)
	if !ok {
		return text, nil
	}
	block := trimmed[:end]
	params, explicit := parseTextBlock(block)
	argsJSON, okArgs := buildArgsJSON(params)
	if !okArgs {
		return text, nil
	}
	name := resolveName(explicit, argsJSON, specs)
	if name == "" {
		return text, nil
	}
	cleaned := strings.TrimSpace(trimmed[end:])
	call := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": argsJSON,
		},
	}
	return cleaned, []map[string]any{call}
}

// -----------------------------------------------------------------------------
// 流式守卫：把"可能正在长成调用"的文字先攒着，确认了再换成标准调用。
// 只在助手这条消息**开头**生效；攒着的文字在任何不确定的路径上都原样吐回。
// -----------------------------------------------------------------------------

type textCallGuard struct {
	specs    []toolSpec
	buf      strings.Builder
	head     string         // 开头的文字（判标记用，最多留 64 字节）
	holding  bool           // 已经在攒块
	off      bool           // 这条消息不再看（判完了或见过标准调用）
	swallow  bool           // 块已认领，还在等它最后那句 </tool_call>（别让它漏出去）
	tailBuf  string         // 吞尾巴时攒的零头
	sawCall  bool           // 这条消息已经出过标准调用
	template map[string]any // 第一块 chunk 当模板，用来造新 chunk
	seq      int
	// 中途认领（2026-10-22）：开头守卫只看消息开头，模型把调用写到正文
	// 中段时就漏了。这里只认「同一个 content 块内完整出现」的
	// <tool_call>…</tool_call> 整块——成功换标准调用，失败原样保留。
	// 跨块的整块不碰（fail-open 原样透传）。一条消息只认领一次。
	midDone bool
}

func newTextCallGuard(specs []toolSpec) *textCallGuard { return &textCallGuard{specs: specs} }

// Filter 吃一块 chat-completion chunk，返回真正该发出去的一串 chunk。
func (g *textCallGuard) Filter(chunk string) []string {
	var obj map[string]any
	if json.Unmarshal([]byte(chunk), &obj) != nil {
		return []string{chunk}
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return []string{chunk}
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return []string{chunk}
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return []string{chunk}
	}
	if g.template == nil {
		g.template = obj
	}
	if tcs, isArr := delta["tool_calls"].([]any); isArr && len(tcs) > 0 {
		// 已经有标准调用了：这条消息一律不再改写（避免重复执行）。
		// 如果攒着的只是裸的 "Calling tools." 前缀，它不是正文，也不是调用本体，
		// 跟着标准调用一起出门只会重复模型 UI 里那句话；这里直接吞掉。
		g.sawCall = true
		if isBareCallingToolsMarker(g.buf.String()) || isInvisibleToolCallPlaceholder(g.buf.String()) {
			g.head, g.buf = "", strings.Builder{}
			g.holding, g.off = false, true
			return []string{chunk}
		}
		outs := g.flushText()
		g.off = true
		return append(outs, chunk)
	}
	content, _ := delta["content"].(string)
	if content == "" {
		return []string{chunk}
	}
	// 中途认领：只在开头守卫判定完毕（off）之后接管，与守卫攒块/吞尾互斥；
	// 见过标准调用或已认领过一次就不再做——一次调用绝不能跑两遍。
	if g.off && !g.swallow && !g.sawCall && !g.midDone {
		if outs, handled := g.midScan(chunk, obj, delta, content); handled {
			return outs
		}
	}
	// 块已经认领过，只差它最后那句 </tool_call>：吞掉，别漏给客户端。
	if g.swallow {
		merged := g.tailBuf + content
		if i := strings.Index(merged, "</tool_call>"); i >= 0 {
			cut := i + len("</tool_call>")
			rest := merged[cut:]
			g.swallow, g.tailBuf = false, ""
			outs := g.chunkWithoutContent(obj, delta)
			if strings.TrimSpace(rest) != "" {
				outs = append(outs, g.chunkWithText(rest))
			}
			return outs
		}
		if len(merged) > 200 { // 等太久了：不再吞，原样放出去
			g.swallow, g.tailBuf = false, ""
			return append(g.chunkWithoutContent(obj, delta), g.chunkWithText(merged))
		}
		g.tailBuf = merged
		return g.chunkWithoutContent(obj, delta)
	}
	if g.off {
		return []string{chunk}
	}
	if !g.holding {
		if isInvisibleToolCallPlaceholder(g.head + content) {
			g.holding = true
			g.head = ""
			g.buf.WriteString(content)
			outs := g.chunkWithoutContent(obj, delta)
			return append(outs, g.maybeResolve()...)
		}
		switch markerState(g.head + content) {
		case 0:
			g.off = true
			// 判定"开头不是调用"的同一块里，可能带着中段的完整调用块——
			// 出门前必须过一遍中途扫描（2026-10-22）。
			if !g.midDone {
				if outs, handled := g.midScan(chunk, obj, delta, content); handled {
					return outs
				}
			}
			return []string{chunk}
		case 1:
			g.head += content
			g.buf.WriteString(content)
			return g.chunkWithoutContent(obj, delta)
		default:
			g.holding = true
			g.head = ""
			g.buf.WriteString(content)
			outs := g.chunkWithoutContent(obj, delta)
			return append(outs, g.maybeResolve()...)
		}
	}
	g.buf.WriteString(content)
	outs := g.chunkWithoutContent(obj, delta)
	return append(outs, g.maybeResolve()...)
}

// Flush 收尾：没走成调用的文字一个字不少地吐回去。
func (g *textCallGuard) Flush() []string {
	return g.flushText()
}

// ── 中途认领（2026-10-22）─────────────────────────────────────────────────
// 只认「同一个 content 块内完整出现」的 <tool_call>…</tool_call>：前缀正文
// 保留、整块换标准调用；认不出来整块原样保留。跨块的整块不碰（fail-open
// 原样透传）。一条消息只认领一次。

const midToolCallOpen = "<tool_call>"

// midScan 吃一块 content，返回 (输出块, 是否已处理)。
func (g *textCallGuard) midScan(chunk string, obj, delta map[string]any, content string) ([]string, bool) {
	idx := strings.Index(content, midToolCallOpen)
	if idx < 0 {
		return nil, false
	}
	rel := strings.Index(content[idx:], "</tool_call>")
	if rel < 0 {
		return nil, false
	}
	end := idx + rel + len("</tool_call>")
	prefix, block, rest := content[:idx], content[idx:end], content[end:]
	g.midDone = true
	var outs []string
	if prefix != "" {
		c := cloneChunk(obj)
		if chs, ok := c["choices"].([]any); ok && len(chs) > 0 {
			if cc, ok := chs[0].(map[string]any); ok {
				cc["delta"] = map[string]any{"content": prefix}
			}
		}
		outs = append(outs, marshalChunk(c))
	}
	_, calls := salvageText(block, g.specs)
	if len(calls) > 0 {
		g.seq++
		outs = append(outs, g.chunkWithCall(calls[0], g.seq))
	} else {
		// 2026-10-22 probe: a text tool call the salver could not claim —
		// it leaks to the client verbatim ("Calling tools." family). One
		// journald line per occurrence, no body text.
		println("[toolleak] path=mid tag=text_call")
		outs = append(outs, g.chunkWithText(block))
	}
	if strings.TrimSpace(rest) != "" {
		outs = append(outs, g.chunkWithText(rest))
	}
	return outs, true
}

func (g *textCallGuard) flushText() []string {
	if strings.TrimSpace(g.buf.String()) == "" || isInvisibleToolCallPlaceholder(g.buf.String()) {
		g.head, g.buf = "", strings.Builder{}
		g.holding, g.off = false, true
		return nil
	}
	text := g.buf.String()
	g.buf.Reset()
	g.off = true
	return []string{g.chunkWithText(text)}
}

// maybeResolve：块凑齐了就换成标准调用，否则原样攒着。
func (g *textCallGuard) maybeResolve() []string {
	end, ok := blockEnd(g.buf.String())
	if !ok {
		return nil
	}
	held := g.buf.String()
	g.buf.Reset()
	g.off = true
	if !strings.Contains(held[:end], "</tool_call>") {
		// 只认到 </function>（或 </parameter>）：最后那句 </tool_call> 还在路上，
		// 让它也被吞掉，别把半截标记漏给客户端。
		g.swallow = true
	}
	cleaned, calls := salvageText(held, g.specs)
	if len(calls) == 0 || g.sawCall {
		// 认不出来就原样放过（保持现状，不猜）
		if strings.TrimSpace(held) == "" {
			return nil
		}
		return []string{g.chunkWithText(held)}
	}
	var outs []string
	if strings.TrimSpace(cleaned) != "" {
		outs = append(outs, g.chunkWithText(cleaned))
	}
	g.seq++
	outs = append(outs, g.chunkWithCall(calls[0], g.seq))
	return outs
}

// chunkWithoutContent：把这一块里的 content 抠掉（其余字段照原样带走）。
func (g *textCallGuard) chunkWithoutContent(obj, delta map[string]any) []string {
	cp := cloneChunk(obj)
	if chs, ok := cp["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if d, ok := c["delta"].(map[string]any); ok {
				d["content"] = ""
				return []string{marshalChunk(cp)}
			}
		}
	}
	return nil
}

// chunkWithText：造一块只带文字的 chunk（沿用第一条的壳）。
func (g *textCallGuard) chunkWithText(text string) string {
	cp := g.cloneTemplate()
	if chs, ok := cp["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			c["delta"] = map[string]any{"content": text}
			c["finish_reason"] = nil
		}
	}
	return marshalChunk(cp)
}

// chunkWithCall：造一块带标准工具调用的 chunk（OpenAI chat 的流式形状）。
func (g *textCallGuard) chunkWithCall(call map[string]any, seq int) string {
	cp := g.cloneTemplate()
	if chs, ok := cp["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			c["delta"] = map[string]any{"tool_calls": []any{call}}
			c["finish_reason"] = nil
		}
	}
	call["index"] = 0
	call["id"] = callID(seq)
	return marshalChunk(cp)
}

func (g *textCallGuard) cloneTemplate() map[string]any {
	if g.template == nil {
		return map[string]any{"choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{}, "finish_reason": nil}}}
	}
	return cloneChunk(g.template)
}

func callID(seq int) string { return "call_salvage_" + itoa(seq) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func cloneChunk(obj map[string]any) map[string]any {
	blob, err := json.Marshal(obj)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(blob, &out) != nil {
		return map[string]any{}
	}
	return out
}

func marshalChunk(obj map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // < > & 原样走（别把工具调用块转义成 \u003c）
	if err := enc.Encode(obj); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}
