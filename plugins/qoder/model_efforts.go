// model_efforts.go — 按模型钳制思考档位（2026-10-18 按老师 qoder2api-hub 重做）。
//
// 依据（老师源码）：
//   - 规则：qoder_proxy.py:2608-2651 normalize_reasoning_effort（关闭类统一成
//     "none" 照发；有档位表则命中原样 / 未命中取最近档、同距偏默认档；有
//     thinking_config 但无档位表（只有开/关）则除 none 外不下发；没有
//     thinking_config 的模型原样透传、不猜测）。
//   - 默认档：qoder_proxy.py:2596-2605（efforts 里 is_default 优先，没有标注
//     取支持表中间档）。
//   - 档位是否可关：目录 thinking_config 里出现 disabled 键（qoder_proxy.py:2275）。
//   - 表的数据源：官方目录快照 qoder_catalog_cn.json / qoder_catalog_intl.json
//     （qoder2api-hub 自带，2026-10 版本），不是 README 的四行样例。
//   - 请求体路径：只写 parameters.reasoning_effort（qoder_proxy.py:3944-3961），
//     全仓不使用 enable_thinking（0 命中）——我们的 body.go 同步去掉该字段。
//
// 档位集合的优先级：
//  1) 上游模型目录里下发的 thinking_config（callModelsAPI 若返回就采用）
//  2) 本文件的官方目录静态快照（cn / intl 两张表，同 key 双区可能不同）
//  3) 表里没有的模型：不注册 Thinking（页面不可设置），请求侧原样透传
package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// effortRank is the official ladder (qoder_proxy.py:2565-2566).
func effortRank(e string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "none":
		return 0, true
	case "minimal":
		return 1, true
	case "low":
		return 2, true
	case "medium":
		return 3, true
	case "high":
		return 4, true
	case "xhigh":
		return 5, true
	case "max":
		return 6, true
	}
	return 0, false
}

// effortOffValues: 关闭类取值统一映射成 "none"（qoder_proxy.py:2567）。
var effortOffValues = map[string]struct{}{
	"none": {}, "off": {}, "disabled": {}, "disable": {},
	"false": {}, "0": {}, "no": {},
}

// effortLevelOrder is the order /v1/models exposes (老师固定 low→max，不含 none)。
var effortLevelOrder = []string{"low", "medium", "high", "xhigh", "max"}

// qoderEffortSpec mirrors one model's thinking_config from the official catalog.
type qoderEffortSpec struct {
	hasThinking bool     // catalog has thinking_config (even if only on/off)
	levels      []string // legal levels, ordered by effortLevelOrder; empty = on/off only
	def         string   // is_default level; empty = middle of levels (老师规则)
	canDisable  bool     // thinking_config carries a disabled key
}

