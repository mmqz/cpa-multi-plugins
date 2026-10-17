package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 2026-09-19 upstream alignment (reference: icebears111/qoderwork2api,
// btpp03/qoder-shim, docs.qoder.cn Qwen3.8-Flash limited-free notice):
//   - new model key qfmodel = Qwen3.8-Flash (limited-time free promo)
//   - every request sends is_reasoning=true + source="system" (unified
//     reasoning chain); upstream ignores both for models that cannot think
//   - OpenAI-style reasoning_effort (low/medium/xhigh) maps to the upstream
//     thinking parameters; absent/invalid keeps upstream defaults

func TestCPAToUpstreamKeyQwen38Flash(t *testing.T) {
	cases := map[string]string{
		"qwen3.8-flash": "qfmodel",
		"qfmodel":       "qfmodel",
		// issue #8: qmodel_preview / qwen3.8-max-preview are retired
		// upstream keys; both now map to the current Qwen3.8-Max key.
		"qwen3.8-max-preview": "qmodel_38max",
		"qwen3.8-max":         "qmodel_38max",
		"qmodel_preview":      "qmodel_38max",
		"qmodel_38max":        "qmodel_38max",
		"glm-5.2":             "gmodel",
		"glm-5.3":             "gmodel",
		"gm51model":           "gmodel",
		"gfmodel":             "gfmodel",
		"kimi-k3":             "kmodel_latest",
		"kmodel":              "kmodel",
		"kimi-k2.7-code":      "kmodel",
		"minimax-m2.7":        "mmodel",
		"minimax-m3":          "mmodel",
		"deepseek-flash":      "dfmodel",
		"qwen3.7-max":         "qmodel_latest",
		"qwen3.6-flash":       "q36fmodel",
		"deepseek-v4-flash":   "dfmodel",
		"auto":                "auto",
		"ultimate":            "ultimate",
		"qoder-performance":   "performance",
		"efficient":           "efficient",
		// dynamic-discovery keys ride along unchanged
		"brand-new-key": "brand-new-key",
	}
	for in, want := range cases {
		if got := cpaToUpstreamKey(in); got != want {
			t.Errorf("cpaToUpstreamKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildQoderBodyReasoningAlwaysOn(t *testing.T) {
	req := &openAIRequest{Model: "qfmodel", Messages: []openAIMessage{{Role: "user", Content: "hi"}}}
	raw, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json: %v", err)
	}
	mc, _ := m["model_config"].(map[string]any)
	if mc == nil {
		t.Fatal("missing model_config")
	}
	if v, _ := mc["is_reasoning"].(bool); !v {
		t.Errorf("model_config.is_reasoning = %v, want true", mc["is_reasoning"])
	}
	if v, _ := mc["source"].(string); v != "system" {
		t.Errorf("model_config.source = %q, want system (reasoning never streams without it)", v)
	}
	if v, _ := mc["key"].(string); v != "qfmodel" {
		t.Errorf("model_config.key = %q, want qfmodel", v)
	}
	cc, _ := m["chat_context"].(map[string]any)
	extra, _ := cc["extra"].(map[string]any)
	emc, _ := extra["modelConfig"].(map[string]any)
	if emc == nil {
		t.Fatal("missing chat_context.extra.modelConfig")
	}
	if v, _ := emc["is_reasoning"].(bool); !v {
		t.Errorf("extra.modelConfig.is_reasoning = %v, want true", emc["is_reasoning"])
	}
	// No client effort → parameters stay template-only: no enable_thinking,
	// template max_tokens preserved.
	params, _ := m["parameters"].(map[string]any)
	if params == nil {
		t.Fatal("missing template parameters")
	}
	if _, why := params["enable_thinking"]; why {
		t.Error("enable_thinking injected without client reasoning_effort")
	}
	if _, ok := params["max_tokens"]; !ok {
		t.Error("template parameters.max_tokens lost")
	}
}

func TestBuildQoderBodyReasoningEffortInjection(t *testing.T) {
	for _, effort := range []string{"low", "medium", "xhigh", "  XHIGH "} {
		req := &openAIRequest{
			Model:           "qfmodel",
			Messages:        []openAIMessage{{Role: "user", Content: "hi"}},
			ReasoningEffort: effort,
		}
		raw, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
		if err != nil {
			t.Fatalf("build(%q): %v", effort, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("json(%q): %v", effort, err)
		}
		params, _ := m["parameters"].(map[string]any)
		if params == nil {
			t.Fatalf("effort %q: parameters missing", effort)
		}
		// 老师口径（qoder_proxy.py:3944-3961）：只写 reasoning_effort，
		// 全仓不使用 enable_thinking。
		if _, why := params["enable_thinking"]; why {
			t.Errorf("effort %q: enable_thinking must not be injected", effort)
		}
		want := strings.ToLower(strings.TrimSpace(effort))
		if v, _ := params["reasoning_effort"].(string); v != want {
			t.Errorf("effort %q: reasoning_effort = %q, want %q", effort, v, want)
		}
		if _, ok := params["max_tokens"]; !ok {
			t.Errorf("effort %q: template max_tokens lost", effort)
		}
	}
}

func TestBuildQoderBodyReasoningEffortClamped(t *testing.T) {
	// 老师规则（qoder_proxy.py:2608-2651）：关闭类照发 none；乱值落默认档；
	// 空值不下发。qmodel_38max 的官方目录档位是 low/medium/xhigh、默认 medium。
	cases := []struct {
		effort string
		want   string // "" = 不下发
	}{
		{"", ""},
		{"ultra", "medium"},
		{"off", "none"},
		{"none", "none"},
		{"10", "medium"},
		{"high", "medium"}, // qmodel_38max 没有 high，最近档 + 同距偏默认 → medium
	}
	for _, c := range cases {
		req := &openAIRequest{
			Model:           "qmodel_38max",
			Messages:        []openAIMessage{{Role: "user", Content: "hi"}},
			ReasoningEffort: c.effort,
		}
		raw, err := buildQoderBody(req, "qmodel_38max", "personal_professional_trial")
		if err != nil {
			t.Fatalf("build(%q): %v", c.effort, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("json(%q): %v", c.effort, err)
		}
		params, _ := m["parameters"].(map[string]any)
		if params == nil {
			if c.want != "" {
				t.Errorf("effort %q: parameters missing", c.effort)
			}
			continue
		}
		if _, why := params["enable_thinking"]; why {
			t.Errorf("effort %q: enable_thinking must not be injected", c.effort)
		}
		got, _ := params["reasoning_effort"].(string)
		if got != c.want {
			t.Errorf("effort %q: reasoning_effort = %q, want %q", c.effort, got, c.want)
		}
	}
}

func TestStaticModelsIncludeQwen38Flash(t *testing.T) {
	found := false
	for _, m := range wbModels() {
		if m.ID == "qfmodel" {
			found = true
			if m.Name != "Qwen3.8-Flash" {
				t.Errorf("qfmodel name = %q, want Qwen3.8-Flash", m.Name)
			}
			if m.ContextLength != 180000 {
				t.Errorf("qfmodel context = %d", m.ContextLength)
			}
		}
	}
	if !found {
		t.Error("static catalog missing qfmodel (Qwen3.8-Flash)")
	}
}
