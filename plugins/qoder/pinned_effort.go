// pinned_effort.go — 【本地补丁 · 档位管家】
//
// models[].reasoning 是强制钉：只要非空就覆盖客户端；Qoder 在发上游前仍会
// 按官方目录归一化/钳制。models[].default_reasoning 是默认档：只在客户端
// 没有给出有效档位时兜底。
//
// 优先级：强制钉 > 客户端有效档位 > 默认档 > Qoder 官方目录兜底。
//
// 配置形状（引擎 config.yaml → plugins.configs.qoder）：
//
//	models:
//	  - id: qfmodel
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

// pinnedReasoningEffort returns the configured forced effort; empty means
// "no pin". It stays unvalidated here so Qoder's official-catalog normalizer
// remains the single place that clamps to a model's real levels.
func pinnedReasoningEffort(model string) string {
	m := normalizedModelKey(model)
	effortPolicyMu.RLock()
	defer effortPolicyMu.RUnlock()
	return pinnedEfforts[m]
}

// defaultReasoningEffort returns the configured default; empty means none.
func defaultReasoningEffort(model string) string {
	m := normalizedModelKey(model)
	effortPolicyMu.RLock()
	defer effortPolicyMu.RUnlock()
	return defaultEfforts[m]
}

// resolveTierEffort applies the tier-manager precedence before Qoder's
// official-catalog normalization.
func resolveTierEffort(clientEffort, pin, def string) string {
	if pin != "" {
		return pin
	}
	if def != "" && !effortIsExplicit(clientEffort) {
		return def
	}
	return clientEffort
}

// logTierProbe logs the client value, the forced pin/default configuration
// and the final effort actually sent toward the upstream normalizer.
func logTierProbe(model, clientEff, pin, def, final string) {
	log.Printf("[tierprobe] qoder model=%s client=%q pin=%q default=%q final=%q",
		model, clientEff, pin, def, final)
}
