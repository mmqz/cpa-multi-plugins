// pinned_effort.go — 【本地补丁 · 档位管家】
//
// 让插件配置里的 models[].reasoning 真正生效：每个请求发出前，按模型名查一次
// "钉子"；客户端带了自己的有效档（low/medium/high/xhigh/max）→ 让路听它的；
// 没带、或带的是上游不认的值 → 用钉子兜底。没钉的模型完全不受影响（2026-10-15 用户定）。
//
// 配置形状（引擎 config.yaml → plugins.configs.workbuddy）：
//
//	models:
//	  - id: deepseek-v4.1-flash
//	    reasoning: low
//
// 键支持 id / name / alias 任一，大小写不敏感；带 "provider/" 前缀也认。
package main

import (
	"log"
	"strings"
	"sync"
)

var (
	pinnedEffortMu sync.RWMutex
	pinnedEfforts  = map[string]string{}
)

// setPinnedEffortsFromModels 用插件配置里的 models 列表重建钉子表。
// 每项取 reasoning（非空字符串生效）；id/name/alias 任一都登记为键。
// 列表为空/缺失 → 清空钉子（配置是唯一事实来源）。
func setPinnedEffortsFromModels(models any) {
	next := map[string]string{}
	if list, ok := models.([]any); ok {
		for _, item := range list {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			eff := strings.ToLower(strings.TrimSpace(configScalarString(obj["reasoning"])))
			if eff == "" {
				continue
			}
			for _, key := range []string{"id", "name", "alias"} {
				if k := strings.ToLower(strings.TrimSpace(configScalarString(obj[key]))); k != "" {
					next[k] = eff
				}
			}
		}
	}
	pinnedEffortMu.Lock()
	pinnedEfforts = next
	pinnedEffortMu.Unlock()
}

// pinnedReasoningEffort 查某个模型名上的钉子（"" = 没钉）。带 provider/ 前缀也认。
func pinnedReasoningEffort(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	pinnedEffortMu.RLock()
	defer pinnedEffortMu.RUnlock()
	return pinnedEfforts[m]
}

// applyPinnedEffortInPlace 在请求体上落钉兜底：客户端没带有效档（或带的是乱值）时，才把 reasoning_effort 填成钉值。
// 必须在 mirrorReasoningEffort（会镜像 reasoning_summary）之前调用。
func applyPinnedEffortInPlace(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	eff := pinnedReasoningEffort(model)
	if eff == "" {
		return false
	}
	cur, _ := obj["reasoning_effort"].(string)
	if clientEffortGiven(cur) {
		return false
	}
	obj["reasoning_effort"] = eff
	return true
}

// logTierProbe 诊断日志（档位管家实测专用）：把「客户端传的档位 / 钉的档位 /
// 最终发往上游的档位」打成一行，走标准库 log → 引擎 journald 可见。
func logTierProbe(model, clientEff, pin, final string) {
	log.Printf("[tierprobe] workbuddy model=%s client=%q pin=%q final=%q",
		model, clientEff, pin, final)
}

// clientEffortGiven 客户端这一笔算不算「带了自己的档」：low/medium/high/xhigh/max
// 是有效档；none/off 是「明确要走无思考」的表达（上游认，尊重它）；空、缺、
// 乱值（ultra 这类）都算没带——交给钉子兜底，也避免乱值扔给上游。
func clientEffortGiven(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low", "medium", "high", "xhigh", "max", "none", "off":
		return true
	}
	return false
}
