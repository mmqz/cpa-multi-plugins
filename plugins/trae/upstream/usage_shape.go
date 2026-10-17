// usage_shape.go — 用量字段归一化（三家同一口径，2026-10-18 用户定）。
//
// 规则出处与 workbuddy / qoder 的 usage_shape.go 一致（三个插件是独立
// go module，无法共享包）：
//
//	读 reasoning：completion_tokens_details.reasoning_tokens
//	            → output_tokens_details.reasoning_tokens
//	            → 顶层 reasoning_tokens
//	            → completion_tokens_details.reasoning
//	            → 顶层 reasoning_token_count
//	读 cached：  顶层 cache_read_tokens / cacheReadTokens
//	            → prompt_cache_hit_tokens → cached_tokens / cachedTokens
//	            → cache_read_input_tokens
//	            → prompt_tokens_details.cached_tokens
//	            → input_tokens_details.cached_tokens
//	写（老师口径，只写规范位）：reasoning 落 completion_tokens_details
//	    （Trae2api-cn sse.py:640-642）；output_tokens_details 那处是补引擎
//	    8.0.23 的 Responses 转换只查这一处的坑。cached 落
//	    prompt_tokens_details.cached_tokens（input_tokens_details 只在原本存在
//	    时同步）。顶层 reasoning_tokens / cached_tokens 一律不写。
//
// 为什么 Trae 必须归一化：SOLO 上游的 token_usage 事件把 reasoning_tokens
// 平铺在顶层（solosse.go 头部实测样例）。引擎 8.0.23 的 usage 解析只认
// completion_tokens_details / output_tokens_details 两处，不认顶层——
// 原样透传的结果就是账本里 Trae 47 条全是 0。这里把顶层值双写进两处，
// 宿主与直连客户端都能读到真数。
//
// "读不到"与"上游明确给了 0"分开：只有字段真的出现过才双写，不拿 0 冒充。
// 改这里必须同步改 workbuddy / qoder 的同名文件。
package upstream

import (
	"encoding/json"
	"strconv"
	"strings"
)

func usageNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case uint64:
		return int64(n), true
	case uint:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	case string:
		if s := strings.TrimSpace(n); s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return int64(f), true
			}
		}
	}
	return 0, false
}

func usageSubObject(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func usageField(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	return usageNumber(v)
}

// cloneUsageSub copies one nested usage object — NormalizeUsageForHost is a
// shallow copy otherwise, and mirroring would mutate the caller's map.
func cloneUsageSub(m map[string]any, key string) map[string]any {
	sub := usageSubObject(m, key)
	out := make(map[string]any, len(sub)+1)
	for k, v := range sub {
		out[k] = v
	}
	return out
}

func readUsageReasoning(m map[string]any) (int64, bool) {
	if m == nil {
		return 0, false
	}
	if sub := usageSubObject(m, "completion_tokens_details"); sub != nil {
		if v, ok := usageField(sub, "reasoning_tokens"); ok {
			return v, true
		}
	}
	if sub := usageSubObject(m, "output_tokens_details"); sub != nil {
		if v, ok := usageField(sub, "reasoning_tokens"); ok {
			return v, true
		}
	}
	if v, ok := usageField(m, "reasoning_tokens"); ok {
		return v, true
	}
	if sub := usageSubObject(m, "completion_tokens_details"); sub != nil {
		if v, ok := usageField(sub, "reasoning"); ok {
			return v, true
		}
	}
	// WorkBuddy 上游实测还会给 completion_thinking_tokens，三份规则一起兜底。
	if v, ok := usageField(m, "completion_thinking_tokens"); ok {
		return v, true
	}
	return usageField(m, "reasoning_token_count")
}

func readUsageCached(m map[string]any) (int64, bool) {
	if m == nil {
		return 0, false
	}
	// 老师的读取顺序（Trae2api-cn sse.py:631-637 / qoder2api-hub qoder_proxy.py:473-474）：
	// Trae 实际上报 cache_read_tokens / cacheReadTokens，Qoder 用 prompt_cache_hit_tokens，
	// Anthropic 风格是 cache_read_input_tokens；details 两处放最后兜底。
	for _, k := range []string{"cache_read_tokens", "cacheReadTokens",
		"prompt_cache_hit_tokens", "cached_tokens", "cachedTokens",
		"cache_read_input_tokens"} {
		if v, ok := usageField(m, k); ok {
			return v, true
		}
	}
	if sub := usageSubObject(m, "prompt_tokens_details"); sub != nil {
		if v, ok := usageField(sub, "cached_tokens"); ok {
			return v, true
		}
	}
	if sub := usageSubObject(m, "input_tokens_details"); sub != nil {
		if v, ok := usageField(sub, "cached_tokens"); ok {
			return v, true
		}
	}
	return 0, false
}

// NormalizeUsageForHost mirrors reasoning / cached detail into every shape the
// host and direct clients read. Returns nil for nil input; a copy otherwise
// (the caller's map is never mutated).
func NormalizeUsageForHost(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	out := make(map[string]any, len(raw)+6)
	for k, v := range raw {
		out[k] = v
	}
	if rea, ok := readUsageReasoning(raw); ok {
		// 老师口径：reasoning 只写 chat 规范位 completion_tokens_details
		// （Trae2api-cn sse.py:640-642），不写顶层。
		// output_tokens_details 是补引擎 8.0.23 的 Responses 转换只查这一处。
		ct := cloneUsageSub(out, "completion_tokens_details")
		ct["reasoning_tokens"] = rea
		out["completion_tokens_details"] = ct
		ot := cloneUsageSub(out, "output_tokens_details")
		ot["reasoning_tokens"] = rea
		out["output_tokens_details"] = ot
	}
	if cached, ok := readUsageCached(raw); ok {
		// 老师口径：cached 写 prompt_tokens_details.cached_tokens（sse.py:639），不写顶层。
		pd := cloneUsageSub(out, "prompt_tokens_details")
		pd["cached_tokens"] = cached
		out["prompt_tokens_details"] = pd
		// input_tokens_details 只在原本就存在时同步，避免宿主求和时双计。
		if usageSubObject(raw, "input_tokens_details") != nil {
			id := cloneUsageSub(out, "input_tokens_details")
			id["cached_tokens"] = cached
			out["input_tokens_details"] = id
		}
	}
	// total 只在字段缺失时补；明确给了 0 就保留 0（老师从不覆盖上游的数）。
	if _, ok := usageField(out, "total_tokens"); !ok {
		in, inOK := usageField(out, "prompt_tokens")
		if !inOK {
			in, _ = usageField(out, "input_tokens")
		}
		outTok, outOK := usageField(out, "completion_tokens")
		if !outOK {
			outTok, _ = usageField(out, "output_tokens")
		}
		if in+outTok > 0 {
			out["total_tokens"] = in + outTok
		}
	}
	// 搬走不复制（老师口径，sse.py:640-642 的 reasoning 是搬走而非复制）：
	// 被搬进 details 的顶层别名删掉，避免"顶层 + details"相加的消费方双计。
	for _, k := range []string{
		"reasoning_tokens", "reasoningTokens", "reasoning_token_count",
		"cached_tokens", "cachedTokens", "cache_read_tokens", "cacheReadTokens",
		"cache_read_input_tokens", "prompt_cache_hit_tokens",
	} {
		delete(out, k)
	}
	return out
}
