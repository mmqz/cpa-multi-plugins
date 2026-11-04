// guard_arm_test.go pins the issue #70 self-arming: a guard config that is
// live while the credits cache misses (fresh re-register, lifecycle off,
// panel never opened) schedules exactly one async arm per credential per
// retry window, logs the unarmed state, and never blocks the pick (the
// fail-open branch fires it on the way out). The fetch itself is a seam so
// these tests carry no goroutines.
package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// swapGuardArmFetch replaces the fetch and launch seams and returns
// (restore, counter): the fetch counts, the launch runs synchronously so
// assertions never race the goroutine.
func swapGuardArmFetch() (func(), *int64) {
	var calls int64
	oldFetch := guardArmFetch
	oldLaunch := guardArmLaunch
	guardArmFetch = func(authID string) { atomic.AddInt64(&calls, 1) }
	guardArmLaunch = func(fn func()) { fn() }
	return func() { guardArmFetch = oldFetch; guardArmLaunch = oldLaunch }, &calls
}

func resetGuardArmWindow() {
	guardArmMu.Lock()
	guardArmLast = map[string]time.Time{}
	guardArmMu.Unlock()
}

func TestGuardTryArmFiresOncePerWindow(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	resetGuardArmWindow()
	defer resetGuardArmWindow()
	restore, calls := swapGuardArmFetch()
	defer restore()
	r := setReserveCredits(100)
	defer r()

	// Unknown credits + configured guard → arms.
	guardTryArm("arm-a")
	guardTryArm("arm-a") // same window → suppressed
	guardTryArm("arm-a") // same window → suppressed
	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("arm fired %d times in one window, want 1", *calls)
	}

	// A different credential gets its own window.
	guardTryArm("arm-b")
	if atomic.LoadInt64(calls) != 2 {
		t.Fatalf("arm-b not fired: %d", *calls)
	}

	// Window elapsed → re-arms (retry after a failed fetch).
	guardArmMu.Lock()
	old := guardArmLast["arm-a"]
	guardArmLast["arm-a"] = old.Add(-guardArmRetryInterval - time.Second)
	guardArmMu.Unlock()
	guardTryArm("arm-a")
	if atomic.LoadInt64(calls) != 3 {
		t.Fatalf("elapsed window did not re-arm: %d", *calls)
	}
}

func TestGuardTryArmSkipsWhenUnconfiguredOrArmed(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	resetGuardArmWindow()
	defer resetGuardArmWindow()
	restore, calls := swapGuardArmFetch()
	defer restore()

	// No guard config anywhere → never arms.
	guardTryArm("arm-none")
	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("unconfigured guard armed: %d", *calls)
	}

	// Configured but armed (cache hit) → no arm.
	r := setReserveCredits(100)
	defer r()
	storeCreditsFor("arm-armed", 500, nil, nil)
	guardTryArm("arm-armed")
	if atomic.LoadInt64(calls) != 0 {
		t.Fatalf("armed credential re-armed: %d", *calls)
	}

	// A legacy entry without credits still counts as unknown → arms.
	accountCache.Store("arm-legacy", &accountCacheEntry{plan: "free"})
	guardTryArm("arm-legacy")
	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("credits-less entry not armed: %d", *calls)
	}
}

func TestGuardSkipFailOpenArms(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	resetGuardArmWindow()
	defer resetGuardArmWindow()
	restore, calls := swapGuardArmFetch()
	defer restore()
	r := setReserveCredits(3000)
	defer r()

	// The exact issue #70 shape: guard configured, cache empty → the pick
	// stays fail-open AND the arm is scheduled.
	if guardSkipCredential("g-70-empty") {
		t.Fatal("fail-open broken: unknown credits must not guard")
	}
	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("fail-open branch did not arm: %d", *calls)
	}
	// Second pick in the same window: still fail-open, no second arm.
	if guardSkipCredential("g-70-empty") {
		t.Fatal("fail-open broken (2nd)")
	}
	if atomic.LoadInt64(calls) != 1 {
		t.Fatalf("second pick re-armed inside the window: %d", *calls)
	}
}

