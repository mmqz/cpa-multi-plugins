// model_ratelimit.go — per-(credential, model) upstream rate-limit registry.
//
// v0.9.32: the WorkBuddy CN gateway answers model-scoped frequency limits
// with HTTP 429 + business code 6004 and says so in the message:
//
//	{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8
//	 重置，您也可以切换其他模型继续使用。","requestId":"..."}
//
// "您也可以切换其他模型继续使用" is upstream declaring the limit MODEL-scoped —
// the account's other models keep working. Attributing that 429 to the
// credential (the pre-0.9.32 behavior) let one throttled model drive the
// host's escalating quota backoff across every model on the credential and
// 503 them all — the exact opposite of what upstream advises.
//
// This file owns the whole 6004 concern:
//  1. isModelScopedRateLimit — the 429+6004 classifier (JSON envelope first,
//     substring fallback for non-JSON envelope variants).
//  2. parseRateLimitResetAt — the "将在 ... UTC+8 重置" reset parser, so the
//     fast-fail window honors what upstream actually declared instead of a
//     guessed backoff curve.
//  3. modelScopedRateLimitCopy — the bilingual user copy. Deliberately free
//     of the literal "429": on some host versions a plain error whose message
//     carries an HTTP status can still be reclassified by message scanning.
//  4. An in-memory registry keyed (credential uid, upstream model): the first
//     6004 records the declared reset; further requests for that pair fail
//     FAST (no upstream call) until the reset instant. Other model pairs are
//     untouched. Errors stay plain (no StatusCode) on every path so no host
//     version ever reads them as credential-level 429 quota.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// modelRateLimitCode is the WorkBuddy business code for model-scoped
	// frequency limits (carried on an HTTP 429).
	modelRateLimitCode = 6004
	// modelRateLimitDefaultWindow covers envelopes whose message carries no
	// parsable reset: short enough that the next probe re-checks upstream
	// quickly, long enough to stop a retry storm from hammering the gateway.
	modelRateLimitDefaultWindow = time.Minute
	// modelRateLimitMaxHorizon caps a parsed reset so a nonsense timestamp
	// can never pin a model for days.
	modelRateLimitMaxHorizon = 24 * time.Hour
	// modelRateLimitMaxEntries bounds the registry. Resets are horizon-capped
	// so entries expire on their own; the sweep on note only guards a
	// pathological burst of (uid, model) pairs.
	modelRateLimitMaxEntries = 512
)

// wbCNZone is the fixed UTC+8 offset the upstream reset copy carries. The
// gateway phrases resets in Beijing time regardless of host timezone, so the
// copy renders them back in the same framing.
var wbCNZone = time.FixedZone("UTC+8", 8*60*60)

// modelRateLimitResetRe matches the authoritative upstream copy
// "将在 2026-09-22 09:46:39 UTC+8 重置".
var modelRateLimitResetRe = regexp.MustCompile(
	`将在\s*(\d{4}-\d{2}-\d{2})[ ](\d{2}:\d{2}:\d{2})\s*UTC\+8`)

// modelRateLimitLooseRe is the fallback timestamp shape. Only consulted when
// the payload also carries the 重置 (reset) marker, so unrelated body
// timestamps (plan expiry, requestId-like blobs) can never pin a model.
var modelRateLimitLooseRe = regexp.MustCompile(
	`(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})`)

// isModelScopedRateLimit reports an upstream model-scoped frequency-limit
// rejection: HTTP 429 carrying business code 6004. The JSON path is
// authoritative; the substring path catches envelope variants where the body
// is not the plain business JSON but still carries the same markers.
func isModelScopedRateLimit(statusCode int, payload string) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	var shape upstreamErrorShape
	if err := json.Unmarshal([]byte(payload), &shape); err == nil {
		return shape.Code == modelRateLimitCode
	}
	low := strings.ReplaceAll(payload, " ", "")
	return strings.Contains(low, `"code":6004`) || strings.Contains(low, `"code":"6004"`)
}

// parseRateLimitResetAt extracts the reset instant upstream declared for a
// 6004 rejection, interpreted in the gateway's fixed UTC+8 framing. A past or
// beyond-horizon instant is treated as absent (the caller falls back to the
// default window). now is injected for testability.
func parseRateLimitResetAtFrom(payload string, now time.Time) (time.Time, bool) {
	if payload == "" {
		return time.Time{}, false
	}
	raw := modelRateLimitResetRe.FindStringSubmatch(payload)
	if raw == nil && strings.Contains(payload, "重置") {
		raw = modelRateLimitLooseRe.FindStringSubmatch(payload)
	}
	if raw == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", raw[1]+" "+raw[2], wbCNZone)
	if err != nil {
		return time.Time{}, false
	}
	if !t.After(now) || now.Add(modelRateLimitMaxHorizon).Before(t) {
		return time.Time{}, false
	}
	return t, true
}

