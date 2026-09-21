// policy.go is the pure decision layer for credit-driven lifecycle actions:
// given an account's region and current credits, decide whether to disable
// (CN), delete (Global), re-enable (CN after check-in restores credits), or
// leave it alone. No I/O happens here — reconcileOneAccount consumes these
// decisions and applies them via the lifecycle.go authfile helpers.
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
	lifecycleDelete
	lifecycleReenable
)

func (a lifecycleAction) String() string {
	switch a {
	case lifecycleDisable:
		return "disable"
	case lifecycleDelete:
		return "delete"
	case lifecycleReenable:
		return "reenable"
	default:
		return "none"
	}
}

// lifecycleAuto gates automatic disable/delete/reenable. Default true.
var (
	lifecycleAuto   = true
	lifecycleAutoMu sync.RWMutex
)

func lifecycleEnabled() bool {
	lifecycleAutoMu.RLock()
	defer lifecycleAutoMu.RUnlock()
	return lifecycleAuto
}

// shouldActOnCredits is true only when credits are *known* exhausted.
// nil / empty (no packages, no used) is unknown → false.
func shouldActOnCredits(cr *creditsSummary) bool {
	return isCreditsExhausted(cr)
}

// hardCreditMarkers are case-insensitive substrings in upstream error bodies.
var hardCreditMarkers = []string{
	"insufficient credit",
	"insufficient credits",
	"no credit",
	"no credits",
	"credit exhausted",
	"credits exhausted",
	"out of credit",
	"out of credits",
	"quota exceeded",
	"quota exhaust",
	"payment required",
	"积分不足",
	"额度不足",
	"余额不足",
	"积分用完",
	"额度用尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError reports business "out of credits" style failures.
// 402 is treated as payment/credit. Pure 429 is not hard unless body has credit markers.
func isHardCreditError(status int, body string) bool {
	if status == http.StatusTooManyRequests {
		// 429 precedes the balance word list (same reordering the upstream
		// 2api sync shipped): a 429 is throttling even when the body says
		// "quota"/"额度" — model-level rate limits use that wording while the
		// ACCOUNT still has credits. Classifying it as hard credit used to
		// trigger the disable/delete lifecycle off a soft rate-limit body.
		// Genuine exhaustion is still caught by the periodic credits reconcile.
		return false
	}
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	// Chinese markers may not lower-map usefully; also scan raw.
	for _, m := range hardCreditMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-credit semantics.
func isSoftRateLimit(status int, body string) bool {
	if isHardCreditError(status, body) {
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

// lifecycleActionFor chooses disable/delete/none from region + credits.
// Does not consider reenable (that needs disabled flag).
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	if region == "global" {
		return lifecycleDelete
	}
	return lifecycleDisable
}

// shouldReenableCN is true when a CN account is disabled but now has credits.
func shouldReenableCN(disabled bool, cr *creditsSummary) bool {
	if !disabled {
		return false
	}
	if cr == nil {
		return false
	}
	if isCreditsExhausted(cr) {
		return false
	}
	// Known positive remain, or non-exhausted with packages still having room.
	return cr.TotalRemain > 0
}

// emptyStreamError renders an upstream "stream closed before first payload"
// failure.
//
// The wording is load-bearing. Plugin-side tests and the aggregate guards key
// on the "empty_stream" prefix; host-side, CPA classifies stream errors from
// the message text that survives the RPC boundary, and
// isConnectionLifecycleMessage treats "unexpected eof" phrasing as a
// transport-lifecycle event (message path only applies when no HTTP status is
// attached): the credential keeps its healthy status instead of being cooled
// for one flaky model.
func emptyStreamError() error {
	return fmt.Errorf("empty_stream: workbuddy upstream closed before a completion payload (unexpected EOF)")
}

// upstreamReadError renders a mid-stream read failure with the same
// transport-lifecycle classification as emptyStreamError.
func upstreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error (unexpected EOF): %w", err)
}

// notePrefix renders the region/disabled head of an auth-card note, without
// the credit segment. Kept separate so syncAuthNote can rebuild a note while
// preserving a previously known credit segment.
func notePrefix(sa *storedAuth, disabled bool) string {
	region := "CN"
	switch accountRegion(sa) {
	case "intl":
		region = "INTL"
	case "global":
		region = "Global"
	}
	parts := []string{region}
	if disabled {
		parts = append(parts, "已禁用")
	}
	return strings.Join(parts, " · ")
}

// creditSegmentFromNote extracts the credit segment of an existing auth note
// (everything after the region / disabled prefix). Returns "" when the note
// carries no usable credit information, so callers never resurrect "积分未知".
func creditSegmentFromNote(note string) string {
	parts := strings.Split(note, " · ")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "CN" || part == "INTL" || part == "Global" || part == "已禁用" {
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
// cr == nil means "credits unknown right now" (startup, a lazy panel refresh,
// or a failed billing call). displayNote falls back to the placeholder because
// it has no disk access; callers that can read the previous note should prefer
// displayNoteWithPrev so a restart or transient billing error cannot regress a
// card that already shows live credits.
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
		// Show remain as primary (what you can still spend). Used is real cycle spend.
		// Size (capacity) grows with check-in packs — do not treat size↑ as usage↓.
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

// labelForAuth adds [CN]/[Global] for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "WorkBuddy"
	if sa != nil && strings.TrimSpace(sa.Account.Nickname) != "" {
		base = strings.TrimSpace(sa.Account.Nickname)
	}
	tag := "CN"
	switch accountRegion(sa) {
	case "intl":
		tag = "Intl"
	case "global":
		tag = "Global"
	}
	return base + " [" + tag + "]"
}
