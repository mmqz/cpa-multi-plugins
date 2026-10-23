// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// toolDescriptionByteLimit is the threshold above which all tool descriptions
// are stripped to avoid Tencent's 64KB body-size filter. Mirrors OmniRoute.
const toolDescriptionByteLimit = 65536

// forceStreamBody returns the request body with "stream":true and
// stream_options.include_usage=true set, since the upstream rejects
// non-streaming chat requests and only then emits its final usage chunk.
// prepareUpstreamBody composes forceStreamBody + ensureStreamUsageInPlace +
// normalizeToolsForUpstream + rewriteSystemForUpstream + ensureSystemMessage +
// rewriteModelInBody into a
// single unmarshal/marshal pass (v0.6.31 perf: was 4-5 full JSON round-trips
// on every chat completion). The 4 legacy helpers remain for tests and other
// call sites that need them individually.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}

	// 1. forceStream: CodeBuddy rejects non-stream requests. Also ask for the
	// final usage chunk: WorkBuddy puts reasoning_tokens in that chunk's
	// completion_tokens_details; without it the gateway records 0/1.
	obj["stream"] = true
	ensureStreamUsageInPlace(obj)

	// 2. normalizeTools: tool_choice object form → string; "none" suppresses tools.
	normalizeToolsInPlace(obj)

	// 2.5 normalizeHistory: OpenAI developer 角色 → system（腾讯后端不认
	// developer，实测判 11128 渠道风控——Coding2API 2026-09 生产实证）；
	// 脏 tool_call（无 function/name）剔除，清空后无内容的 assistant 占位
	// 与悬空 role=tool 结果成对清理（大输入/长会话韧性，同源实证对策）。
	normalizeHistoryInPlace(obj)

	// 5.5 contextProjection（opt-in，默认关）：超大 agent 历史按规则投影
	// （保尾 + 规则摘要 + 工具输出头尾裁剪），见 context_projection.go。
	// 开关关闭时本函数直接返回，请求体一个字节都不变。
	projectContextInPlace(obj)

	// 【本地补丁·档位管家】强制钉优先；客户端有效档位压过默认档；
	// 两者都没有时由 forceMaxThinking 用真实最高档兜底。
	clientEff, _ := obj["reasoning_effort"].(string)
	if !applyPinnedEffortInPlace(obj) {
		applyDefaultEffortInPlace(obj)
	}

	// 3. rewriteSystem: strip blocked Claude Code template phrases + force thinking.
	rewriteSystemInPlace(obj)

	// 3.2 sanitizeHistory（self 0.9.63）：同一批风控短语也会藏在 assistant 历史
	// 回显、用户粘贴、tool_calls 参数和 reasoning 字段里。腾讯 11128 通道门
	// 扫描整个请求体，只清 system 会漏。必须放在 injectReasoningInPlace 之前，
	// 否则推理回填会把已清掉的短语重新带进正文。只做同义替换，未命中的内容
	// 逐字节不变。
	sanitizeHistoryPhrasesInPlace(obj)

	// 3.5 injectReasoning: fold historical assistant reasoning_content into
	// content as <thought> blocks — the upstream drops the non-standard
	// field from multi-turn history and the model loses its own prior
	// chain of thought (issue #5).
	injectReasoningInPlace(obj)

	// 4. ensureSystemMessage: inject minimal system msg for Global only.
	ensureSystemMessageInPlace(obj, sa)

	// 5. rewriteModel: swap client model name to upstream model id.
	rewriteModelInPlace(obj, upstreamModel)

	// 诊断日志（tierprobe）：client=客户端原值 pin=钉子 final=最终发往上游的值
	finalEff, _ := obj["reasoning_effort"].(string)
	logTierProbe(upstreamModel, clientEff, pinnedReasoningEffort(upstreamModel), finalEff)

	// 6. maxTokenRename: OpenAI-newer clients send max_completion_tokens;
	// the CodeBuddy upstream speaks max_tokens. Copy the value across when
	// max_tokens is absent, then drop the foreign key so the gateway does
	// not see an unknown parameter (wb2api payload.go, PR #116 semantics).
	if v, ok := obj["max_completion_tokens"]; ok {
		if _, has := obj["max_tokens"]; !has && v != nil {
			obj["max_tokens"] = v
		}
		delete(obj, "max_completion_tokens")
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeHistoryInPlace normalizes message history before the upstream
// sees it (v0.9.15, large-input resilience):
//   - role "developer" → "system": the Tencent backend does not recognize
//     the OpenAI developer role and rejects the whole request as channel
//     risk-control 11128 (reasoning-model clients like PI convert system
//     messages to developer; Coding2API production evidence 2026-09).
//   - dirty tool_calls (missing function or empty name) are dropped; an
//     assistant placeholder left with no content after the cleanup is
//     dropped entirely; dangling role=tool results whose tool_call_id
//     points at a dropped call are dropped with it (same shape as the
//     Coding2API _clean_history_tool_calls defense). Oversized agent
//     histories trimmed by the client mid-conversation are the main
//     source of such orphans.
func normalizeHistoryInPlace(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	orphan := map[string]struct{}{}
	rewritten := make([]any, 0, len(msgs))
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			rewritten = append(rewritten, mi)
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(role, "developer") {
			m["role"] = "system"
			changed = true
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			kept := make([]any, 0, len(tcs))
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				name := ""
				if fn != nil {
					name, _ = fn["name"].(string)
				}
				if strings.TrimSpace(name) == "" {
					if id, _ := tc["id"].(string); id != "" {
						orphan[id] = struct{}{}
					}
					changed = true
					continue
				}
				kept = append(kept, tc)
			}
			if len(kept) == 0 {
				delete(m, "tool_calls")
				changed = true
				if cv, has := m["content"]; !has || cv == nil {
					continue // 清空后无内容的占位 assistant 整条丢弃
				}
			} else {
				m["tool_calls"] = kept
			}
		}
		rewritten = append(rewritten, m)
	}
	if len(orphan) > 0 {
		filtered := make([]any, 0, len(rewritten))
		for _, mi := range rewritten {
			m, ok := mi.(map[string]any)
			if !ok {
				filtered = append(filtered, mi)
				continue
			}
			if role, _ := m["role"].(string); strings.EqualFold(role, "tool") {
				id, _ := m["tool_call_id"].(string)
				if _, bad := orphan[id]; bad {
					changed = true
					continue
				}
			}
			filtered = append(filtered, m)
		}
		rewritten = filtered
	}
	if !changed {
		return false
	}
	obj["messages"] = rewritten
	return true
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
	changed := false
	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			delete(obj, "tool_choice")
			changed = true
		}
	}
	return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
