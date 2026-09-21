package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/pool"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// issue #9 (v0.12.79) 主包回归：模型类 4001 不冷却账号 + 静态目录与
// solo_work_lite 通道一致 + 客户端文案给请求级指引。

// ErrModelUnavailable 是请求级失败：换任何账号结果相同，不得 NoteError
// 累计冷却健康账号（对齐 trae2api-more/TraeWorkAssistant 语义）。
func TestApplyCooldownModelUnavailableIsNoop(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok", Variant: "cn"})

	for i := 0; i < 5; i++ {
		applyCooldownOn(p, "u1", upstream.ErrModelUnavailable)
	}
	if st, ok := p.Status("u1"); ok {
		if st.ErrCount != 0 || st.Cooling || st.Disabled {
			t.Errorf("model-unavailable must not punish account: %+v", st)
		}
	}

	// 对照组：ErrClient 仍走 NoteError 累计（threshold=3）。
	applyCooldownOn(p, "u1", upstream.ErrClient)
	applyCooldownOn(p, "u1", upstream.ErrClient)
	if st, ok := p.Status("u1"); !ok || st.ErrCount != 2 {
		t.Errorf("ErrClient should accumulate errCount=2, got %+v (ok=%v)", st, ok)
	}

	// 对照组：ErrInputTooLarge 同样不惩罚（v0.12.50 语义不回归）。
	p2 := pool.New("")
	p2.Add(&auth.Auth{UID: "u2", AccessToken: "tok", Variant: "solo"})
	for i := 0; i < 5; i++ {
		applyCooldownOn(p2, "u2", upstream.ErrInputTooLarge)
	}
	if st, ok := p2.Status("u2"); ok && (st.ErrCount != 0 || st.Cooling) {
		t.Errorf("input-too-large must not punish account: %+v", st)
	}
}

func TestApplyCooldownClientAccumulates(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u3", AccessToken: "tok", Variant: "cn"})
	for i := 0; i < 3; i++ {
		applyCooldownOn(p, "u3", upstream.ErrClient)
	}
	st, ok := p.Status("u3")
	if !ok || !st.Cooling {
		t.Fatalf("3x ErrClient should cool the account, got %+v (ok=%v)", st, ok)
	}
	if time.Until(st.Until) <= 0 {
		t.Errorf("cooldown window expired immediately: %v", st.Until)
	}
}

// 静态目录：所有 variant 共享 solo_work_lite 快照 —— 死模型（IDE 目录 /
// solo_agent-only 名单）不得出现，报告者实测可用的 glm-5.3 必须在列。
func TestStaticFallbackCatalogMatchesChatLane(t *testing.T) {
	all := map[string][]string{}
	for _, id := range idsOfModelInfos(staticForVariant("cn")) {
		all[id] = append(all[id], "staticForVariant(cn)")
	}
	for _, id := range idsOfModelInfos(staticForVariant("solo")) {
		all[id] = append(all[id], "staticForVariant(solo)")
	}
	for _, id := range idsOfModelInfos(staticUnionModels()) {
		all[id] = append(all[id], "staticUnionModels()")
	}
	dead := []string{"seed_m8", "kimi-k2", "Doubao-Seed-Code", "agnes-agent-x",
		"DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "DeepSeek-V4-Flash-Official", "DeepSeek-V4-Pro-Official"}
	for _, d := range dead {
		if _, ok := all[d]; ok {
			t.Errorf("dead model %q back in static fallback (%v)", d, all[d])
		}
	}
	live := []string{"glm-5.2", "glm-5.3", "kimi-k2.6", "kimi-k2.7-code", "minimax-m3",
		"Doubao-Seed-2.1-Pro", "qwen-3.7-plus", "qwen3.8-max"}
	for _, l := range live {
		if _, ok := all[l]; !ok {
			t.Errorf("live model %q missing from static fallback", l)
		}
	}
}

// HTTP 路径的 4001 同样给请求级指引文案（对齐输入过大的 v0.12.50 形状）。
func TestChatHTTPErrorForModelUnavailable(t *testing.T) {
	err := chatHTTPErrorFor(400, upstream.ErrModelUnavailable, `{"code":4001,"msg":"the param is invalid"}`)
	msg := err.Error()
	for _, want := range []string{"模型不在当前聊天通道", "请求级问题", "与账号无关", "其他模型", "code\":4001"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg missing %q: %s", want, msg)
		}
	}
}

func idsOfModelInfos(ms []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
