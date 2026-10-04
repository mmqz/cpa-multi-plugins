// usage.go — quota fetch + auth-note sync (v0.2.11).
//
// Two user-plane quota surfaces, wire-verified 200 on the real machine
// (user-supplied captures, 2026-09; the desktop's internal
// mimo:getUserUsage / mimo:getUserSubscription RPCs hit the same endpoints):
//
//	GET {base}/user/usage
//	  → {"code":0,"data":{"percent":63.4,"resetAt":1790250598,
//	     "resetDate":"2026-09-24"},"message":"success"}
//	GET {base}/user/xiaomi/subscription/self
//	  → {"code":0,"data":{"current":{bizNo,planCode,title,planTier,status,
//	     source,renewalMode,startTime,endTime,nextResetTime,percent},
//	     "subscriptions":[…]}}
//
// Wire semantics (bundle cross-check, Hie() parser):
//   - percent is the REMAINING quota percentage — user-corrected 2026-09;
//     the bundle only type-checks the number and never labels it.
//   - resetDate === null ⇒ no active subscription: the bundle throws
//     "no-data / usage has no active subscription" in that state. We render
//     「无有效订阅」 instead of guessing a percent.
//
// Auth: both lanes work on both endpoints (measured). Cookie lane rides the
// desktop engine-lane fingerprint headers (applyCookieLaneHeaders), sk lane
// rides Bearer + mimocode-cli (applyKeyLaneHeaders). /user/available_models
// is sk-only and /user/xiaomi/me is RETIRED (302) — neither is used here.
//
// Base: the usage surface lives on the region servers (same table as chat);
// the bundle's intl table pins sgp as its default. The cn host's usage
// surface is unverified — if it answers non-200 the note is left untouched
// (prev preserved), never fabricated.
//
// Privacy: usage-display calls the desktop itself makes — not the audit
// plane (/audit/*), not telemetry; docs/MIMO_PRIVACY.md §7 boundary intact.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// usageNoteInterval paces the background quota/note refresh. The usage
	// endpoints are display-plane (desktop polls them per UI open); 30 min
	// keeps the credential cards fresh without idle chatter.
	usageNoteInterval = 30 * time.Minute
	// usageNoteStartDelay lets the first adoption/exchange settle before the
	// first sweep (register → adopt → mint is already in flight).
	usageNoteStartDelay = 45 * time.Second
	// usageSweepSpacing spaces per-credential fetches within one sweep.
	usageSweepSpacing = 2 * time.Second
)

// errNoActiveSubscription mirrors the bundle's resetDate===null state.
var errNoActiveSubscription = errors.New("usage has no active subscription")

// mimoUsageData is the /user/usage data object.
type mimoUsageData struct {
	Percent   *float64 `json:"percent"`   // REMAINING percent (absent on odd payloads)
	ResetAt   int64    `json:"resetAt"`   // unix seconds (resetDate is the display source)
	ResetDate *string  `json:"resetDate"` // "2026-09-24"; null ⇒ no active subscription
}

type mimoUsageEnvelope struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    *mimoUsageData `json:"data"`
}

// mimoSubscription is the slice of subscription/current the note uses.
// Time fields (startTime/endTime/nextResetTime) are deliberately not
// modeled — /user/usage already carries the reset date and their wire types
// were not part of the verified capture.
type mimoSubscription struct {
	Title    string   `json:"title"`
	PlanCode string   `json:"planCode"`
	PlanTier string   `json:"planTier"`
	Status   string   `json:"status"` // ACTIVE on a live plan
	Percent  *float64 `json:"percent"`
}

type mimoSubscriptionEnvelope struct {
	Code int `json:"code"`
	Data *struct {
		Current *mimoSubscription `json:"current"`
	} `json:"data"`
}

// usageBaseFor resolves the /user/* base for one credential:
//
//   - cookie lane → the credential's chat base (regionBaseFor: pinned region
//     > config > cn) — the session's actual region, same host the engine lane
//     talks to;
//   - sk lane → the bundle's intl usage default (sgp), unless the config
//     region names a specific one. The sk itself is region-free.
func usageBaseFor(sa *storedAuth) string {
	if authLaneFor(sa) == laneCookie {
		return regionBaseFor(sa)
	}
	if mode := strings.ToLower(strings.TrimSpace(loadedRegionMode())); mode != "" {
		if base, ok := regionBases[mode]; ok {
			return base
		}
	}
	return regionBases["sgp"]
}