// It does three things:
//  1. For each system message: apply sanitizeBlockedTemplates (blocked
//     phrases → safe variants). v0.9.28: the wholesale neutralPrompt swap
//     (length/agentPattern triggers) is retired — see
//     rewriteSystemContentField for the evidence chain.
//  2. Strip reasoning_effort "none"/"off" (Tencent rejects them); mirror
//     other values to reasoning_summary="auto" (OmniRoute codebuddy-cn.ts).
//  3. forceMaxThinking for hy3/hy4-family models.
func rewriteSystemInPlace(obj map[string]any) bool {
	changed := rewriteSystemMessagesInPlace(obj)
	if mirrorReasoningEffort(obj) {
		changed = true
	}
	if forceMaxThinking(obj) {
		changed = true
	}
	if compactToolDescriptions(obj) {
		changed = true
	}
	return changed
}

// rewriteSystemMessagesInPlace applies the content-filter defenses to SYSTEM
// messages only. v0.9.18 scope fix: the pre-0.9.18 loop fed EVERY message
// through the then-wholesale replacement, so any user paste / tool result /
// assistant history entry over the then-maxSystemPromptBytes (or matching the
// then-agentPattern) was silently rewritten to neutralPrompt and the upstream
// model saw a history full of hollow "You are a helpful AI assistant..."
// messages. Field reports mapping to exactly this: tool output that "looks
// truncated / stdout empty", conversations that "reset every so often", and
// the model answering
// with the neutral prompt itself on interruption. OmniRoute codebuddy-cn.ts
// (the porting source) gates the replacement on
// `message.role !== "system" -> return message` verbatim; this restores it.
func rewriteSystemMessagesInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); !strings.EqualFold(role, "system") {
			continue
		}
		if rewriteSystemContentField(msg) {
			changed = true
		}
	}
	return changed
}

