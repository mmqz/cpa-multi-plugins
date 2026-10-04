package main

// issue #18 回归：intl 出站模型名与 CN/SOLO 同接缝——body 里的名字带着宿主
// 凭据前缀原样进入插件，而 intlupstream 的 resolveMode 只剥插件自己的
// -intl 后缀；宿主解析后的 ExecutorRequest.Model 必须优先。

import "testing"

func TestIntlResolvedModelPrefersHostResolved(t *testing.T) {
	cases := []struct {
		name, resolved, body, want string
	}{
		{"prefixed body, resolved wins", "kimi-k2.6-intl", "tr/kimi-k2.6-intl", "kimi-k2.6-intl"},
		{"empty resolved falls back to body", "", "tr/kimi-k2.6-intl", "tr/kimi-k2.6-intl"},
		{"no prefix, identical", "kimi-k2.6-intl", "kimi-k2.6-intl", "kimi-k2.6-intl"},
		{"resolved already bare", "kimi-k2.6", "tr/kimi-k2.6-intl", "kimi-k2.6"},
		{"deepseek-style upstream segment", "deepseek-ai/deepseek-v4-intl", "tr/deepseek-ai/deepseek-v4-intl", "deepseek-ai/deepseek-v4-intl"},
	}
	for _, c := range cases {
		if got := intlResolvedModel(c.resolved, c.body); got != c.want {
			t.Errorf("%s: intlResolvedModel(%q,%q)=%q want %q", c.name, c.resolved, c.body, got, c.want)
		}
	}
}
