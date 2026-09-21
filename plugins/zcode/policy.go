// policy.go is the pure decision layer for quota-driven lifecycle actions and
// the error-shaping helpers the executor uses. No I/O happens here.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// lifecycleAction is the policy decision for one account.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleDisable
)

func (a lifecycleAction) String() string {
	switch a {
	case lifecycleDisable:
		return "disable"
	default:
		return "none"
	}
}

// lifecycleAuto gates automatic disable/reenable. Default true.
var (
	lifecycleAuto   = true
	lifecycleAutoMu sync.RWMutex
)

func lifecycleEnabled() bool {
	lifecycleAutoMu.RLock()
	defer lifecycleAutoMu.RUnlock()
	return lifecycleAuto
}

// shouldActOnQuota is true only when quota is *known* exhausted.
// nil / empty (no packages, no used) is unknown → false.
func shouldActOnQuota(cr *creditsSummary) bool {
	return isCreditsExhausted(cr)
}

// hardQuotaMarkers are case-insensitive substrings in upstream error bodies.
var hardQuotaMarkers = []string{
	"insufficient quota", "quota exceeded", "quota exhaust",
	"out of quota", "no quota", "payment required",
	"insufficient balance", "arrears",
	"1113", // BigModel: insufficient balance/欠费 business code
	"额度不足", "余额不足", "配额不足", "配额用尽", "欠费",
}