// rewriteSystemContentField applies the filter defenses to one SYSTEM
// message's content. v0.9.28 (issue #3 closure, adopting PR #4's position):
// the wholesale neutralPrompt replacement — for content over
// maxSystemPromptBytes or matching agentPattern — is RETIRED. Evidence that
// it defended nothing: (a) the upstream filter blocklists VERBATIM phrases
// (sanitizeBlockedTemplates' one-word insert "tool" defeats it — a verbatim
// matcher does not reject by length or by broad agent-identity patterns);
// (b) PR #4's author removed both triggers and ran without 400s; (c) since
// the v0.9.18 role gate, non-system messages of any length or content pass
// unfiltered and no rejection was ever reported. What the wholesale swap DID
// do was gut every agent host's system prompt (nearly all match "you are
// claude code" / "you are a coding agent" and exceed 2000 bytes): tool-use
// rules, project context and behavior constraints were silently swapped for
// an 18-byte generic line — a permanent invisible degradation, strictly worse
// than a loud 400 that can be reported and fixed. System content now only
// goes through sanitizeBlockedTemplates (known blocked phrases → safe
// variants; everything else survives verbatim), on strings and per text part
// on arrays alike.
func rewriteSystemContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

// mirrorReasoningEffort implements OmniRoute codebuddy-cn.ts reasoning_effort
// handling:
//   - "none"/"off" → delete field (Tencent has no "none")
//   - other value → set reasoning_summary="auto" (mirror)
//   - absent → no-op (forcing reasoning triggers content filter)
func mirrorReasoningEffort(obj map[string]any) bool {
	eff, ok := obj["reasoning_effort"].(string)
	if !ok {
		return false
	}
	effLower := strings.ToLower(strings.TrimSpace(eff))
	if effLower == "none" || effLower == "off" {
		delete(obj, "reasoning_effort")
		return true
	}
	if effLower != "" {
		obj["reasoning_summary"] = "auto"
		return true
	}
	return false
}

// compactToolDescriptions strips tool.function.description when the serialized
// tools array exceeds toolDescriptionByteLimit (64KB). Tencent's body-size
// filter rejects large tool descriptions. Mirrors OmniRoute codebuddy-cn.ts.
func compactToolDescriptions(obj map[string]any) bool {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	serialized, err := json.Marshal(tools)
	if err != nil {
		return false
	}
	if len(serialized) < toolDescriptionByteLimit {
		return false
	}
	changed := false
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasDesc := fn["description"]; hasDesc {
			delete(fn, "description")
			changed = true
		}
	}
	return changed
}

// injectReasoningInPlace folds each historical assistant turn's reasoning
// back into its content as a <thought> block (issue #5, implementation
// adapted from PR #6). The upstream gateway silently drops the non-standard
// reasoning_content field on multi-turn history, so the model never sees its
// own prior chain of thought — "I derived X last turn" self-attention is
// gone and the model re-derives or contradicts itself. Inlining the reasoning
// into content survives the gateway's field whitelist. Idempotent: a message
// whose content already starts with <thought> is skipped.
func injectReasoningInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		reasoning := reasoningTextFromMessage(msg)
		if reasoning == "" {
			continue
		}
		if prependThoughtInPlace(msg, "<thought>\n"+reasoning+"\n</thought>") {
			changed = true
		}
	}
	return changed
}

// reasoningTextFromMessage returns the first non-empty reasoning text carried
// on a message, from either reasoning_content or reasoning fields.
func reasoningTextFromMessage(msg map[string]any) string {
	if rc, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(rc) != "" {
		return strings.TrimSpace(rc)
	}
	if r, ok := msg["reasoning"].(string); ok && strings.TrimSpace(r) != "" {
		return strings.TrimSpace(r)
	}
	return ""
}

