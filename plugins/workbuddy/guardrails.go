// guardrails.go — opt-in credit guardrails for the scheduler pick (#61).
//
// Two knobs, both default-off, both reading the credits cache. Since 0.9.85
// (issue #70) a cache miss under live guard config schedules an async
// singleflight arm — the #56 "zero upstream calls" contract now means "zero
// SYNCHRONOUS upstream calls on the pick path"; an unarmed guardrail that
// silently fails open was the worse trade:
//
//	reserve_credits    skip a credential whose cached remaining credits are
//	                   <= the reserve, so the account stays usable for the
//	                   official client / emergencies instead of being drained
//	                   to zero by routine pool traffic.
//	daily_credits_cap  skip a credential once its estimated same-day credit
//	                   spend reaches the cap. Spend is measured as
//	                   day-start-remaining − current-remaining from the same
//	                   cache plane (mmqz #61: "start with the difference, it's
//	                   monotone enough for a guardrail") — no per-request
//	                   accounting, no wiring out of the streaming path.
//
// Placement (mmqz #61 answer 2): the skip lives in handleSchedulerPick — a
// guarded credential is simply not offered as a candidate, which is invisible
// to the host (no error surfaced, no retry budget consumed) and keeps the
// layering with #60's admission control: admission owns concurrency (runtime
// state), guardrails own balance (persistent state).
//
// Documented limitation (#61): the skip only bites when the plugin is doing
// the picking (scheduler_mode: credits); in host-side pick modes the guardrail
// cannot intervene. When EVERY workbuddy candidate is guarded the pick defers
// to the host like any empty-candidate case — logged, not silently swallowed.
//
// Day semantics (mmqz #61 answer 3): UTC+8 calendar day (upstream's 6004 reset
// copy phrases windows in Beijing time); counters are in-memory only. A daily
// budget counter is estimative state: after a restart the first billing
// refresh rebuilds the credits side exactly, and any under-count errs toward
// firing later — acceptable for a guardrail whose job is keeping a reserve.
// The day-start snapshot is taken on first sight of a credential each day, so
// spend that happened before the first cache read is under-counted (same
// err-later direction). Credits granted mid-day (check-in rewards) can make
// the difference negative; a negative spend never guards.
//
// Per-credential overrides (mmqz #61 config-shape lean): global defaults,
// optionally overridden per credential in the auth document top level —
// reserve_credits / daily_credits_cap as numbers; absent = inherit the
// global, 0 = explicitly disable that guardrail for this credential.
package main

import (
	"log"
	"sync"
	"time"
)

// Global knobs, applied by configure(); 0 = off.
var (
	reserveCredits    int64
	reserveCreditsMu  sync.RWMutex
	dailyCreditsCap   int64
	dailyCreditsCapMu sync.RWMutex
)

// setReserveCredits / setDailyCreditsCap are test seams (setSchedulerMode shape).
func setReserveCredits(n int64) func() {
	reserveCreditsMu.Lock()
	old := reserveCredits
	reserveCredits = n
	reserveCreditsMu.Unlock()
	return func() { reserveCreditsMu.Lock(); reserveCredits = old; reserveCreditsMu.Unlock() }
}
func setDailyCreditsCap(n int64) func() {
	dailyCreditsCapMu.Lock()
	old := dailyCreditsCap
	dailyCreditsCap = n
	dailyCreditsCapMu.Unlock()
	return func() { dailyCreditsCapMu.Lock(); dailyCreditsCap = old; dailyCreditsCapMu.Unlock() }
}
func loadedReserveCredits() int64 {
	reserveCreditsMu.RLock()
	defer reserveCreditsMu.RUnlock()
	return reserveCredits
}
func loadedDailyCreditsCap() int64 {
	dailyCreditsCapMu.RLock()
	defer dailyCreditsCapMu.RUnlock()
	return dailyCreditsCap
}

// beijingDayNow is the UTC+8 calendar-day key; a var so tests can roll the day.
var beijingDayNow = func() string {
	return time.Now().In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02")
}

// guardDayState tracks the day-start remaining snapshot per credential.
type guardDayState struct {
	mu             sync.Mutex
	day            string
	dayStartRemain map[string]int64
}

var guardSpend = &guardDayState{dayStartRemain: map[string]int64{}}

// resetGuardSpendForTest clears snapshot state (test seam).
func resetGuardSpendForTest() {
	guardSpend.mu.Lock()
	guardSpend.day = ""
	guardSpend.dayStartRemain = map[string]int64{}
	guardSpend.mu.Unlock()
}

// dayStartSnapshot returns (records if needed) the day-start remaining for
// authID given its current remaining. Day rollover resets every snapshot.
func (s *guardDayState) dayStartSnapshot(authID string, current int64) int64 {
	day := beijingDayNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	if day != s.day {
		s.day = day
		s.dayStartRemain = map[string]int64{}
	}
	if _, ok := s.dayStartRemain[authID]; !ok {
		s.dayStartRemain[authID] = current
	}
	return s.dayStartRemain[authID]
}

