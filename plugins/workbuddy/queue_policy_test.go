package main

import (
	"errors"
	"net/http"
	"testing"
)

// 2026-10-22 用户定：请求级确定性错统一按 400 回宿主，让引擎第一次就停；
// 账号级错误（401/402/429）保持原状态，模型级 6004 保持无状态（排队）。
func TestUpstreamStatusErrorRequestLevelIs400(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
	}{
		{"model not registered", http.StatusBadRequest, `{"code":11102,"msg":"model [x] service info not found"}`},
		{"prompt too long", http.StatusNotFound, `{"code":11115,"msg":"prompt is too long"}`},
		{"channel risk", http.StatusBadRequest, `{"code":11128,"msg":"channel risk"}`},
		{"waf challenge", http.StatusForbidden, `<html>challenge</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := upstreamStatusError(tc.status, tc.payload, errors.New("translated"))
			var se *statusError
			if !errors.As(err, &se) {
				t.Fatalf("status %d payload %q: expected statusError, got %T", tc.status, tc.payload, err)
			}
			if se.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", se.status)
			}
		})
	}
}

func TestUpstreamStatusErrorAccountLevelUnchanged(t *testing.T) {
	cases := []struct {
		status  int
		payload string
		want    int
	}{
		{http.StatusUnauthorized, `{"code":11101,"msg":"unauthorized"}`, http.StatusUnauthorized},
		{http.StatusPaymentRequired, `{"code":11199,"msg":"insufficient credits"}`, http.StatusPaymentRequired},
		{http.StatusTooManyRequests, `{"code":11120,"msg":"model quota exceeded"}`, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		err := upstreamStatusError(tc.status, tc.payload, errors.New("translated"))
		var se *statusError
		if !errors.As(err, &se) || se.status != tc.want {
			t.Fatalf("status %d: got %T %v, want statusError{%d}", tc.status, err, err, tc.want)
		}
	}
}

func TestUpstreamStatusErrorModelScopedLimitStaysPlain(t *testing.T) {
	payload := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-12-31 10:00:00 UTC+8 重置，您也可以切换其他模型继续使用。"}`
	err := upstreamStatusError(http.StatusTooManyRequests, payload, errors.New("translated"))
	var se *statusError
	if errors.As(err, &se) {
		t.Fatalf("6004 must stay status-less so the credential is never cooled; got statusError{%d}", se.status)
	}
}

func TestStreamFaultStatusRequestLevelIs400(t *testing.T) {
	if got := streamFaultStatus(`{"code":11102,"msg":"model [x] service info not found"}`); got != http.StatusBadRequest {
		t.Fatalf("11102 frame status = %d, want 400", got)
	}
	payload := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-12-31 10:00:00 UTC+8 重置，您也可以切换其他模型继续使用。"}`
	if got := streamFaultStatus(payload); got != 0 {
		t.Fatalf("6004 frame status = %d, want 0 (queue, no credential cooldown)", got)
	}
}