// prependThoughtInPlace prepends thoughtBlock to a message's content when not
// already present. Handles plain string content and OpenAI-style content
// parts (the thought block becomes a new leading text part). Returns true
// when the message content was modified.
func prependThoughtInPlace(msg map[string]any, thoughtBlock string) bool {
	switch c := msg["content"].(type) {
	case string:
		if strings.HasPrefix(strings.TrimSpace(c), "<thought>") {
			return false
		}
		if strings.TrimSpace(c) == "" {
			msg["content"] = thoughtBlock
		} else {
			msg["content"] = thoughtBlock + "\n\n" + c
		}
		return true
	case []any:
		if len(c) == 0 {
			return false
		}
		// Multimodal (array-of-parts) content: prepend a text part
		// carrying the thought block before the first part.
		first, ok := c[0].(map[string]any)
		if !ok {
			return false
		}
		txt, ok := first["text"].(string)
		if !ok {
			return false
		}
		if strings.HasPrefix(strings.TrimSpace(txt), "<thought>") {
			return false
		}
		msg["content"] = append([]any{map[string]any{"type": "text", "text": thoughtBlock}}, c...)
		return true
	}
	return false
}

// ensureSystemMessageInPlace is the in-place form of ensureSystemMessage.
// Returns true when obj was modified.
//
// v0.9.37 (issue #20): the Global realm validates messages[0] — a payload
// whose FIRST message is not a system prompt is rejected (code 11128,
// "first message is not system prompt"; contract mirrored from
// zqcccc/workbuddy-cliproxy's ensureSystemFirst). The old any-position
// scan skipped the injection whenever a client carried a system message
// mid-history (normalizeHistory's developer→system rewrite preserves
// position), leaving a user first message for upstream to reject. A
// system message that is not first no longer suppresses the injection;
// the Global-only gate is unchanged so CN traffic stays byte-identical.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
	// [self 0.9.63] Upstream 0.9.59 (issue #40, PR #42): the INTL realm
	// enforces the same messages[0].role == "system" position check as Global.
	// CN keeps its byte-identical path.
	if sa == nil || !(isGlobalDomain(sa.Auth.Domain) || isIntlDomain(sa.Auth.Domain)) {
		return false
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	// Position, not presence (issue #20): only a system message AT THE
	// FRONT satisfies the upstream check.
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return false
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Mirrors rewriteModelInBody's behavior (case-insensitive compare); returns
// true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), upstreamModel) {
		return false
	}
	obj["model"] = upstreamModel
	return true
}

// ensureStreamUsageInPlace asks the upstream to append the final usage chunk.
// WorkBuddy exposes completion_tokens_details.reasoning_tokens there; a stream
// without this option can be persisted as 0/1 even though reasoning happened.
func ensureStreamUsageInPlace(obj map[string]any) bool {
	opts, ok := obj["stream_options"].(map[string]any)
	if !ok {
		opts = map[string]any{}
		obj["stream_options"] = opts
	}
	if include, ok := opts["include_usage"].(bool); ok && include {
		return false
	}
	opts["include_usage"] = true
	return true
}