func TestGuardAnyConfigured(t *testing.T) {
	r1 := setReserveCredits(0)
	defer r1()
	r2 := setDailyCreditsCap(0)
	defer r2()
	if guardAnyConfigured() {
		t.Fatal("both off must report unconfigured")
	}
	r1()
	r1 = setReserveCredits(1)
	defer r1()
	if !guardAnyConfigured() {
		t.Fatal("reserve on must report configured")
	}
}

// pick_priority_test.go cases live here too (same seam family).

func TestPickPriorityAuthFirstLive(t *testing.T) {
	setActiveAuthID("")
	defer setActiveAuthID("")
	cands := []activeAuthCandidate{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	if got := pickPriorityAuth(cands); got != "a" {
		t.Fatalf("first live = %q, want a", got)
	}
	// Sticky selection on c must NOT hold: priority reclaims to a.
	setActiveAuthID("c")
	if got := pickPriorityAuth(cands); got != "a" {
		t.Fatalf("reclaim = %q, want a (issue #71)", got)
	}
	if getActiveAuthID() != "a" {
		t.Fatalf("panel selection must track the serving account: %q", getActiveAuthID())
	}
}

func TestPickPriorityAuthSkipsNonLive(t *testing.T) {
	setActiveAuthID("")
	defer setActiveAuthID("")
	cands := []activeAuthCandidate{
		{ID: "a", Exhausted: true},
		{ID: "b", Disabled: true},
		{ID: "c"},
	}
	if got := pickPriorityAuth(cands); got != "c" {
		t.Fatalf("first LIVE = %q, want c", got)
	}
	// a recovers → the pick reclaims to a on the very next call (the ratchet
	// heals; the sticky picker would have stayed on c forever).
	cands[0].Exhausted = false
	if got := pickPriorityAuth(cands); got != "a" {
		t.Fatalf("recovered earlier credential not reclaimed: %q", got)
	}
}

func TestPickPriorityAuthAllExhaustedFallback(t *testing.T) {
	setActiveAuthID("")
	defer setActiveAuthID("")
	cands := []activeAuthCandidate{
		{ID: "a", Exhausted: true}, {ID: "b", Exhausted: true}, {ID: "c", Exhausted: true},
	}
	// All exhausted with no selection → first candidate, and the selection
	// is NOT moved (mirrors pickActiveAuth's keep-current semantics).
	if got := pickPriorityAuth(cands); got != "a" {
		t.Fatalf("all-exhausted fallback = %q, want a", got)
	}
	// All exhausted with the selection still listed → keep it.
	setActiveAuthID("c")
	if got := pickPriorityAuth(cands); got != "c" {
		t.Fatalf("all-exhausted keep-current = %q, want c", got)
	}
	// Selection gone from the list → first candidate.
	cands = cands[:2]
	if got := pickPriorityAuth(cands); got != "a" {
		t.Fatalf("gone selection fallback = %q, want a", got)
	}
	if pickPriorityAuth(nil) != "" {
		t.Fatal("empty candidate list must yield empty pick")
	}
}

func TestEnsureDefaultActiveAuthPriorityMode(t *testing.T) {
	restore := setPriorityReclaim(true)
	defer restore()
	setActiveAuthID("")
	defer setActiveAuthID("")

	// The panel view must agree with pickPriorityAuth: first live in host
	// order, reclaiming from a later sticky selection.
	accounts := []wbAccount{
		{AuthID: "a"}, {AuthID: "b", Exhausted: true}, {AuthID: "c"},
	}
	setActiveAuthID("c")
	if got := ensureDefaultActiveAuth(accounts); got != "a" {
		t.Fatalf("panel priority view = %q, want a", got)
	}
	if getActiveAuthID() != "a" {
		t.Fatalf("panel selection must track: %q", getActiveAuthID())
	}
}
