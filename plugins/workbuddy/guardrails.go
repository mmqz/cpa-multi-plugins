// guardrails.go — opt-in credit guardrails for the scheduler pick (#61).
//
// Two knobs, both default-off, both pure consumers of the existing credits
// cache (the #56-era standing constraint: guardrails add zero upstream calls):
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