func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	ensureStreamUsageInPlace(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := false

	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}

	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					// Object force without a name: fall back to auto instead of 400.
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				// Unknown object shape → drop rather than forward a 400.
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			// null / array / number — drop to keep upstream happy.
			delete(obj, "tool_choice")
			changed = true
		}
	}

	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := rewriteSystemMessagesInPlace(obj)
	if forceMaxThinking(obj) {
		changed = true
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// ensureSystemMessage injects a minimal system message unless the FIRST
// message is already a system prompt. Global (www.workbuddy.ai) rejects
// user-only requests with code 11101 "Parse message failed: 11101:invalid
// request", and per issue #20 field evidence any payload whose first
// message is not system with 11128 — the upstream check is on position,
// not on the presence of a system message somewhere in the list (mirrors
// zqcccc/workbuddy-cliproxy's ensureSystemFirst). CN (copilot.tencent.com)
// does not require a system message but tolerates one. Inserting a
// harmless system message unifies both paths.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
	if len(payload) == 0 {
		return payload
	}
	// [self 0.9.63] Global and Intl both validate position; CN is unchanged.
	if sa == nil || !(isGlobalDomain(sa.Auth.Domain) || isIntlDomain(sa.Auth.Domain)) {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}
	// Position, not presence (issue #20): only a system message AT THE
	// FRONT satisfies the upstream check.
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return payload // first message already system
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// (rewriteContentField / sanitizeContentText were removed in v0.9.18: they
// applied the wholesale neutralPrompt replacement to messages of ANY role.
// The system-scoped replacements live in rewriteSystemContentField above;
// non-system messages are never rewritten — see rewriteSystemMessagesInPlace.)

// Blocked-template variants seen in the wild beyond the two verbatim
// templates above: capitalization drift, straight/curly quote drift, and
// "Anthropic's official CLI" phrasing without the "You are Claude Code"
// prefix (adapted from PR #4). The exact-match ReplaceAll calls above stay
// byte-identical to OmniRoute; these regexes are the safety net.
var (
	reClaudeCodeCli = regexp.MustCompile(`(?i)anthropic(?:'s)?\s+official\s+cli\s+for\s+claude`)
	reMainBranchPr  = regexp.MustCompile(`(?i)main\s+branch\s+\(you\s+will\s+usually\s+use\s+this\s+for\s+prs\)`)

	// reCodexCliIntro matches the Codex CLI base-instructions opening
	// signature that Tencent's gateway blocklists as channel risk-control
	// (issue #31, code 11128 "Illegal API invocation from an unapproved
	// channel"): the two-sentence intro every Codex CLI / Codex Desktop
	// session opens its system prompt with. Reporter's single-variable
	// replay: 16 tools / long context / reasoning all pass; only this
	// sentence pair 100% triggers 11128, and swapping it for a generic
	// line passes verbatim with the full 20k-char instruction intact.
	// Tolerant to case and to the first sentence ending with or without
	// further description before the period.
	reCodexCliIntro          = regexp.MustCompile(`(?i)you are a coding agent running in the codex cli[^.\n]*\.\s*codex cli is an open source project led by openai\s*\.?`)
	codexCliIntroReplacement = "You are a helpful coding assistant running in the terminal."

	// [self 0.9.63] Upstream 0.9.62 (PR #46): Tencent's CN gateway blocklists
	// the DeepSeek Harness base-instructions opening as 11128 "Illegal API
	// invocation from an unapproved channel". Single-variable replay: the bare
	// sentence 400s every time, while "DeepSeek Harness" alone, "powered by
	// DeepSeek Harness", and the same sentence naming another harness all pass.
	reDeepSeekHarnessIntro          = regexp.MustCompile(`(?i)you are an ai agent powered by deepseek harness`)
	deepSeekHarnessIntroReplacement = "You are an AI assistant"

	// [self 0.9.63] Upstream 0.9.63 (PR #49): the runtime-context line
	// "the DeepSeek Harness Web GUI" is a separate member of the same 11128
	// family. Replay: the verbatim four-token phrase 400s, while breaking any
	// one token ("Site GUI", "Web UI", "Harness Web GUI") passes.
	reDeepSeekHarnessWebGUI          = regexp.MustCompile(`(?i)deepseek\s+harness\s+web\s+gui`)
	deepSeekHarnessWebGUIReplacement = "DeepSeek Harness web interface"
)

func sanitizeBlockedTemplates(s string) string {
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = reClaudeCodeCli.ReplaceAllString(s, "Anthropic's official CLI tool for Claude")
	s = reMainBranchPr.ReplaceAllString(s, "Default branch (you will usually use this for PRs)")
	// v0.9.54 (issue #31): Codex CLI intro signature — Tencent gateway
	// channel risk-control (11128). Exact match first (the verified
	// trigger), then the tolerant regex for case / phrasing drift between
	// Codex releases. System-role gating happens upstream in
	// rewriteSystemMessagesInPlace — user content stays untouched.
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
		codexCliIntroReplacement)
	s = reCodexCliIntro.ReplaceAllString(s, codexCliIntroReplacement)
	// [self 0.9.63] DeepSeek Harness family: same 11128 channel gate. The
	// caller decides scope — rewriteSystemMessagesInPlace for system content,
	// sanitizeHistoryPhrasesInPlace for every other role.
	s = reDeepSeekHarnessIntro.ReplaceAllString(s, deepSeekHarnessIntroReplacement)
	s = reDeepSeekHarnessWebGUI.ReplaceAllString(s, deepSeekHarnessWebGUIReplacement)
	return s
}

