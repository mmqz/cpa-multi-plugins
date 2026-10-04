package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// v0.12.50 大输入韧性：请求级失败给出明确指引，不再以裸形状交到客户端。

func TestChatHTTPErrorForInputTooLarge(t *testing.T) {
	err := chatHTTPErrorFor(400, upstream.ErrInputTooLarge, `{"msg":"prompt is too long"}`)
	msg := err.Error()
	for _, want := range []string{"输入过大", "请求级问题", "与账号无关", "压缩上下文", "prompt is too long"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg missing %q: %s", want, msg)
		}
	}
}

func TestChatHTTPErrorForHistoricalShape(t *testing.T) {
	err := chatHTTPErrorFor(400, upstream.ErrClient, `{"code":11101,"msg":"bad param"}`)
	want := "upstream 400 (client): "
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("err=%q want prefix %q", err.Error(), want)
	}
}

func TestSoloStreamEventMsg(t *testing.T) {
	got := soloStreamEventMsg(4001, "prompt is too long: 200000 > 131072")
	if !strings.Contains(got, "trae error code=4001") {
		t.Errorf("base lost: %s", got)
	}
	if !strings.Contains(got, "输入过大") || !strings.Contains(got, "请求级问题") {
		t.Errorf("guidance missing: %s", got)
	}
	// v0.12.79 (issue #9): 4001 非过大文案 = 模型不匹配，同样给请求级指引。
	mismatch := soloStreamEventMsg(4001, "param is invalid")
	if !strings.Contains(mismatch, "trae error code=4001") {
		t.Errorf("base lost: %s", mismatch)
	}
	if !strings.Contains(mismatch, "模型不在当前聊天通道") || !strings.Contains(mismatch, "与账号无关") {
		t.Errorf("model-mismatch guidance missing: %s", mismatch)
	}
	// 其他业务码保持裸形状。
	plain := soloStreamEventMsg(4023, "bad request")
	if plain != "trae error code=4023 msg=bad request" {
		t.Errorf("plain changed: %s", plain)
	}
}

func TestSoloStreamErrorCopy(t *testing.T) {
	se := &upstream.SOLOStreamError{Code: 0, Msg: "context length exceeded"}
	err := soloStreamErrorCopy(se)
	if !errors.Is(err, se) {
		t.Errorf("oversize copy lost %v", se)
	}
	if !strings.Contains(err.Error(), "输入过大") {
		t.Errorf("guidance missing: %s", err.Error())
	}
	// v0.12.79 (issue #9): 4001 模型不匹配 —— 原错误保留 + 请求级指引。
	mismatch := &upstream.SOLOStreamError{Code: 4001, Msg: "param is invalid"}
	copied := soloStreamErrorCopy(mismatch)
	if !errors.Is(copied, mismatch) {
		t.Errorf("mismatch copy lost %v", mismatch)
	}
	if !strings.Contains(copied.Error(), "模型不在当前聊天通道") {
		t.Errorf("model-mismatch guidance missing: %s", copied.Error())
	}
	// 非请求级错误原样透传。
	other := &upstream.SOLOStreamError{Code: 4023, Msg: "bad request"}
	if soloStreamErrorCopy(other) != error(other) {
		t.Errorf("plain error should pass through unchanged")
	}
}
