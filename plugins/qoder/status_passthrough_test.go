package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestErrorEnvelopeForCarriesStatus(t *testing.T) {
	raw := errorEnvelopeFor(&statusError{status: http.StatusTooManyRequests, err: errors.New("upstream 429: quota")})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got ok=%v", env.OK)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", env.Error.HTTPStatus)
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

// TestUpstreamStatusErrorPolicy locks the 0.8.13 matrix: only 401/402/429
// pass their status to the host cooldown layer; everything else (413/input
// too large, 403, 5xx) stays status-less — request-level or unevidenced
// shapes must not cool the credential beyond the historical default.
func TestUpstreamStatusErrorPolicy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"402 payment", 402, 402},
		{"401 dead token", 401, 401},
		{"429 free quota drained", 429, 429},
		{"413 input too large", 413, 0},
		{"403 unknown shape", 403, 0},
		{"400 business", 400, 0},
		{"500 server", 500, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := errors.New("translated")
			err := upstreamStatusError(tc.status, base)
			var se *statusError
			if tc.want == 0 {
				if errors.As(err, &se) {
					t.Fatalf("status %d: expected plain error, got statusError{%d}", tc.status, se.status)
				}
				if !errors.Is(err, base) {
					t.Fatalf("plain error must wrap base")
				}
				return
			}
			if !errors.As(err, &se) {
				t.Fatalf("status %d: expected statusError", tc.status)
			}
			if se.status != tc.want {
				t.Fatalf("status = %d, want %d", se.status, tc.want)
			}
		})
	}
}

// TestNextCheckinTimeSkipsBeforeOpeningHour guards the 10:00 opening-hour
// fix: from 09:59 the next auto tick must be 10:00, never the stale 09:00.
func TestNextCheckinTimeSkipsBeforeOpeningHour(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 20, 9, 59, 0, 0, loc)
	next := nextCheckinTime(now)
	if next.Hour() != 10 || next.Day() != now.Day() {
		t.Fatalf("next = %s, want today 10:00", next)
	}
}
