package upstream

import (
	"testing"
)

// issue #9 (v0.12.79) 回归锁定：cn→inline_chat 死路修复 + 4001 语义重分类。
// 三方证据：trae2api-web RESEARCH.md（llm_utils_chat 仅接受 solo_work_lite）、
// trae2api-more IsModelConfigMismatch（4001=模型在当前 function 不可用）、
// 报告者 variant 翻转实验（同一 cn JWT 仅换 function 立即可用）。

func TestFunctionForAllVariantsRideSoloWorkLite(t *testing.T) {
	for _, v := range []string{"cn", "solo", "intl", "solo-intl", "", "unknown"} {
		if got := FunctionFor(v); got != "solo_work_lite" {
			t.Errorf("FunctionFor(%q) = %q, want solo_work_lite (llm_utils_chat's only live function)", v, got)
		}
	}
}

// 顺序敏感：4001 + 过大文案仍归 ErrInputTooLarge（v0.12.50 语义不得回归），
// 只有泛化 "param is invalid" 的 4001 才是模型不匹配。
func TestKind4001OrderSensitive(t *testing.T) {
	tooLarge := &SOLOStreamError{Code: 4001, Msg: "prompt is too long: 200000 > 131072"}
	if k := tooLarge.Kind(); k != ErrInputTooLarge {
		t.Errorf("4001 + too-large text kind = %v, want input_too_large", k)
	}
	mismatch := &SOLOStreamError{Code: 4001, Msg: "We're sorry, the param is invalid."}
	if k := mismatch.Kind(); k != ErrModelUnavailable {
		t.Errorf("4001 generic kind = %v, want model_unavailable", k)
	}
	if k := (&SOLOStreamError{Code: 4023, Msg: "param is invalid"}).Kind(); k != ErrClient {
		t.Errorf("4023 kind = %v, want client (only 4001 is model-mismatch)", k)
	}
	if !IsModelMismatchCode(4001) {
		t.Errorf("4001 should be model-mismatch")
	}
	if IsModelMismatchCode(0) || IsModelMismatchCode(4023) {
		t.Errorf("IsModelMismatchCode table wrong")
	}
	if got := ErrModelUnavailable.String(); got != "model_unavailable" {
		t.Errorf("String() = %q", got)
	}
}

// HTTP 状态路径：4xx + body code=4001 → ErrModelUnavailable；但 4001 + 过大
// 文案仍是 ErrInputTooLarge；413 语义唯一（最顶判定）不受影响。
func TestClassify4001ModelUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"400-4001-mismatch", 400, `{"code":4001,"msg":"We're sorry, the param is invalid."}`, ErrModelUnavailable},
		{"400-4001-too-large", 400, `{"code":4001,"msg":"prompt is too long: 200000 tokens > 131072 maximum context length"}`, ErrInputTooLarge},
		{"413-semantic-unique", 413, `{"code":4001,"msg":"anything"}`, ErrInputTooLarge},
		{"400-other-4xx", 400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{"401-still-session", 401, `{"code":4001,"msg":"unauthorized"}`, ErrSessionDead},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: Classify(%d, body) = %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
}

func TestConfigIsSoloAgentOnly(t *testing.T) {
	dead := []string{"agnes-agent-x", "Agnes-Code", "DeepSeek-V4-Flash", "deepseek-v4-pro-official", "DEEPSEEK-V4"}
	live := []string{"deepseek-v5-flash", "kimi-k2.6", "glm-5.2", "Doubao-Seed-2.1-Pro", "seed_m8"}
	for _, n := range dead {
		if !configIsSoloAgentOnly(n) {
			t.Errorf("%q should be solo_agent-only (dead on llm_utils_chat)", n)
		}
	}
	for _, n := range live {
		if configIsSoloAgentOnly(n) {
			t.Errorf("%q wrongly blocked", n)
		}
	}
}
