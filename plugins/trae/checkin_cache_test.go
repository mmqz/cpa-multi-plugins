package main

import (
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// checkinCacheEntry pins the v0.12.61 funds-snapshot rule: a claim that
// upstream accepted (code:0, including the documented idempotent echo) must
// INVALIDATE the cached usage/pool snapshot — the award lands upstream
// asynchronously, and carrying the pre-checkin snapshot forward froze the
// 积分余额 display on the old number. A failed claim keeps the previous
// snapshot (nothing changed upstream).

func TestCheckinCacheEntryClaimAcceptedInvalidatesFundsSnapshot(t *testing.T) {
	prev := &accountCacheEntry{
		credits:     420,
		fetched:     time.Now().Add(-time.Hour),
		usage:       testUsageSummary(25, 100),
		usageFilled: true,
		plan:        "SOLO Pro",
	}
	e := checkinCacheEntry(prev, true, testCheckinStatus(true, 150))
	if e.credits != -1 {
		t.Fatalf("claimAccepted must invalidate the credits snapshot, got credits=%d", e.credits)
	}
	if e.usageFilled {
		t.Fatal("claimAccepted must clear usageFilled so the dashboard omits the credits row (panel lazy-loads /credits)")
	}
	if e.checkin == nil || !e.checkin.CheckedIn || e.checkin.Credits != 150 {
		t.Fatalf("checkin card must refresh from the new status, got %+v", e.checkin)
	}
	if !e.fetched.After(prev.fetched) {
		t.Fatal("entry timestamp must advance")
	}
}

func TestCheckinCacheEntryFailedClaimKeepsPrevSnapshot(t *testing.T) {
	prev := &accountCacheEntry{
		credits:     420,
		fetched:     time.Now().Add(-time.Hour),
		usage:       testUsageSummary(25, 100),
		usageFilled: true,
		plan:        "SOLO Pro",
	}
	e := checkinCacheEntry(prev, false, testCheckinStatus(false, 150))
	if e.credits != 420 {
		t.Fatalf("failed claim must keep the previous credits snapshot, got credits=%d", e.credits)
	}
	if !e.usageFilled {
		t.Fatal("failed claim must keep usageFilled")
	}
	if e.usage.Remain != 25 || e.plan != "SOLO Pro" {
		t.Fatalf("failed claim must carry the previous usage/plan forward, got remain=%d plan=%q", e.usage.Remain, e.plan)
	}
	if e.checkin == nil || e.checkin.CheckedIn {
		t.Fatalf("checkin card must reflect the fresh (unchecked-in) status, got %+v", e.checkin)
	}
}

func TestCheckinCacheEntryNilPrev(t *testing.T) {
	e := checkinCacheEntry(nil, false, testCheckinStatus(false, 150))
	if e.credits != -1 || e.usageFilled {
		t.Fatalf("nil prev must degrade to the unknown-funds defaults, got credits=%d usageFilled=%v", e.credits, e.usageFilled)
	}
	if e.checkin == nil || e.checkin.Credits != 150 {
		t.Fatalf("checkin card must still refresh from status, got %+v", e.checkin)
	}
}

// testUsageSummary builds a minimal known-remain usage snapshot.
func testUsageSummary(remain, total int64) (u upstream.UsageSummary) {
	u.UsageModel = "basic"
	u.RemainKnown = true
	u.Remain = remain
	u.Used = total - remain
	u.Total = total
	return u
}

// testCheckinStatus builds the upstream.CheckinStatusResult the manual-checkin
// handler feeds the cache (CheckedIn/DidCheckedIn/Credits/Enable shape).
func testCheckinStatus(checkedIn bool, award int64) *upstream.CheckinStatusResult {
	return &upstream.CheckinStatusResult{
		CheckedIn: checkedIn,
		Credits:   award,
		Enable:    true,
	}
}