// isHardQuotaError reports business "out of quota" style failures.
// 402 is treated as payment/quota. Pure 429 is not hard unless body has quota markers.
func isHardQuotaError(status int, body string) bool {
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardQuotaMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	// Chinese markers may not lower-map usefully; also scan raw.
	for _, m := range hardQuotaMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-quota semantics.
func isSoftRateLimit(status int, body string) bool {
	if isHardQuotaError(status, body) {
		return false
	}
	if status == 429 {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining quota.
// Missing quota data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0, size>0, or packages present). Pure zero with no packages = no data.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}

// lifecycleActionFor chooses disable/none from quota state.
// Disable (not delete) so a top-up / plan refresh restores the account
// without forcing the user to re-login.
func lifecycleActionFor(provider string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnQuota(cr) {
		return lifecycleNone
	}
	return lifecycleDisable
}

// chatSizeMarkers are substrings (case-insensitive) of upstream rejections
// caused by oversized input/context. These are REQUEST-level problems: they
// must never read as account trouble (quota) and deserve actionable copy.
var chatSizeMarkers = []string{
	"too long", "too large", "context length", "context_length", "context too",
	"max input", "input token", "token limit", "prompt is too long",
	"内容过长", "输入过长", "上下文过长", "上下文太长", "超过最大",
}

// chatInputTooLarge reports whether an upstream chat rejection was caused by
// oversized input: explicit HTTP 413 (without quota semantics), or a body
// naming a size/context limit.
func chatInputTooLarge(status int, body string) bool {
	if status == http.StatusRequestEntityTooLarge {
		return !isHardQuotaError(status, body)
	}
	lower := strings.ToLower(body)
	for _, m := range chatSizeMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// statusError carries an upstream HTTP status across the RPC boundary. The
// host's decodeEnvelopeResult rebuilds it as rpcError (via the envelope error
// http_status field, see errorEnvelopeFor) whose StatusCode() drives
// MarkResult's per-status cooldown: 402 -> 30 min, 429 -> escalating quota
// backoff (credential-scoped across models), 401 -> 30 min.
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
// request-level shapes (413/输入过大 etc.) stay status-less — the host's
// 1-minute transient default applies.
func upstreamStatusError(status int, err error) error {
	if status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusTooManyRequests {
		return &statusError{status: status, err: err}
	}
	return err
}

// emptyStreamError renders an upstream "stream closed before first payload"
// failure.
//
// The wording is load-bearing twice. Plugin-side, cooldown.go matches the
// "empty_stream" prefix to cool the (credential, model) pair — single-model
// flakiness must not take the whole account down. Host-side, CPA classifies
// stream errors from the message text that survives the RPC boundary, and
// isConnectionLifecycleMessage treats "unexpected eof" phrasing as a
// transport-lifecycle event (message path only applies when no HTTP status is
// attached): the credential keeps its healthy status instead of being cooled
// for one flaky model.
func emptyStreamError() error {
	return fmt.Errorf("empty_stream: zcode upstream closed before a completion payload (unexpected EOF)")
}

// upstreamReadError renders a mid-stream read failure with the same
// transport-lifecycle classification as emptyStreamError.
func upstreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error (unexpected EOF): %w", err)
}

// chatUpstreamError renders an upstream chat failure for the client, adding
// actionable copy when the rejection was caused by oversized input so users
// don't mistake it for an account/quota problem.
func chatUpstreamError(status int, body string) error {
	trimmed := truncateRedacted(body, 200)
	if chatInputTooLarge(status, body) {
		return fmt.Errorf("输入过大被上游拒绝（请求级问题，与账号无关）：请压缩上下文/清理会话后重试 — upstream %d: %s", status, trimmed)
	}
	return fmt.Errorf("upstream %d: %s", status, trimmed)
}

// notePrefix renders the provider/disabled head of an auth-card note, without
// the credit segment. Kept separate so syncAuthNote can rebuild a note while
// preserving a previously known credit segment.
func notePrefix(sa *storedAuth, disabled bool) string {
	prov := strings.ToUpper(authProviderFor(sa))
	parts := []string{prov}
	if plan := authPlanFor(sa); plan == planStart {
		parts = append(parts, "START")
	}
	if disabled {
		parts = append(parts, "已禁用")
	}
	return strings.Join(parts, " · ")
}

// creditSegmentFromNote extracts the credit segment of an existing auth note
// (everything after the provider / disabled prefix). Returns "" when the note
// carries no usable credit information, so callers never resurrect "积分未知".
func creditSegmentFromNote(note string) string {
	parts := strings.Split(note, " · ")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "ZAI" || part == "BIGMODEL" || part == "START" || part == "已禁用" {
			continue
		}
		segments = append(segments, part)
	}
	seg := strings.Join(segments, " · ")
	if seg == "" || strings.HasPrefix(seg, "积分未知") {
		return ""
	}
	return seg
}

// displayNote builds a one-line note for CPAMP Auth cards.
//
// cr == nil means "quota unknown right now" (startup, a lazy panel refresh,
// or a failed billing call). displayNote falls back to the placeholder because
// it has no disk access; callers that can read the previous note should prefer
// displayNoteWithPrev so a restart or transient billing error cannot regress a
// card that already shows live quota.
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	return displayNoteWithPrev(sa, cr, disabled, "")
}

// displayNoteWithPrev is displayNote plus a previously known credit segment.
// prev is ignored whenever cr carries fresh data.
func displayNoteWithPrev(sa *storedAuth, cr *creditsSummary, disabled bool, prev string) string {
	parts := []string{notePrefix(sa, disabled)}
	switch {
	case cr == nil:
		if seg := creditSegmentFromNote(prev); seg != "" {
			parts = append(parts, seg)
		} else {
			parts = append(parts, "积分未知")
		}
	case isCreditsExhausted(cr):
		parts = append(parts, fmt.Sprintf("耗尽 · 余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
	default:
		// Show remain as primary (what you can still spend). Used is real spend.
		if cr.TotalSize > 0 {
			parts = append(parts, fmt.Sprintf("余%d 已用%d 池%d", cr.TotalRemain, cr.TotalUsed, cr.TotalSize))
		} else {
			parts = append(parts, fmt.Sprintf("余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
		}
	}
	note := strings.Join(parts, " · ")
	if len(note) > 80 {
		note = note[:77] + "..."
	}
	return note
}

// labelForAuth tags the host label with the provider.
func labelForAuth(sa *storedAuth) string {
	base := "ZCode"
	prov := "ZAI"
	if sa != nil {
		if strings.TrimSpace(sa.Account.Nickname) != "" {
			base = strings.TrimSpace(sa.Account.Nickname)
		}
		prov = strings.ToUpper(authProviderFor(sa))
	}
	return base + " [" + prov + "]"
}