// applyLaneHeadersForLane applies the lane's official fingerprint to a usage
// request — the exact header builders the chat path uses, so usage traffic
// is wire-indistinguishable from the desktop/CLI's own.
func applyLaneHeadersForLane(req *http.Request, sa *storedAuth) {
	if authLaneFor(sa) == laneCookie {
		applyCookieLaneHeaders(req, sa, "")
		return
	}
	applyKeyLaneHeaders(req, sa, "")
}

// fetchUsageOnce performs one /user/usage GET. Returns the parsed data plus
// the HTTP status so the ladder can classify session bounces. A code!=0
// envelope (e.g. the bundle's "no-data" family) is an error carrying the
// upstream message.
func fetchUsageOnce(sa *storedAuth) (*mimoUsageData, int, error) {
	req, err := http.NewRequest(http.MethodGet, usageBaseFor(sa)+"/user/usage", nil)
	if err != nil {
		return nil, 0, err
	}
	applyLaneHeadersForLane(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("usage http %d", resp.StatusCode)
	}
	var env mimoUsageEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("usage parse: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("usage code %d: %s", env.Code, truncateRedacted(env.Message, 120))
	}
	if env.Data == nil || env.Data.ResetDate == nil || strings.TrimSpace(*env.Data.ResetDate) == "" {
		// Bundle semantics: no resetDate ⇒ no active subscription. Report the
		// canonical condition instead of rendering a meaningless percent.
		return nil, resp.StatusCode, errNoActiveSubscription
	}
	return env.Data, resp.StatusCode, nil
}

// fetchMimoUsage fetches the usage with the cookie lane's one renew+retry
// ladder (302 serviceLogin bounce / 401 → re-mint → retry once), mirroring
// the chat path (session.go). sk lane has no renewal: its sk is permanent.
func fetchMimoUsage(sa *storedAuth) (*mimoUsageData, error) {
	data, status, err := fetchUsageOnce(sa)
	if err == nil {
		return data, nil
	}
	if authLaneFor(sa) == laneCookie &&
		(status == http.StatusFound || status == http.StatusUnauthorized) &&
		renewCookieSessionFn(sa) {
		if data, _, err = fetchUsageOnce(sa); err == nil {
			return data, nil
		}
	}
	return nil, err
}

