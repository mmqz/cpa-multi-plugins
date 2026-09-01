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

// neutralPrompt is the substitute for over-long or agent-identity system
// prompts that Tencent CodeBuddy's content filter rejects. Mirrors OmniRoute's
// codebuddy-cn.ts NEUTRAL_PROMPT.
const neutralPrompt = "You are a helpful AI assistant that helps with software engineering tasks."

// agentPattern matches Claude Code / Cursor / Windsurf / Cline / Aider / Continue
// / Copilot / Cody identity lines that Tencent's content filter blocklists.
// Ported verbatim from OmniRoute/open-sse/executors/codebuddy-cn.ts (MIT).
var agentPattern = regexp.MustCompile(`(?i)you are claude code|claude.?code.+official.+cli|anthropic.+official.+cli|anxthxropic.+official.+cli|you are (?:cursor|windsurf|cline|aider|continue|copilot|cody)|you are an? (?:ai )?(?:coding |code )?agent|cc_entrypoint\s*=\s*(?:cli|vscode|jetbrains|gui)|claude.?code.+issues|give feedback.+claude.?code|you are .{0,30}(?:powerful )?ai agent|orchestration capabilities|OhMyOpenCode|<agent-identity>|<Role>|<Behavior_Instructions>`)

// maxSystemPromptBytes is the byte-length threshold above which a system prompt
// is replaced wholesale with neutralPrompt. Tencent's filter rejects very long
// system prompts even when they don't match agentPattern. Mirrors OmniRoute.
const maxSystemPromptBytes = 2000

// toolDescriptionByteLimit is the threshold above which all tool descriptions
// are stripped to avoid Tencent's 64KB body-size filter. Mirrors OmniRoute.
const toolDescriptionByteLimit = 65536

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
// prepareUpstreamBody composes forceStreamBody + normalizeToolsForUpstream +
// rewriteSystemForUpstream + ensureSystemMessage + rewriteModelInBody into a
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

        // 1. forceStream: CodeBuddy rejects non-stream requests.
        obj["stream"] = true

        // 2. normalizeTools: tool_choice object form → string; "none" suppresses tools.
        normalizeToolsInPlace(obj)

        // 3. rewriteSystem: strip blocked Claude Code template phrases + force thinking.
        rewriteSystemInPlace(obj)

        // 4. ensureSystemMessage: inject minimal system msg for Global only.
        ensureSystemMessageInPlace(obj, sa)

        // 5. rewriteModel: swap client model name to upstream model id.
        rewriteModelInPlace(obj, upstreamModel)

        out, err := json.Marshal(obj)
        if err != nil {
                return src
        }
        return out
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
//  1. For each system message: if length > maxSystemPromptBytes or matches
//     agentPattern, replace content wholesale with neutralPrompt. Otherwise,
//     apply sanitizeBlockedTemplates (single-word substitutions).
//  2. Strip reasoning_effort "none"/"off" (Tencent rejects them); mirror
//     other values to reasoning_summary="auto" (OmniRoute codebuddy-cn.ts).
//  3. forceMaxThinking for hy3/hy4-family models.
func rewriteSystemInPlace(obj map[string]any) bool {
        messages, _ := obj["messages"].([]any)
        changed := false
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if rewriteContentField(msg) {
                        changed = true
                }
        }
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

// ensureSystemMessageInPlace is the in-place form of ensureSystemMessage.
// Returns true when obj was modified.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
        if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
                return false
        }
        messages, ok := obj["messages"].([]any)
        if !ok || len(messages) == 0 {
                return false
        }
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
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
        messages, _ := obj["messages"].([]any)
        changed := false
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if rewriteContentField(msg) {
                        changed = true
                }
        }
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

// ensureSystemMessage injects a minimal system message if none is present.
// Global (www.workbuddy.ai) rejects user-only requests with code 11101
// "Parse message failed: 11101:invalid request". CN (copilot.tencent.com)
// does not require a system message but tolerates one. Inserting a
// harmless system message unifies both paths.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
        if len(payload) == 0 {
                return payload
        }
        // Only inject for Global; CN doesn't need it and we minimize diff.
        if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
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
        for _, m := range messages {
                msg, ok := m.(map[string]any)
                if !ok {
                        continue
                }
                if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
                        return payload // already has system message
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

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
//
// Per OmniRoute codebuddy-cn.ts:
//   - If content length > maxSystemPromptBytes (2000) OR matches agentPattern,
//     replace content wholesale with neutralPrompt.
//   - Otherwise, apply sanitizeBlockedTemplates (single-word substitutions).
//
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
        switch c := msg["content"].(type) {
        case string:
                if r := sanitizeContentText(c); r != c {
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
                                if r := sanitizeContentText(t); r != t {
                                        part["text"] = r
                                        modified = true
                                }
                        }
                }
                return modified
        }
        return false
}

// sanitizeContentText decides between wholesale replacement (neutralPrompt)
// and template-level single-word substitution. Mirrors OmniRoute codebuddy-cn.ts
// AGENT_PATTERN + length check.
func sanitizeContentText(text string) string {
        if len(text) > maxSystemPromptBytes || agentPattern.MatchString(text) {
                return neutralPrompt
        }
        return sanitizeBlockedTemplates(text)
}

func sanitizeBlockedTemplates(s string) string {
        s = strings.ReplaceAll(s,
                "You are Claude Code, Anthropic's official CLI for Claude.",
                "You are Claude Code, Anthropic's official CLI tool for Claude.")
        s = strings.ReplaceAll(s,
                "Main branch (you will usually use this for PRs)",
                "Default branch (you will usually use this for PRs)")
        return s
}

// forceMaxThinking pins reasoning_effort to "high" for hy3/hy4-family models
// (Tencent Hunyuan) so they always reason at maximum depth. CodeBuddy only
// honors "high" for deep thinking (medium/low/max/xhigh/ultra all fall back to
// no reasoning), so we override whatever the client sent. Matching is
// case-insensitive because this runs before rewriteModelInPlace swaps the
// client-facing model name for the upstream ID. Returns true if changed.
func forceMaxThinking(obj map[string]any) bool {
        model, _ := obj["model"].(string)
        lm := strings.ToLower(model)
        if !strings.HasPrefix(lm, "hy3") && !strings.HasPrefix(lm, "hy4") {
                return false
        }
        if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
                return false
        }
        obj["reasoning_effort"] = "high"
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
