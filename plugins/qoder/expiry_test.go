package main

// expiry_test.go — pins the stored-expiry magnitude normalisation (0.8.42,
// adapted from bfSan f05e9e3). The stored expiresAt is seconds by contract;
// historical millisecond writes read back as 1970 and made every token look
// long expired, skewing refresh scheduling.

import (
        "testing"
        "time"
)

func TestTokenExpiryUnix(t *testing.T) {
        cases := []struct {
                name string
                in   int64
                want int64
        }{
                {"zero stays zero", 0, 0},
                {"negative stays zero", -5, 0},
                {"plausible seconds pass through", 1792000000, 1792000000}, // 2026-10, seconds
                {"milliseconds convert down", 1792000000000, 1792000000},
                {"pre-2020 seconds treated as ms", 1000000000, 0}, // 2001 in seconds → below secondsFloor; *1000 < millisFloor? no → scaled path
                {"bogus tiny value is zero", 42, 0},
        }
        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        if got := tokenExpiryUnix(tc.in); got != tc.want {
                                t.Fatalf("tokenExpiryUnix(%d) = %d, want %d", tc.in, got, tc.want)
                        }
                })
        }
}

func TestTokenExpiryUnixAmbiguousBandFailsOpen(t *testing.T) {
        // 1000000000 seconds is 2001 — below secondsFloor and its ms scaling is
        // below millisFloor: bogus either way, normalises to zero.
        if got := tokenExpiryUnix(1000000000); got != 0 {
                t.Fatalf("tokenExpiryUnix(1e9 s, pre-2020) = %d, want 0", got)
        }
        // The ambiguous band [secondsFloor, millisFloor) (≈2020s in seconds but
        // also ≈1970s+ in ms) cannot be resolved; reading it as seconds fails
        // OPEN — "never expires" is the safe direction (no refresh storm, no
        // false death). Real historical millisecond writes (2024+) sit above
        // millisFloor and convert down correctly.
        if got := tokenExpiryUnix(1420070400000); got != 1420070400000 {
                t.Fatalf("tokenExpiryUnix(2015 ms) = %d, want passthrough (fail-open)", got)
        }
}

func TestTokenExpiredUnknownIsNotExpired(t *testing.T) {
        now := time.Unix(1792000000, 0)
        if tokenExpired(0, now) {
                t.Fatal("zero expiry must not count as expired — that would refresh-storm credentials with a missing timestamp")
        }
        if tokenExpired(now.Unix()+3600, now) {
                t.Fatal("future expiry must not count as expired")
        }
        if !tokenExpired(now.Unix()-3600, now) {
                t.Fatal("past expiry must count as expired")
        }
        // A millisecond value in the past must still read as expired after
        // normalisation (1792000000000 ms = 1792000000 s = now — boundary; use
        // clearly-past ms).
        if !tokenExpired(1791000000000, now) {
                t.Fatal("millisecond-encoded past expiry must count as expired")
        }
}

func TestPreserveExpiryNormalisesBothSides(t *testing.T) {
        secs := int64(1792000000)
        ms := secs * 1000
        // New value present: normalised, never passed through raw.
        if got := preserveExpiry(ms, secs); got != secs {
                t.Fatalf("preserveExpiry(ms, secs) = %d, want %d (ms must convert down)", got, secs)
        }
        // New value missing: previous value normalised too.
        if got := preserveExpiry(0, ms); got != secs {
                t.Fatalf("preserveExpiry(0, ms) = %d, want %d (stale ms snapshot must not survive)", got, secs)
        }
        if got := preserveExpiry(0, secs); got != secs {
                t.Fatalf("preserveExpiry(0, secs) = %d, want %d", got, secs)
        }
}