// fetchMimoSubscription returns current when status=ACTIVE, else nil.
// Best-effort plan-title source for the note; failures are non-fatal.
func fetchMimoSubscription(sa *storedAuth) *mimoSubscription {
	req, err := http.NewRequest(http.MethodGet, usageBaseFor(sa)+"/user/xiaomi/subscription/self", nil)
	if err != nil {
		return nil
	}
	applyLaneHeadersForLane(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	var env mimoSubscriptionEnvelope
	if json.Unmarshal(resp.Body, &env) != nil || env.Code != 0 || env.Data == nil {
		return nil
	}
	cur := env.Data.Current
	if cur == nil || !strings.EqualFold(strings.TrimSpace(cur.Status), "ACTIVE") {
		return nil
	}
	return cur
}

// formatPercent renders the percent compactly: 63.4 → "63.4", 100 → "100".
func formatPercent(p float64) string {
	return strconv.FormatFloat(p, 'f', -1, 64)
}

// shortResetDate trims "2026-09-24" to "09-24" for the one-line note; other
// shapes pass through trimmed (never guessed).
func shortResetDate(d string) string {
	d = strings.TrimSpace(d)
	if len(d) == 10 && d[4] == '-' && d[7] == '-' {
		return d[5:]
	}
	return d
}

// regionPrefixFor renders the note head: region uppercase when the credential
// carries one (cookie lane), the lane tag otherwise (sk lane). Mirrors
// labelForAuth's [KEY]/[COOKIE/SGP] convention.
func regionPrefixFor(sa *storedAuth) string {
	lane := authLaneFor(sa)
	r := ""
	if sa != nil {
		r = strings.TrimSpace(sa.Auth.Region)
	}
	if r == "" && lane == laneCookie {
		r = loadedRegionMode()
	}
	if r == "" && lane == laneCookie {
		r = regionCN
	}
	if r != "" {
		return strings.ToUpper(r)
	}
	return "KEY"
}

// usageNoteFor builds the one-line quota note (workbuddy「区域 · 余N」形制):
//
//	「SGP · MiMo GL Plus · 余63.4% · 重置09-24」 — full shape
//	「SGP · 余63.4% · 重置09-24」              — usage without a live plan
//	「SGP · 无有效订阅」                        — resetDate null (bundle no-data)
//
// All pieces are measured facts; nothing is inferred from a percent alone.
// The no-subscription state is checked FIRST and swallows the plan title —
// a stale ACTIVE record must never sit next to「无有效订阅」.
func usageNoteFor(sa *storedAuth, u *mimoUsageData, sub *mimoSubscription) string {
	parts := []string{regionPrefixFor(sa)}
	if u == nil || u.ResetDate == nil {
		// resetDate null/absent ⇒ no active subscription (bundle semantics).
		parts = append(parts, "无有效订阅")
		return strings.Join(parts, " · ")
	}
	if sub != nil && strings.TrimSpace(sub.Title) != "" {
		parts = append(parts, strings.TrimSpace(sub.Title))
	}
	if u.Percent != nil && *u.Percent >= 0 {
		parts = append(parts, "余"+formatPercent(*u.Percent)+"%")
	}
	parts = append(parts, "重置"+shortResetDate(*u.ResetDate))
	note := strings.Join(parts, " · ")
	if len(note) > 80 {
		r := []rune(note)
		if len(r) > 26 {
			r = r[:26]
		}
		note = string(r) + "…"
	}
	return note
}

// mergeAuthNote returns the credential JSON with only the top-level note
// replaced. map[string]json.RawMessage keeps every untouched field
// byte-exact — re-marshaling through map[string]any would rewrite int64
// fields (adoptedAt/exchangedAt/cookie expires) in exponent form.
func mergeAuthNote(existing []byte, note string) ([]byte, error) {
	base := map[string]json.RawMessage{}
	if err := json.Unmarshal(existing, &base); err != nil {
		return nil, fmt.Errorf("note merge: parse auth json: %w", err)
	}
	nb, err := json.Marshal(note)
	if err != nil {
		return nil, err
	}
	base["note"] = nb
	return json.Marshal(base)
}

// noteFromAuthJSON reads the top-level note off a raw credential body.
func noteFromAuthJSON(raw []byte) string {
	var m struct {
		Note string `json:"note"`
	}
	_ = json.Unmarshal(raw, &m)
	return strings.TrimSpace(m.Note)
}

// syncAuthUsageNote writes the quota note for one credential. Idempotent:
// the physical JSON is read, the save is skipped when the note already
// matches; everything except the note stays byte-exact (disabled state
// included).
func syncAuthUsageNote(authIndex string, sa *storedAuth, u *mimoUsageData, sub *mimoSubscription) error {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return err
	}
	next := usageNoteFor(sa, u, sub)
	if next == "" || next == noteFromAuthJSON(phys.JSON) {
		return nil
	}
	merged, err := mergeAuthNote(phys.JSON, next)
	if err != nil {
		return err
	}
	name, _ := resolveAuthFileTarget(sa, phys)
	return hostAuthPersistFn(name, merged)
}

// usage-note loop plumbing. The loop starts once on register/reconfigure and
// stays quiet without a host bridge (tests, bare parses).
var (
	usageNoteOnce sync.Once
	usageNoteStop = make(chan struct{})
	// usageSweepFn is the test seam for the periodic sweep body.
	usageSweepFn = refreshAllUsageNotes
)

// startUsageNoteLoop kicks the background quota/note refresh (once).
func startUsageNoteLoop() {
	if hostAPI == nil {
		return // no host bridge (unit tests): stay quiet
	}
	usageNoteOnce.Do(func() {
		go func() {
			select {
			case <-time.After(usageNoteStartDelay):
			case <-usageNoteStop:
				return
			}
			for {
				usageSweepFn()
				select {
				case <-time.After(usageNoteInterval):
				case <-usageNoteStop:
					return
				}
			}
		}()
	})
}

// refreshAllUsageNotes sweeps every host-known credential: fetch usage, sync
// the note. A failed fetch leaves the previous note in place — a transient
// billing/session failure must never blank a card the user already sees
// (workbuddy syncAuthNote's prev-segment guarantee, adapted).
func refreshAllUsageNotes() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	for _, f := range files {
		select {
		case <-usageNoteStop:
			return
		default:
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil || sa == nil {
			continue
		}
		u, err := fetchMimoUsage(sa)
		if err != nil {
			// Expected shapes: cn-host usage surface unverified, desktop-
			// revoked cookie sessions, genuinely unsubscribed accounts
			// (no-data). Prev note preserved; log is quiet by design.
			log.Printf("usage %s: %v", truncateRedacted(f.Name, 60), err)
			continue
		}
		sub := fetchMimoSubscription(sa)
		if err := syncAuthUsageNote(f.AuthIndex, sa, u, sub); err != nil {
			log.Printf("usage note %s: %v", truncateRedacted(f.Name, 60), err)
		}
		select {
		case <-time.After(usageSweepSpacing):
		case <-usageNoteStop:
			return
		}
	}
}