// effectiveReserve / effectiveDailyCap resolve global default vs the
// credential-record override carried on the cache entry (nil = inherit).
func effectiveReserve(override *int64) int64 {
	if override != nil {
		return *override
	}
	return loadedReserveCredits()
}
func effectiveDailyCap(override *int64) int64 {
	if override != nil {
		return *override
	}
	return loadedDailyCreditsCap()
}

// guardAnyConfigured reports whether ANY guardrail is configured globally.
// Per-credential overrides ride the cache entry itself, so with an empty
// cache they are invisible by construction — the arming pass is what
// (re)materializes them (cachedAccountDetails Stores the sa-carried
// override pointers).
func guardAnyConfigured() bool {
	return loadedReserveCredits() > 0 || loadedDailyCreditsCap() > 0
}

// Self-arming (issue #70): the guardrails judge the credits cache, but
// nothing outside the panel poll ever (re)populated an EMPTY cache —
// reconcileAfterExecutorError is lifecycle-gated, the check-in tick was
// Load-Modify-Store, and a plugin re-register (upgrade / config apply)
// clears the map. Between re-register and the first panel open the
// guardrails silently failed open with no log line. Now a pick that
// finds guard config live but no credits snapshot schedules ONE async
// arm per credential per retry window (the fetch rides the existing
// per-authID singleflight + TTL in cachedAccountDetails); the pick
// itself stays fail-open — the guardrail bites from the next pick on.
const guardArmRetryInterval = 30 * time.Second

var (
	guardArmMu   sync.Mutex
	guardArmLast = map[string]time.Time{} // authID -> last arm attempt
	// guardArmFetch is the injectable fetch seam (tests swap it out to
	// count arming passes without goroutines or a host bridge);
	// guardArmLaunch schedules the fetch (async in production, synchronous
	// under test so assertions are deterministic).
	guardArmFetch  = guardArmFetchHost
	guardArmLaunch = func(fn func()) { go fn() }
)

// guardArmFetchHost resolves authID -> storedAuth through the host list
// and runs one cachedAccountDetails pass (singleflight-serialized; the
// 45s TTL means an armed credential never re-triggers).
func guardArmFetchHost(authID string) {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	for _, f := range files {
		if f.ID != authID {
			continue
		}
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			return
		}
		cachedAccountDetails(authID, sa, false)
		return
	}
}

// guardTryArm fires the async arm when guard config is live and the
// credential has no credits snapshot. Rate-limited per credential to one
// attempt per retry window; every attempt that actually fires logs the
// unarmed state (issue #70: "no log ever said the guardrail was unarmed").
func guardTryArm(authID string) {
	if !guardAnyConfigured() {
		return
	}
	// cachedCreditsScore's second return is exhausted, NOT a cache-hit
	// flag — remain < 0 is the unknown-credits signal (no entry, or a
	// legacy entry whose credits field is nil).
	if remain, _ := cachedCreditsScore(authID); remain >= 0 {
		return
	}
	now := time.Now()
	guardArmMu.Lock()
	if t, seen := guardArmLast[authID]; seen && now.Sub(t) < guardArmRetryInterval {
		guardArmMu.Unlock()
		return
	}
	guardArmLast[authID] = now
	guardArmMu.Unlock()
	log.Printf("workbuddy: credit guardrail configured but %s has no credits snapshot (re-registered / lifecycle off / panel not opened) — arming async, fail-open until then (issue #70)", authID)
	guardArmLaunch(func() { guardArmFetch(authID) })
}

// guardSkipCredential reports whether the credential should be withheld from
// scheduler candidates. Unknown credits (no cache entry yet, -1 remain) never
// guard — a guardrail cannot judge what it cannot see. Per-credential
// overrides carried on the cache entry take precedence over the globals and
// work even when the globals are off.
func guardSkipCredential(authID string) bool {
	reserveOv, capOv := credentialGuardOverrides(authID)
	if loadedReserveCredits() <= 0 && loadedDailyCreditsCap() <= 0 && reserveOv == nil && capOv == nil {
		return false // nothing configured anywhere: zero state, byte-for-byte old pick
	}
	remain, _ := cachedCreditsScore(authID)
	if remain < 0 {
		// Unknown credits: fail open (a guardrail cannot judge what it
		// cannot see) but schedule the async arm so the window closes
		// (issue #70).
		guardTryArm(authID)
		return false
	}
	return guardSkipRemain(authID, remain, reserveOv, capOv)
}

// guardSkipRemain is the pure core of guardSkipCredential with injected
// values (test surface): current remaining + optional per-credential
// overrides carried on the cache entry.
func guardSkipRemain(authID string, remain int64, reserveOverride, capOverride *int64) bool {
	reserve := effectiveReserve(reserveOverride)
	cap := effectiveDailyCap(capOverride)
	if reserve <= 0 && cap <= 0 {
		return false
	}
	if reserve > 0 && remain <= reserve {
		return true
	}
	if cap > 0 {
		start := guardSpend.dayStartSnapshot(authID, remain)
		if start-remain >= cap {
			return true
		}
	}
	return false
}

// credentialGuardOverrides returns the per-credential override pointers from
// the cache entry (nil when absent/stale — inherit globals).
func credentialGuardOverrides(authID string) (reserve, cap *int64) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return nil, nil
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok {
		return nil, nil
	}
	return entry.reserveOverride, entry.dailyCapOverride
}
