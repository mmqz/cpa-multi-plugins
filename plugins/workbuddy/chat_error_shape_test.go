// chat_error_shape_test.go covers the 0.9.14 error-shape translations:
// 11115 prompt-too-long, WAF-shaped bare 403, business-envelope detection,
// and the Retry-After header family hint.
package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsPromptTooLong(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
		want    bool
	}{
		{"envelope 400", http.StatusBadRequest, `{"code":11115,"msg":"prompt is too long"}`, true},
		{"string code", http.StatusBadRequest, `{"code":"11115","msg":"prompt is too long"}`, true},
		{"wording only", http.StatusBadRequest, `upstream says prompt is too long for model`, true},
		{"413 envelope", http.StatusRequestEntityTooLarge, `{"code":11115,"msg":"x"}`, true},
		{"404 spaced envelope", http.StatusNotFound, `{"code": 11115, "msg":"x"}`, true},
		{"wrong status", http.StatusInternalServerError, `{"code":11115}`, false},
		{"429 stays rate", http.StatusTooManyRequests, `{"code":11115}`, false},
		{"other 400", http.StatusBadRequest, `{"code":11101,"msg":"bad"}`, false},
	}
	for _, tc := range cases {
		if got := isPromptTooLong(tc.status, tc.payload); got != tc.want {
			t.Errorf("%s: isPromptTooLong(%d, ...) = %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
}

func TestIsWafBlocked(t *testing.T) {
	if !isWafBlocked(http.StatusForbidden, `<html><body>403 Forbidden</body></html>`) {
		t.Error("HTML page 403 must be WAF")
	}
	if !isWafBlocked(http.StatusForbidden, "") {
		t.Error("empty-body 403 must be WAF")
	}
	if !isWafBlocked(http.StatusForbidden, "plain text reject") {
		t.Error("plain-text 403 must be WAF")
	}
	if isWafBlocked(http.StatusForbidden, `{"code":11140,"msg":"request illegal"}`) {
		t.Error("business 403 keeps its own classification")
	}
	if isWafBlocked(http.StatusBadRequest, "<html>403</html>") {
		t.Error("non-403 status is never WAF")
	}
}

func TestHasBusinessEnvelope(t *testing.T) {
	if !hasBusinessEnvelope(`{"code":0}`) || !hasBusinessEnvelope(`{"msg":"x"}`) {
		t.Error("code/msg field names are the envelope markers")
	}
	if hasBusinessEnvelope("<html>error page</html>") {
		t.Error("HTML has no envelope")
	}
}

func TestRetryAfterHintFamily(t *testing.T) {
	// Seconds.
	h := http.Header{}
	h.Set("Retry-After", "30")
	if got := retryAfterHint(h); got == "" || !strings.Contains(got, "30") {
		t.Errorf("Retry-After seconds hint = %q", got)
	}
	// Milliseconds.
	h2 := http.Header{}
	h2.Set("Retry-After-Ms", "1500")
	if got := retryAfterHint(h2); got == "" || !strings.Contains(got, "1500") {
		t.Errorf("Retry-After-Ms hint = %q", got)
	}
	// Epoch seconds in the near future.
	h3 := http.Header{}
	h3.Set("X-RateLimit-Reset", "not-a-number")
	if got := retryAfterHint(h3); got != "" {
		t.Errorf("invalid reset must yield empty, got %q", got)
	}
	// Absent headers → empty.
	if got := retryAfterHint(http.Header{}); got != "" {
		t.Errorf("no headers must yield empty, got %q", got)
	}
	if got := retryAfterHint(nil); got != "" {
		t.Errorf("nil header must yield empty, got %q", got)
	}
	// Oversane values are dropped (cap 2h).
	h4 := http.Header{}
	h4.Set("Retry-After", "99999")
	if got := retryAfterHint(h4); got != "" {
		t.Errorf("over-cap Retry-After must be dropped, got %q", got)
	}
}

func TestTranslateChatUpstreamErrorFullShapes(t *testing.T) {
	sa := &storedAuth{}

	// 11115 → request-level copy, not the raw envelope.
	err := translateChatUpstreamErrorFull(http.StatusBadRequest,
		`{"code":11115,"msg":"prompt is too long"}`, sa, nil)
	msg := err.Error()
	if !strings.Contains(msg, "11115") || !strings.Contains(msg, "账号无关") {
		t.Errorf("11115 copy must say request-level: %s", msg)
	}

	// WAF 403 → WAF copy.
	err = translateChatUpstreamErrorFull(http.StatusForbidden,
		"<html>blocked</html>", sa, nil)
	msg = err.Error()
	if !strings.Contains(msg, "WAF") || !strings.Contains(msg, "403") {
		t.Errorf("WAF copy must name WAF+403: %s", msg)
	}

	// Business 403 keeps the raw shape.
	err = translateChatUpstreamErrorFull(http.StatusForbidden,
		`{"code":11140,"msg":"request illegal"}`, sa, nil)
	if !strings.Contains(err.Error(), "upstream 403") {
		t.Errorf("business 403 keeps raw shape: %s", err.Error())
	}

	// 11102 keeps its dedicated translation (priority over newer shapes).
	err = translateChatUpstreamErrorFull(http.StatusBadRequest,
		`{"code":11102,"msg":"model [x] service info not found"}`, sa, nil)
	if !strings.Contains(err.Error(), "11102") {
		t.Errorf("11102 translation must survive: %s", err.Error())
	}

	// Retry-After header appends the hint to the raw shape.
	h := http.Header{}
	h.Set("Retry-After", "45")
	err = translateChatUpstreamErrorFull(http.StatusTooManyRequests,
		`{"code":11120,"msg":"rate limited"}`, sa, h)
	msg = err.Error()
	if !strings.Contains(msg, "45") {
		t.Errorf("Retry-After hint must be appended: %s", msg)
	}

	// No headers → no hint, raw shape.
	err = translateChatUpstreamErrorFull(http.StatusTooManyRequests,
		`{"code":11120,"msg":"rate limited"}`, sa, nil)
	if strings.Contains(err.Error(), "重试（") {
		t.Errorf("no hint expected without headers: %s", err.Error())
	}
}
