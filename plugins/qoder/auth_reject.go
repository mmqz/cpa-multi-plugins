package main

// auth_reject.go — distinguish "upstream refused the credential" from
// "upstream had a bad moment". Adapted from bfSan/qoder-cpa-plugin f05e9e3.
//
// Why this exists (2026-10-02 field report, panel evidence): with upstream
// rejecting the billing surface (401 missing cookie header), every panel
// fetch failed — and the stale-while-error carryover in cachedAccountDetails
// kept rendering the PREVIOUS snapshot, so an account that never signed in
// showed 「今日已签到」 and a frozen credit balance. A stale snapshot must not
// survive a credential rejection: it is indistinguishable from a real
// check-in. Transient upstream failures (5xx, timeouts, parse errors) still
// carry over — a flaky network must not blank the panel.

import (
	"errors"
	"net/http"
	"strings"
)

// authRejectedError marks an upstream response that refuses the credential
// itself (401/403), as opposed to a transient failure or a business no-op.
type authRejectedError struct {
	status int
	err    error
}

func (e *authRejectedError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "credential rejected by upstream"
}

func (e *authRejectedError) Unwrap() error { return e.err }

// isAuthRejectedError reports whether err means "this credential is not
// accepted". It matches explicit status errors and the body shapes Qoder
// returns when the dt-/jt- token alone is no longer enough (observed live:
// /sash/api/v1/* answering 401 {"code":"UNAUTHORIZED",
// "message":"missing cookie header"}).
func isAuthRejectedError(err error) bool {
	if err == nil {
		return false
	}
	var rejected *authRejectedError
	if errors.As(err, &rejected) {
		return true
	}
	msg := err.Error()
	if status := statusFromErrorString(msg); status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	lower := strings.ToLower(msg)
	// Body-level markers that survive error wrapping.
	return strings.Contains(lower, "missing cookie header") ||
		strings.Contains(lower, "user not authenticated") ||
		strings.Contains(lower, `"code":"unauthorized"`) ||
		strings.Contains(lower, "unauthorized")
}

// statusFromErrorString extracts an "http <n>" marker left by the billing
// helpers, which format failures as `... http 401 body=...`.
func statusFromErrorString(msg string) int {
	const marker = "http "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len(marker):]
	digits := ""
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			digits += string(r)
			continue
		}
		break
	}
	switch digits {
	case "401":
		return http.StatusUnauthorized
	case "403":
		return http.StatusForbidden
	default:
		return 0
	}
}
