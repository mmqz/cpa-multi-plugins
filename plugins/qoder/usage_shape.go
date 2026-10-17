// usage_shape.go — 用量字段归一化（三家同一口径，2026-10-18 用户定）。
//
// 规则出处与 workbuddy / trae 的 usage_shape.go 完全一致（三个插件是独立
// go module，无法共享包）：读 reasoning / cached 走层层兜底链；写只写协议
// 规范位、不写顶层——reasoning 落 completion_tokens_details（output_tokens_details
// 是补引擎 8.0.23 的 Responses 转换只查这一处的坑），cached 落
// prompt_tokens_details（input_tokens_details 只在原本存在时同步，避免同一笔
// 缓存被两处相加）。顶层 reasoning_tokens / cached_tokens 一律不写。
//
// "读不到"与"上游明确给了 0"分开：只有字段真的出现过才双写，不拿 0 冒充。
// 改这里必须同步改 workbuddy / trae 的同名文件。
package main

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

// cloneUsageSub 复制一层嵌套 usage 对象——normalizeUsageMap 只是浅拷贝，
// 不复制就会就地改写入参。
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
	// 老师口径（trae sse.py:631-637 / qoder_proxy.py:473-474）：先读各家顶层别名，
	// details 两处放最后兜底。
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

func normalizeUsageMap(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	out := make(map[string]any, len(raw)+6)
	for k, v := range raw {
		out[k] = v
	}
	if rea, ok := readUsageReasoning(raw); ok {
		// 老师口径：reasoning 只写 chat 规范位 completion_tokens_details
		// （qoder_proxy.py:472 / trae sse.py:640-642），不写顶层。
		// output_tokens_details 是补引擎 8.0.23 的 Responses 转换只查这一处。
		ct := cloneUsageSub(out, "completion_tokens_details")
		ct["reasoning_tokens"] = rea
		out["completion_tokens_details"] = ct
		ot := cloneUsageSub(out, "output_tokens_details")
		ot["reasoning_tokens"] = rea
		out["output_tokens_details"] = ot
	}
	if cached, ok := readUsageCached(raw); ok {
		// 老师口径：cached 写 prompt_tokens_details.cached_tokens，不写顶层。
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