// parseRateLimitResetAt is the production entry point (real clock).
func parseRateLimitResetAt(payload string) (time.Time, bool) {
	return parseRateLimitResetAtFrom(payload, time.Now())
}

// renderModelRateLimitReset renders a reset instant in the upstream's own
// UTC+8 framing so user copy matches what the gateway told them.
func renderModelRateLimitReset(at time.Time) string {
	return at.In(wbCNZone).Format("2006-01-02 15:04:05 UTC+8")
}

// modelScopedRateLimitCopy renders the bilingual 6004 copy. resetKnown=false
// (unparsable reset) swaps the exact instant for a generic auto-recovery
// clause. The text must never contain "429" or hard-credit markers — see the
// file comment.
func modelScopedRateLimitCopy(resetAt time.Time, resetKnown bool) string {
	var zh, en string
	if resetKnown {
		zh = modelScopedRateLimitClause(resetAt)
		en = modelScopedRateLimitClauseEN(resetAt)
	} else {
		zh = "稍后自动恢复，届时可重试"
		en = "recovers automatically shortly; retry then"
	}
	return fmt.Sprintf(
		"上游模型级频率限制（code 6004）——仅当前模型受限，同一凭证的其他模型不受影响，%s；上游建议期间切换其他模型继续使用。"+
			" // Upstream model-level rate limit (code 6004) — only this model is throttled, other models on the same credential keep working, %s; upstream suggests switching to another model meanwhile.",
		zh, en)
}

// -----------------------------------------------------------------------------
// registry

type modelRateLimitRegistry struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

// wbModelRateLimits is the process-wide registry. In-memory only: resets are
// horizon-capped and re-armed by the next 6004 after a restart.
var wbModelRateLimits = &modelRateLimitRegistry{entries: map[string]time.Time{}}

func modelRateLimitKey(uid, model string) string {
	return strings.TrimSpace(uid) + "\x1f" + strings.ToLower(strings.TrimSpace(model))
}

func (r *modelRateLimitRegistry) note(key string, resetAt, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) >= modelRateLimitMaxEntries {
		for k, at := range r.entries {
			if !at.After(now) {
				delete(r.entries, k)
			}
		}
	}
	// Never shorten an active window: a second 6004 with a smaller parsed
	// reset must not release the earlier (larger) hold.
	if existing, ok := r.entries[key]; ok && existing.After(resetAt) {
		return
	}
	r.entries[key] = resetAt
}

func (r *modelRateLimitRegistry) until(key string, now time.Time) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.entries[key]
	if !ok {
		return time.Time{}, false
	}
	if !at.After(now) {
		delete(r.entries, key) // lazy expiry
		return time.Time{}, false
	}
	return at, true
}

// noteModelRateLimit records (or extends) the hold for one (credential,
// upstream model) pair. A zero/past resetAt falls back to the default window
// so an unparseable envelope still gets short storm protection.
func noteModelRateLimit(uid, model string, resetAt time.Time) {
	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(modelRateLimitDefaultWindow)
	}
	wbModelRateLimits.note(modelRateLimitKey(uid, model), resetAt, now)
}

// noteModelRateLimitFromPayload is the chat-path hook: no-op unless the
// upstream response is the 6004 model-scoped rejection.
func noteModelRateLimitFromPayload(uid, model string, statusCode int, payload string) {
	if !isModelScopedRateLimit(statusCode, payload) {
		return
	}
	reset, _ := parseRateLimitResetAt(payload)
	noteModelRateLimit(uid, model, reset)
}

// modelRateLimitedUntil reports the active reset instant for the pair, if any.
func modelRateLimitedUntil(uid, model string) (time.Time, bool) {
	return wbModelRateLimits.until(modelRateLimitKey(uid, model), time.Now())
}

// modelRateLimitError renders the fast-fail error for a pair with an active
// window, or nil. Deliberately plain (no StatusCode() impl): every host
// version must treat it as a transient request-level failure, never as
// credential-level 429 quota.
func modelRateLimitError(uid, model string) error {
	at, ok := modelRateLimitedUntil(uid, model)
	if !ok {
		return nil
	}
	return fmt.Errorf(
		"该模型处于上游模型级限流中（code 6004）——仅此模型受限，同一凭证的其他模型不受影响，%s；上游建议期间切换其他模型继续使用。"+
			" // This model is under an upstream model-level rate limit (code 6004) — other models on the same credential are unaffected, %s; upstream suggests switching to another model meanwhile.",
		modelScopedRateLimitClause(at), modelScopedRateLimitClauseEN(at))
}

// modelScopedRateLimitClause / ClauseEN render the reset clause shared by the
// fast-fail and post-6004 copies.
func modelScopedRateLimitClause(at time.Time) string {
	return fmt.Sprintf("将于 %s 重置，到期自动恢复，无需更换凭证", renderModelRateLimitReset(at))
}

func modelScopedRateLimitClauseEN(at time.Time) string {
	return fmt.Sprintf("resets at %s and recovers automatically; keep the credential", renderModelRateLimitReset(at))
}
