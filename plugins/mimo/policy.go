// policy.go renders upstream failures for the host's classification layer
// (status-attributed errors for real credential faults; status-less for
// request-level ones), plus the shared redaction and small-string helpers.
package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// statusError attaches the upstream HTTP status so the host's cooldown layer
// can attribute real account-level faults.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string   { return e.err.Error() }
func (e *statusError) StatusCode() int { return e.status }
func (e *statusError) Unwrap() error   { return e.err }

// upstreamStatusError wraps a translated upstream chat failure with the HTTP
// status the host cooldown layer should attribute to the credential.
// Only unambiguous account-level statuses pass (401/402/429). 403 and the
// request-level shapes stay status-less — the host's 1-minute transient
// default applies.
func upstreamStatusError(status int, err error) error {
	if status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusTooManyRequests {
		return &statusError{status: status, err: err}
	}
	return err
}

// emptyStreamError renders an upstream "stream closed before first payload"
// failure. The wording is load-bearing host-side too: CPA classifies stream
// errors from the message text that survives the RPC boundary, and
// isConnectionLifecycleMessage treats "unexpected eof" phrasing as a
// transport-lifecycle event, so the credential keeps its healthy status
// instead of being cooled for one flaky model.
func emptyStreamError() error {
	return fmt.Errorf("empty_stream: mimo upstream closed before a completion payload (unexpected EOF)")
}

// upstreamReadError renders a mid-stream read failure with the same
// transport-lifecycle classification as emptyStreamError.
func upstreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error (unexpected EOF): %w", err)
}

// chatUpstreamError renders an upstream chat failure for the client,
// redacted and bounded.
func chatUpstreamError(status int, body string) error {
	return fmt.Errorf("upstream %d: %s", status, truncateRedacted(body, 200))
}

// -----------------------------------------------------------------------------
// Redaction
// -----------------------------------------------------------------------------

var (
	redactREBearer = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-+/=]{12,}`)
	redactREJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)
	// The kv alternation must cover the bare key=value shapes an upstream
	// error body may echo, not just cookie-header fragments: the xiaomi
	// credential set (serviceToken/passToken/cUserId), the sk lane's key, and
	// the region-scoped ticket rows (<sid>_ph/<sid>_slh, e.g. mimosgp_slh).
	// \b keeps the short `sk` name from hitting unrelated shapes like
	// task=123...; the value class gains % so URL-encoded tickets redact whole,
	// and : because the real passToken wire form is `V1:<base64>` — without it
	// the value run dies at the colon and nothing matches (deep-audit P2 #2,
	// 2026-09-25; the V1: miss was the reviewer's re-check find, same
	// fixture-vs-real-shape class as the 0.2.2 nonce bug).
	redactRETokenKV = regexp.MustCompile(`(?i)((?:access_?token|refresh_?token|id_?token|service_?token|pass_?token|c_?user_?id|\bsk|[a-z0-9]+_(?:ph|slh))["']?\s*[=:]\s*["']?)([A-Za-z0-9._\-+/=%:]{12,})`)
	// redactRECookie catches Set-Cookie/Cookie header fragments that may ride
	// an upstream error body — the cookie lane's credential material.
	redactRECookie = regexp.MustCompile(`(?i)((?:set-)?cookie\s*[:=]\s*)[^;\r\n]{8,}`)
)

// redactSecrets strips bearer tokens / JWT-like blobs / bare credential
// key-value pairs / cookie fragments from error bodies before they reach logs
// or clients. Every upstream error string this plugin surfaces must route
// through redactSecrets (or truncateRedacted).
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = redactREBearer.ReplaceAllString(s, "Bearer ***")
	s = redactREJWT.ReplaceAllString(s, "***jwt***")
	s = redactRETokenKV.ReplaceAllString(s, "${1}***")
	s = redactRECookie.ReplaceAllString(s, "${1}***")
	return s
}

// truncateRedacted redacts secrets then truncates — use for any error body
// returned to clients / logs.
func truncateRedacted(s string, n int) string {
	return truncate(redactSecrets(s), n)
}

// truncate cuts s to at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// sha256hex8 returns the first 8 hex chars of sha256(s) — used to derive a
// stable pseudo-UID when no explicit account id exists.
func sha256hex8(s string) string {
	return sha256Sum(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