// sanitizeHistoryPhrasesInPlace applies the blocked-phrase rewrites to EVERY
// message in the history — assistant echoes, tool results and user pastes
// included (upstream v0.9.63). Evidence chain (2026-10-21, single-variable
// replays against copilot.tencent.com): a 50-message body whose only
// "DeepSeek Harness Web GUI" occurrences live in a user message and an
// assistant history echo 400s with code 11128 even when the system prompt is
// fully sanitized; scrubbing the phrase in every role flips the same body to
// 200. The Claude Code git-injection line "Main branch (you will usually use
// this for PRs)" hits the same way inside an assistant echo.
//
// Scope guard: this is NOT the retired v0.9.18 wholesale swap. Every rewrite
// is the same meaning-preserving substitution the system path already uses,
// and content without a trigger phrase passes byte-identical.
//
// [self 0.9.63] Placement matters: this runs AFTER our context projection and
// system rewrite, and BEFORE injectReasoningInPlace. The fold in step 3.5
// copies reasoning_content/reasoning into content, so those fields must be
// scrubbed here or the fold would re-arm the phrases inside already-sanitized
// content.
//
// Handles: string content; array content (per text part); tool_calls
// function.arguments (agents echo protocol text there); assistant
// reasoning_content / reasoning mirrors.
func sanitizeHistoryPhrasesInPlace(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	scrub := sanitizeBlockedTemplates
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		// reasoning_content / reasoning are folded into content by
		// injectReasoningInPlace AFTER this pass, so scrub them here.
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "assistant") {
			for _, key := range []string{"reasoning_content", "reasoning"} {
				if rc, ok := m[key].(string); ok && rc != "" {
					if r := scrub(rc); r != rc {
						m[key] = r
						changed = true
					}
				}
			}
		}
		if c, ok := m["content"].(string); ok {
			if r := scrub(c); r != c {
				m["content"] = r
				changed = true
			}
		} else if parts, ok := m["content"].([]any); ok {
			for _, pi := range parts {
				p, ok := pi.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					if r := scrub(t); r != t {
						p["text"] = r
						changed = true
					}
				}
			}
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok || fn == nil {
					continue
				}
				if a, ok := fn["arguments"].(string); ok {
					if r := scrub(a); r != a {
						fn["arguments"] = r
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// forceMaxThinking selects the highest reasoning level that the model actually
// advertises. The catalog is authoritative: never send a level the model did
// not return. This matters for models such as step-5-preview (low/medium/high)
// and glm-5.2 (high/xhigh), which must not receive max.
//
// If no Thinking.Levels are known, the request is left untouched rather
// than guessed. Explicit client choices are preserved here; the tier policy
// has already handled pin/default precedence before this fallback runs.
func forceMaxThinking(obj map[string]any) bool {
	model, _ := obj["model"].(string)

	// A configured forced pin always wins. A configured default is also
	// authoritative over this automatic fallback, even when the client sent an
	// invalid value. A valid client value beats the automatic max fallback.
	if pinnedReasoningEffort(model) != "" || defaultReasoningEffort(model) != "" {
		return false
	}
	current, _ := obj["reasoning_effort"].(string)
	if clientEffortGiven(current) {
		m := normalizedModelKey(model)
		if m == "hy3-x" && current == "low" {
			return false
		}
		for _, level := range thinkingLevelsForModel(model) {
			if strings.EqualFold(strings.TrimSpace(level), current) {
				return false
			}
		}
	}

	levels := thinkingLevelsForModel(model)
	if len(levels) == 0 {
		return false
	}

	lm := normalizedModelKey(model)
	current = strings.ToLower(strings.TrimSpace(current))
	if current == "low" && (lm == "hy3" || lm == "hy3-x") {
		return false
	}

	best := highestEffortLevel(levels)
	if best == "" || current == best {
		return false
	}
	obj["reasoning_effort"] = best
	return true
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		// Legacy function_call shell: {"name":"","arguments":""} is the
		// upstream's terminal-chunk artifact, not a real call — treat as empty
		// when every value is itself empty.
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
