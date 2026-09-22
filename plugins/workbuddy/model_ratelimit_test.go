package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The exact upstream shape observed on 2026-09-22 (issue round): the gateway
// itself declares the limit model-scoped by advising "switch to another model".
const modelRateLimitSamplePayload = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"055d655c-f97b-4d00-aba8-9d55a4e588f6"}`

func TestIsModelScopedRateLimit(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
		want    bool
	}{
		{"canonical 6004", http.StatusTooManyRequests, modelRateLimitSamplePayload, true},
		{"json envelope 6004", http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, true},
		{"string code variant", http.StatusTooManyRequests, `gateway said "code":"6004" somewhere`, true},
		{"spaced variant", http.StatusTooManyRequests, `{ "code" : 6004 }`, true},
		{"other 429 code", http.StatusTooManyRequests, `{"code":11120,"msg":"quota exceeded"}`, false},
		{"6004 on 400", http.StatusBadRequest, `{"code":6004,"msg":"x"}`, false},
		{"6004 on 200", http.StatusOK, `{"code":6004,"msg":"x"}`, false},
		{"empty body", http.StatusTooManyRequests, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelScopedRateLimit(tc.status, tc.payload); got != tc.want {
				t.Fatalf("isModelScopedRateLimit(%d, %q) = %v, want %v", tc.status, tc.payload, got, tc.want)
			}
		})
	}
}

func TestParseRateLimitResetAtFrom(t *testing.T) {
	// now sits safely before the sample reset (09:46:39 UTC+8 on 2026-09-22).
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	reset, ok := parseRateLimitResetAtFrom(modelRateLimitSamplePayload, now)
	if !ok {
		t.Fatalf("canonical sample must parse, payload=%s", modelRateLimitSamplePayload)
	}
	want := time.Date(2026, 9, 22, 9, 46, 39, 0, wbCNZone)
	if !reset.Equal(want) {
		t.Fatalf("reset = %v, want %v", reset, want)
	}

	// Past reset → treated as absent (caller falls back to the default window).
	past, ok := parseRateLimitResetAtFrom(modelRateLimitSamplePayload, want.Add(time.Minute))
	if ok || !past.IsZero() {
		t.Fatalf("past reset must be absent, got %v ok=%v", past, ok)
	}

	// Beyond the horizon cap → absent.
	far, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"将在 2027-09-22 09:46:39 UTC+8 重置"}`, now)
	if ok || !far.IsZero() {
		t.Fatalf("beyond-horizon reset must be absent, got %v ok=%v", far, ok)
	}

	// Loose fallback requires the 重置 marker.
	loose, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"limit hit，窗口将于 2026-09-22 10:00:00 UTC+8 重置。"}`, now)
	if !ok || loose.IsZero() {
		t.Fatalf("loose fallback with 重置-free copy must not parse; got %v ok=%v", loose, ok)
	}

	// Unrelated future timestamps must NOT pin a model (no 重置 marker).
	junk, ok := parseRateLimitResetAtFrom(
		`{"code":6004,"msg":"trial ends 2026-10-01 00:00:00"}`, now)
	if ok || !junk.IsZero() {
		t.Fatalf("timestamp without reset marker must be ignored, got %v ok=%v", junk, ok)
	}

	// Empty payload.
	if _, ok := parseRateLimitResetAtFrom("", now); ok {
		t.Fatalf("empty payload must not parse")
	}
}

func TestModelRateLimitRegistryLifecycle(t *testing.T) {
	uid, model := "uid-registry-lifecycle", "deepseek-v4.1-flash"
	key := modelRateLimitKey(uid, model)

	// Miss before any note.
	if _, ok := wbModelRateLimits.until(key, time.Now()); ok {
		t.Fatalf("fresh key must miss")
	}

	// Note a future reset → hit with the same instant.
	future := time.Now().Add(30 * time.Minute)
	noteModelRateLimit(uid, model, future)
	got, ok := wbModelRateLimits.until(key, time.Now())
	if !ok || !got.Equal(future) {
		t.Fatalf("until = %v ok=%v, want %v", got, ok, future)
	}

	// A later 6004 with an earlier parsed reset must never shorten the hold.
	noteModelRateLimit(uid, model, time.Now().Add(5*time.Minute))
	got, ok = wbModelRateLimits.until(key, time.Now())
	if !ok || !got.Equal(future) {
		t.Fatalf("hold was shortened: got %v, want %v", got, future)
	}

	// Zero reset falls back to the default window.
	defUid, defModel := "uid-registry-lifecycle", "glm-5.2"
	noteModelRateLimit(defUid, defModel, time.Time{})
	got, ok = modelRateLimitedUntil(defUid, defModel)
	if !ok || got.Before(time.Now()) || got.After(time.Now().Add(modelRateLimitDefaultWindow+time.Second)) {
		t.Fatalf("default window = %v ok=%v", got, ok)
	}

	// Lazy expiry: a past entry is dropped on read.
	expired := modelRateLimitKey("uid-registry-expired", "kimi-k2.7")
	wbModelRateLimits.note(expired, time.Now().Add(-time.Minute), time.Now())
	if _, ok := wbModelRateLimits.until(expired, time.Now()); ok {
		t.Fatalf("expired entry must be dropped")
	}
	if _, present := wbModelRateLimits.entries[expired]; present {
		t.Fatalf("expired entry must be deleted from the map")
	}
}

func TestNoteModelRateLimitFromPayload(t *testing.T) {
	uid, model := "uid-hook", "deepseek-v4.1-flash"

	// Non-6004 payloads never touch the registry.
	noteModelRateLimitFromPayload(uid, model, http.StatusTooManyRequests, `{"code":11120,"msg":"x"}`)
	if _, ok := modelRateLimitedUntil(uid, model); ok {
		t.Fatalf("non-6004 payload must not register a window")
	}

	// 6004 with a reset ~1h out → the registry carries exactly that instant.
	reset := time.Now().Add(time.Hour)
	stamp := reset.In(wbCNZone).Format("2006-01-02 15:04:05")
	payload := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` + stamp + ` UTC+8 重置，您也可以切换其他模型继续使用。"}`
	noteModelRateLimitFromPayload(uid, model, http.StatusTooManyRequests, payload)
	got, ok := modelRateLimitedUntil(uid, model)
	if !ok {
		t.Fatalf("6004 payload must register a window")
	}
	if diff := got.Sub(reset.Truncate(time.Second)); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("registered reset = %v, want ≈ %v", got, reset)
	}

	// 6004 without a parsable reset → default window entry.
	uid2, model2 := "uid-hook", "hy3-preview"
	noteModelRateLimitFromPayload(uid2, model2, http.StatusTooManyRequests, `{"code":6004,"msg":"limit"}`)
	got, ok = modelRateLimitedUntil(uid2, model2)
	if !ok || got.IsZero() {
		t.Fatalf("6004 without reset must still register the default window")
	}
}

