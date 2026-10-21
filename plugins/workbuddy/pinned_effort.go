// pinned_effort.go — 【本地补丁 · 档位管家】
//
// models[].reasoning 是强制钉：只要它对当前模型真实档位有效，就覆盖客户端。
// models[].default_reasoning 是默认档：只在客户端没有给出有效档位时兜底。
// 两者都没配置时，WorkBuddy 继续用“真实支持的最高档”作为最后兜底。
//
// 优先级：强制钉 > 客户端有效档位 > 默认档 > 真实最高档兜底。
//
// 配置形状（引擎 config.yaml → plugins.configs.workbuddy）：
//
//	models:
//	  - id: deepseek-v4.1-flash
//	    reasoning: high
//	    default_reasoning: low
//
// 键支持 id / name / alias 任一，大小写不敏感；带 "provider/" 前缀也认。
package main

import (
	"log"
	"strings"
	"sync"
)

var (
	effortPolicyMu sync.RWMutex
	pinnedEfforts  = map[string]string{}
	defaultEfforts = map[string]string{}
)

// setPinnedEffortsFromModels 用插件配置里的 models 列表重建档位策略表。
// reasoning 是强制钉；default_reasoning 是默认档。id/name/alias 都登记为键。
// 列表为空/缺失 → 清空两套配置（配置是唯一事实来源）。
func setPinnedEffortsFromModels(models any) {
	nextPinned := map[string]string{}
	nextDefault := map[string]string{}
	if list, ok := models.([]any); ok {
		for _, item := range list {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			pin := strings.ToLower(strings.TrimSpace(configScalarString(obj["reasoning"])))
			def := strings.ToLower(strings.TrimSpace(configScalarString(obj["default_reasoning"])))
			if pin == "" && def == "" {
				continue
			}
			for _, key := range []string{"id", "name", "alias"} {
				if k := strings.ToLower(strings.TrimSpace(configScalarString(obj[key]))); k != "" {
					if pin != "" {
						nextPinned[k] = pin
					}
					if def != "" {
						nextDefault[k] = def
					}
				}
			}
		}
	}
	effortPolicyMu.Lock()
	pinnedEfforts = nextPinned
	defaultEfforts = nextDefault
	effortPolicyMu.Unlock()
}

// normalizedModelKey strips a provider prefix and lowercases the id.
func normalizedModelKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// advertisedEffort returns the configured level only when the model itself
// advertises it. Unknown levels are never sent to the upstream.
func advertisedEffort(model, effort string) string {
	if effort == "" {
		return ""
	}
	for _, level := range thinkingLevelsForModel(model) {
		if strings.EqualFold(strings.TrimSpace(level), effort) {
			return strings.ToLower(strings.TrimSpace(level))
		}
	}
	return ""
}

// pinnedReasoningEffort returns the configured forced effort when it is one of
// the model's advertised levels; empty means "no applicable forced pin".
func pinnedReasoningEffort(model string) string {
	m := normalizedModelKey(model)
	effortPolicyMu.RLock()
	pin := pinnedEfforts[m]
	effortPolicyMu.RUnlock()
	return advertisedEffort(model, pin)
}

// defaultReasoningEffort returns the configured default effort when it is one
// of the model's advertised levels; empty means "no applicable default".
func defaultReasoningEffort(model string) string {
	m := normalizedModelKey(model)
	effortPolicyMu.RLock()
	def := defaultEfforts[m]
	effortPolicyMu.RUnlock()
	return advertisedEffort(model, def)
}

// applyPinnedEffortInPlace applies the forced pin. It intentionally wins over
// a valid client value; that is the difference between a pin and a default.
func applyPinnedEffortInPlace(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	eff := pinnedReasoningEffort(model)
	if eff == "" {
		return false
	}
	obj["reasoning_effort"] = eff
	return true
}

// applyDefaultEffortInPlace applies the default only when the client did not
// give a meaningful effort. A valid client choice always wins over default.
func applyDefaultEffortInPlace(obj map[string]any) bool {
	current, _ := obj["reasoning_effort"].(string)
	if clientEffortGiven(current) {
		return false
	}
	model, _ := obj["model"].(string)
	eff := defaultReasoningEffort(model)
	if eff == "" {
		return false
	}
	obj["reasoning_effort"] = eff
	return true
}

// clientEffortGiven reports whether the client supplied a meaningful effort:
// the named ladder or an explicit off value. Empty and unknown values fall
// through to the tier policy/default.
func clientEffortGiven(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low", "medium", "high", "xhigh", "max", "none", "off":
		return true
	}
	return false
}

// logTierProbe logs the client value, the applicable forced pin and the
// final value actually prepared for the upstream.
func logTierProbe(model, clientEff, pin, final string) {
	log.Printf("[tierprobe] workbuddy model=%s client=%q pin=%q final=%q",
		model, clientEff, pin, final)
}
