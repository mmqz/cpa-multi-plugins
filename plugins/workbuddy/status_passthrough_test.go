package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestErrorEnvelopeForCarriesStatus(t *testing.T) {
	raw := errorEnvelopeFor(&statusError{status: http.StatusPaymentRequired, err: errors.New("upstream 402: boom")})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got ok=%v", env.OK)
	}
	if env.Error.HTTPStatus != http.StatusPaymentRequired {
		t.Fatalf("http_status = %d, want 402", env.Error.HTTPStatus)
	}
	if env.Error.Message != "upstream 402: boom" {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

func TestErrorEnvelopeForPlainErrorOmitsStatus(t *testing.T) {
	raw := errorEnvelopeFor(errors.New("plain failure"))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil || env.Error.HTTPStatus != 0 {
		t.Fatalf("expected http_status omitted, got %+v", env.Error)
	}
}

// TestUpstreamStatusErrorPolicy locks the 0.9.17 pass-through matrix:
// account-level statuses wrap (host cooldown applies), request/IP-level
// shapes stay plain (only the 1-minute transient default).
func TestUpstreamStatusErrorPolicy(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
		want    int // 0 = plain error (no status), else expected status
	}{
		{"402 payment", 402, `{"code":11199,"msg":"insufficient credits"}`, 402},
		{"401 dead token", 401, `{"code":11101,"msg":"unauthorized"}`, 401},
		{"429 free quota drained", 429, `{"code":11120,"msg":"model quota exceeded"}`, 429},
		{"403 business envelope", 403, `{"code":11140,"msg":"request illegal"}`, 403},
		{"413 prompt too long", 413, `<html>413</html>`, 0},
		{"404 with 11115", 404, `{"code":11115,"msg":"prompt is too long"}`, 0},
		{"400 channel risk 11128", 400, `{"code":11128,"msg":"channel risk"}`, 0},
		{"400 model not registered", 400, `{"code":11102,"msg":"model [x] service info not found"}`, 0},
		{"403 waf no envelope", 403, `<html>challenge</html>`, 0},
		{"400 plain business", 400, `{"code":11199,"msg":"bad request"}`, 0},
		{"500 server", 500, `oops`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := errors.New("translated")
			err := upstreamStatusError(tc.status, tc.payload, base)
			var se *statusError
			if tc.want == 0 {
				if errors.As(err, &se) {
					t.Fatalf("status %d payload %q: expected plain error, got statusError{%d}", tc.status, tc.payload, se.status)
				}
				if !errors.Is(err, base) {
					t.Fatalf("plain error must wrap base")
				}
				return
			}
			if !errors.As(err, &se) {
				t.Fatalf("status %d payload %q: expected statusError, got plain", tc.status, tc.payload)
			}
			if se.status != tc.want {
				t.Fatalf("status = %d, want %d", se.status, tc.want)
			}
		})
	}
}

func TestStatusErrorUnwrapMessage(t *testing.T) {
	base := errors.New("translated message with 中文")
	se := &statusError{status: 429, err: base}
	if !strings.Contains(se.Error(), "中文") {
		t.Fatalf("Error() must delegate to wrapped error")
	}
	if se.StatusCode() != 429 {
		t.Fatalf("StatusCode = %d", se.StatusCode())
	}
}