// TestUpstreamStatusError6004StaysRequestLevel pins the v0.9.32 carve-out:
// the 6004 model-scoped rejection must NOT ride the 429 credential quota
// path on any host version — it stays a plain status-0 error.
func TestUpstreamStatusError6004StaysRequestLevel(t *testing.T) {
	base := errors.New("translated")
	err := upstreamStatusError(http.StatusTooManyRequests, modelRateLimitSamplePayload, base)
	var se *statusError
	if errors.As(err, &se) {
		t.Fatalf("6004 must stay plain, got statusError{%d}", se.status)
	}
	if !errors.Is(err, base) {
		t.Fatalf("6004 plain error must wrap the translated base")
	}
	// Regression: a 429 carrying a DIFFERENT business code still wraps.
	other := upstreamStatusError(http.StatusTooManyRequests, `{"code":11120,"msg":"quota"}`, base)
	if !errors.As(other, &se) || se.status != http.StatusTooManyRequests {
		t.Fatalf("non-6004 429 must still wrap as statusError{429}")
	}
}

func TestTranslateChatUpstreamErrorFullModelScopedCopy(t *testing.T) {
	err := translateChatUpstreamErrorFull(http.StatusTooManyRequests, modelRateLimitSamplePayload, nil, nil)
	msg := err.Error()
	for _, want := range []string{"6004", "其他模型", "UTC+8", "2026-09-22 09:46:39"} {
		if !strings.Contains(msg, want) {
			t.Errorf("6004 copy must contain %q, got %s", want, msg)
		}
	}
	// The literal "429" must never appear: hosts lacking the envelope status
	// would re-read it from the message as credential quota.
	if strings.Contains(msg, "429") {
		t.Errorf("6004 copy must not carry the literal 429: %s", msg)
	}

	// No parsable reset → generic auto-recovery clause, still no "429".
	err = translateChatUpstreamErrorFull(http.StatusTooManyRequests, `{"code":6004,"msg":"limit"}`, nil, nil)
	msg = err.Error()
	if !strings.Contains(msg, "6004") || !strings.Contains(msg, "自动恢复") {
		t.Errorf("reset-less copy must fall back to auto-recovery wording: %s", msg)
	}
	if strings.Contains(msg, "429") {
		t.Errorf("reset-less copy must not carry the literal 429: %s", msg)
	}
}

func TestModelRateLimitErrorFastFail(t *testing.T) {
	uid, model := "uid-fastfail", "deepseek-v4.1-flash"
	if err := modelRateLimitError(uid, model); err != nil {
		t.Fatalf("no window registered yet, fast-fail must be nil, got %v", err)
	}
	noteModelRateLimit(uid, model, time.Now().Add(10*time.Minute))
	err := modelRateLimitError(uid, model)
	if err == nil {
		t.Fatalf("active window must produce a fast-fail error")
	}
	msg := err.Error()
	for _, want := range []string{"6004", "其他模型"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fast-fail copy must contain %q, got %s", want, msg)
		}
	}
	// Plain error: no StatusCode() implementation may leak onto the fast-fail.
	type statusCarrier interface{ StatusCode() int }
	if _, carries := any(err).(statusCarrier); carries {
		t.Fatalf("fast-fail error must not implement StatusCode()")
	}
	// A different model on the same credential is untouched.
	if err := modelRateLimitError(uid, "glm-5.2"); err != nil {
		t.Fatalf("sibling model must be unaffected, got %v", err)
	}
}