// qoderEffortsCN / qoderEffortsIntl are the official catalog snapshots
// (qoder_catalog_cn.json / qoder_catalog_intl.json). Keep them in sync with
// the catalog; entries absent here fall through to "unknown model" behavior.
var qoderEffortsCN = map[string]qoderEffortSpec{
	"qmodel_38max":  {hasThinking: true, levels: []string{"low", "medium", "xhigh"}, def: "medium", canDisable: true},
	"qfmodel":       {hasThinking: true, levels: []string{"low", "medium", "xhigh"}, def: "medium", canDisable: true},
	"qmodel_latest": {hasThinking: true, canDisable: true},
	"qmodel":        {hasThinking: true, canDisable: true},
	"dmodel":        {hasThinking: true, levels: []string{"high", "max"}, def: "max", canDisable: true},
	"dfmodel":       {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max", canDisable: true},
	"gmodel":        {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	"gfmodel":       {hasThinking: true, levels: []string{"high", "max"}, def: "max"},
	"gm51model":     {hasThinking: true, levels: []string{"high", "max"}, def: "max", canDisable: true},
	"kmodel_latest": {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	"kmodel":        {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	// auto / q37fmodel / mmodel 在 CN 目录里没有 thinking_config → 不进表
	// （原样透传；"表里没有" = 未知模型，与老师的"无 thinking_config"同路）。
}

var qoderEffortsIntl = map[string]qoderEffortSpec{
	"ultimate":      {hasThinking: true, levels: []string{"low", "medium", "high", "xhigh", "max"}, def: "high", canDisable: true},
	"performance":   {hasThinking: true, levels: []string{"low", "medium", "high", "xhigh", "max"}, def: "medium", canDisable: true},
	"smodel":        {hasThinking: true, levels: []string{"low", "medium", "high", "xhigh", "max"}, def: "high"},
	"cmodel":        {hasThinking: true, levels: []string{"low", "medium", "high", "xhigh", "max"}, def: "high"},
	"qmodel_38max":  {hasThinking: true, levels: []string{"low", "medium", "xhigh"}, def: "xhigh", canDisable: true},
	"qfmodel":       {hasThinking: true, levels: []string{"low", "medium", "xhigh"}, def: "medium", canDisable: true},
	"qmodel_latest": {hasThinking: true, canDisable: true},
	"qmodel":        {hasThinking: true, canDisable: true},
	"kmodel_latest": {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	"kmodel":        {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	"gmodel":        {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max"},
	"gfmodel":       {hasThinking: true, levels: []string{"high", "max"}, def: "max"},
	"dmodel":        {hasThinking: true, levels: []string{"high", "max"}, def: "max", canDisable: true},
	"dfmodel":       {hasThinking: true, levels: []string{"low", "high", "max"}, def: "max", canDisable: true},
	// efficient / mmodel 在 INTL 目录里没有 thinking_config → 不进表（原样透传）。
}

// normalizeModelKey mirrors cpaToUpstreamKey's output shape: lowercase, strip
// any "provider/" prefix.
func normalizeModelKey(modelKey string) string {
	key := strings.ToLower(strings.TrimSpace(modelKey))
	if i := strings.Index(key, "/"); i >= 0 {
		key = key[i+1:]
	}
	return key
}

// modelEffortSpec resolves one model's contract for a realm.
func modelEffortSpec(modelKey, realm string) (qoderEffortSpec, bool) {
	key := normalizeModelKey(modelKey)
	if normalizeRegion(realm) == regionIntl {
		if spec, ok := qoderEffortsIntl[key]; ok {
			return spec, true
		}
	}
	spec, ok := qoderEffortsCN[key]
	return spec, ok
}

// defaultEffort: catalog is_default, else the middle of the support list
// (老师规则 qoder_proxy.py:2596-2605).
func defaultEffort(spec qoderEffortSpec) string {
	if spec.def != "" {
		return spec.def
	}
	if len(spec.levels) == 0 {
		return ""
	}
	return spec.levels[len(spec.levels)/2]
}

// normalizeReasoningEffortFor clamps a client dial to the model's official set.
// Returns (value, inject). inject=false means: do not write reasoning_effort.
func normalizeReasoningEffortFor(modelKey, realm, s string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(s))
	if e == "" {
		return "", false
	}
	if _, off := effortOffValues[e]; off {
		// 关闭意图是一等公民：照发 "none"（老师 qoder_proxy.py:2629-2630）。
		return "none", true
	}
	spec, known := modelEffortSpec(modelKey, realm)
	if !known || !spec.hasThinking {
		// 目录里没有该模型的思考配置（路由器 / 未知模型）：原样透传，不猜。
		return e, true
	}
	if len(spec.levels) == 0 {
		// 只有开/关，没有档位表：除 none 外不下发（老师 qoder_proxy.py:2635-2637）。
		return "", false
	}
	for _, l := range spec.levels {
		if e == l {
			return e, true
		}
	}
	want, rankOK := effortRank(e)
	if !rankOK {
		// 乱值 → 默认档（老师 qoder_proxy.py:2640-2642）。
		if d := defaultEffort(spec); d != "" {
			return d, true
		}
		return spec.levels[0], true
	}
	// 最近合法档；同距偏向默认档（老师排序键 (|rank-want|, |rank-default|)）。
	def := defaultEffort(spec)
	defRank, defOK := effortRank(def)
	best := spec.levels[0]
	bestD, bestDD := 1<<30, 1<<30
	for _, l := range spec.levels {
		lr, ok := effortRank(l)
		if !ok {
			continue
		}
		d := lr - want
		if d < 0 {
			d = -d
		}
		dd := 0
		if defOK {
			dd = lr - defRank
			if dd < 0 {
				dd = -dd
			}
		}
		if d < bestD || (d == bestD && dd < bestDD) {
			best, bestD, bestDD = l, d, dd
		}
	}
	return best, true
}

// effortIsExplicit reports whether the client gave a meaningful dial: a legal
// level or an explicit off value. Used by the pinned-effort fallback so a
// client asking to disable thinking is never overridden by a pin.
func effortIsExplicit(s string) bool {
	e := strings.ToLower(strings.TrimSpace(s))
	if e == "" {
		return false
	}
	if _, off := effortOffValues[e]; off {
		return true
	}
	_, ok := effortRank(e)
	return ok
}

// effortsFromMeta reads thinking_config out of an upstream catalog entry.
// hasThinking=false means the entry has no thinking_config at all.
func effortsFromMeta(meta map[string]any) (levels []string, canDisable, hasThinking bool) {
	if meta == nil {
		return nil, false, false
	}
	tc, ok := meta["thinking_config"].(map[string]any)
	if !ok || len(tc) == 0 {
		return nil, false, false
	}
	hasThinking = true
	if _, ok := tc["disabled"]; ok {
		canDisable = true
	}
	enabled, _ := tc["enabled"].(map[string]any)
	efforts, _ := enabled["efforts"].(map[string]any)
	for _, l := range effortLevelOrder {
		if _, ok := efforts[l]; ok {
			levels = append(levels, l)
		}
	}
	return levels, canDisable, hasThinking
}

// thinkingForModel projects the contract into ModelInfo.Thinking. An upstream
// thinking_config always wins; otherwise the official catalog snapshot applies.
// nil = no effort info (the panel renders the model as not settable).
func thinkingForModel(modelKey, realm string, meta map[string]any) *pluginapi.ThinkingSupport {
	levels, canDisable, hasThinking := effortsFromMeta(meta)
	if hasThinking {
		if len(levels) == 0 {
			// 有思考配置但没有档位（只能开/关，如 cn 的 qmodel）：注册一个
			// 空 Levels 的 Thinking，页面据此显示"不可设置"而不是回退成五档
			// 假可选（老师用 reasoning_can_disable 表达同一事实）。
			return &pluginapi.ThinkingSupport{Levels: []string{}, ZeroAllowed: canDisable}
		}
		return &pluginapi.ThinkingSupport{
			Levels:      append([]string(nil), levels...),
			ZeroAllowed: canDisable,
		}
	}
	spec, known := modelEffortSpec(modelKey, realm)
	if !known || !spec.hasThinking {
		// 目录里没有该模型的思考配置（auto / q37fmodel / mmodel 等）：也要写一个
		// 明确的空 Thinking，而不是 nil——nil 会被管理台当成"缓存没刷到"而回退
		// 成五档假可选；空 Levels 才能如实显示"不可设置"。
		return &pluginapi.ThinkingSupport{Levels: []string{}}
	}
	if len(spec.levels) == 0 {
		return &pluginapi.ThinkingSupport{Levels: []string{}, ZeroAllowed: spec.canDisable}
	}
	return &pluginapi.ThinkingSupport{
		Levels:      append([]string(nil), spec.levels...),
		ZeroAllowed: spec.canDisable,
	}
}
