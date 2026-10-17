// usage_shape.go — 用量字段归一化（三家同一口径，2026-10-18 用户定）。
//
// 为什么要有这一层：上游各网关把 reasoning / cache 明细放在不同位置
// （completion_tokens_details / output_tokens_details / 顶层平铺），引擎各版本
// 的 usage 解析器也只认其中一到两个位置（8.0.23 的非流式 /v1/responses 只查
// output_tokens_details）。插件是唯一同时看得到"上游原始写法"与"宿主解析口径"
// 的一层，所以在这里做一次 读全 + 双写：
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
//	写（老师口径，只写规范位、不写顶层）：reasoning 落
//	    completion_tokens_details.reasoning_tokens；output_tokens_details 那处
//	    是补引擎 8.0.23 的 Responses 转换只查这一处的坑。cached 落
//	    prompt_tokens_details.cached_tokens（input_tokens_details 只在原本存在
//	    时同步，避免同一笔缓存被两处相加）。顶层 reasoning_tokens/cached_tokens
//	    一律不写——老师从不写，写了遇到"顶层与 details 相加"的消费方会双计。
//
// "读不到"与"上游明确给了 0"必须分开：只有字段真的出现过（present）才双写，
// 不拿 0 冒充"读过"——页面上的 0 要能说清是上游回的 0，不是我们编的。
//
// 规则只写这一份（本插件内）。trae、qoder 插件是各自独立的 go module，无法
// 共享本文件，各有一份同名同规则的实现；改这里必须同步改另两份。
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// usageNumber coerces one usage scalar (number or numeric string) to int64.
// Encoding-agnostic on purpose: upstreams have been seen flipping between
// JSON numbers, json.Number and quoted strings.
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

// usageSubObject returns a nested usage object (nil when absent/not an object).
func usageSubObject(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

// usageField reads one scalar field, reporting whether it was present at all.
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

// cloneUsageSub returns a copy of a nested usage object so mirroring detail
// fields never mutates the caller's map (normalizeUsageMap is shallow-copy
// otherwise: nested objects would still be shared).
func cloneUsageSub(m map[string]any, key string) map[string]any {
	sub := usageSubObject(m, key)
	out := make(map[string]any, len(sub)+1)
	for k, v := range sub {
		out[k] = v
	}
	return out
}

// readUsageReasoning walks the five-field fallback chain (order matches the
// mature reference proxies' live-verified probes). present=false means the
// upstream did not report reasoning at all — the caller must not write a 0.
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
	// WorkBuddy 上游实测还会给 completion_thinking_tokens（老师日志里与
	// completion_tokens_details.reasoning_tokens 同值），一并兜底。
	if v, ok := usageField(m, "completion_thinking_tokens"); ok {
		return v, true
	}
	return usageField(m, "reasoning_token_count")
}

// readUsageCached walks the five-field cached-token fallback chain.
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

// normalizeUsageMap returns a copy of one upstream usage object with reasoning
// and cached detail mirrored into every shape the host / client may read.
// It never invents values: a detail that was absent stays absent.
func normalizeUsageMap(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	out := make(map[string]any, len(raw)+6)
	for k, v := range raw {
		out[k] = v
	}
	if rea, ok := readUsageReasoning(raw); ok {
		// 老师口径：reasoning 只写协议规范位——chat 是 completion_tokens_details
		// （Trae2api-cn sse.py:640-642 / qoder2api-hub qoder_proxy.py:472）。
		// 不写顶层（老师从不写，写了遇到"顶层与 details 相加"的消费方会双计）。
		// output_tokens_details 是补引擎 8.0.23 的 Responses 转换只查这一处的坑。
		ct := cloneUsageSub(out, "completion_tokens_details")
		ct["reasoning_tokens"] = rea
		out["completion_tokens_details"] = ct
		ot := cloneUsageSub(out, "output_tokens_details")
		ot["reasoning_tokens"] = rea
		out["output_tokens_details"] = ot
	}
	if cached, ok := readUsageCached(raw); ok {
		// 老师口径：cached 写 prompt_tokens_details.cached_tokens（chat 规范位，
		// Trae2api-cn sse.py:639），不写顶层。
		pd := cloneUsageSub(out, "prompt_tokens_details")
		pd["cached_tokens"] = cached
		out["prompt_tokens_details"] = pd
		// input_tokens_details 只在原本就存在时同步：凭空再写一个同义位置，
		// 宿主若把两处求和就会双计。
		if usageSubObject(raw, "input_tokens_details") != nil {
			id := cloneUsageSub(out, "input_tokens_details")
			id["cached_tokens"] = cached
			out["input_tokens_details"] = id
		}
	}
	// total 只在字段缺失时补（输入 + 输出；reasoning 已含在 output 里）。
	// 明确给了 0 就保留 0——老师从不拿估算值覆盖上游给的数。
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
	// 搬走不复制（老师口径）：被搬进 details 的顶层别名删掉，避免"顶层 + details"
	// 相加的消费方双计。没出现过的键删了也无副作用。
	for _, k := range []string{
		"reasoning_tokens", "reasoningTokens", "reasoning_token_count",
		"cached_tokens", "cachedTokens", "cache_read_tokens", "cacheReadTokens",
		"cache_read_input_tokens", "prompt_cache_hit_tokens",
	} {
		delete(out, k)
	}
	return out
}

// normalizeChunkUsage rewrites one SSE chunk JSON whose "usage" object needs
// the normalized shape. Chunks without usage are returned byte-identical so
// the hot streaming path pays nothing on ordinary content frames.
func normalizeChunkUsage(chunkJSON string) string {
	if chunkJSON == "" || !strings.Contains(chunkJSON, `"usage"`) {
		return chunkJSON
	}
	var obj map[string]any
	if json.Unmarshal([]byte(chunkJSON), &obj) != nil {
		return chunkJSON
	}
	u, ok := obj["usage"].(map[string]any)
	if !ok || len(u) == 0 {
		return chunkJSON
	}
	obj["usage"] = normalizeUsageMap(u)
	out, err := json.Marshal(obj)
	if err != nil {
		return chunkJSON
	}
	return string(out)
}
