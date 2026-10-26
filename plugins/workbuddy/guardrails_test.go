package main

import (
	"testing"
	"time"
)

func storeCreditsFor(authID string, remain int64, reserveOv, capOv *int64) {
	accountCache.Store(authID, &accountCacheEntry{
		credits:         &creditsSummary{TotalRemain: remain},
		reserveOverride: reserveOv,
		dailyCapOverride: capOv,
	})
}

func TestGuardrailsDisabledByDefault(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	r1 := setReserveCredits(0)
	defer r1()
	r2 := setDailyCreditsCap(0)
	defer r2()

	storeCreditsFor("g-default", 0, nil, nil) // even zero remain
	if guardSkipCredential("g-default") {
		t.Fatal("default-off guardrails skipped a credential")
	}
	if guardSkipCredential("g-unknown") {
		t.Fatal("default-off guardrails skipped an unknown credential")
	}
}

func TestGuardrailsReserve(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	restore := setReserveCredits(10)
	defer restore()

	storeCreditsFor("g-res-low", 10, nil, nil) // remain == reserve → guarded
	if !guardSkipCredential("g-res-low") {
		t.Fatal("remain == reserve should guard")
	}
	storeCreditsFor("g-res-ok", 11, nil, nil)
	if guardSkipCredential("g-res-ok") {
		t.Fatal("remain > reserve should not guard")
	}
	// Unknown credits never guard.
	if guardSkipCredential("g-res-unknown") {
		t.Fatal("unknown credits should never guard")
	}
}

func TestGuardrailsReservePerCredentialOverride(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	restore := setReserveCredits(100)
	defer restore()

	zero := int64(0)
	higher := int64(500)
	storeCreditsFor("g-res-zero", 5, &zero, nil) // explicit 0 disables
	if guardSkipCredential("g-res-zero") {
		t.Fatal("per-credential 0 override must disable the guardrail")
	}
	storeCreditsFor("g-res-higher", 5, &higher, nil) // tighter override
	if !guardSkipCredential("g-res-higher") {
		t.Fatal("per-credential 500 override must guard at remain=5")
	}
	storeCreditsFor("g-res-inherit", 5, nil, nil)
	if !guardSkipCredential("g-res-inherit") {
		t.Fatal("nil override must inherit global=100 and guard at remain=5")
	}
}

func TestGuardrailsOverrideWorksWhenGlobalOff(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	r := setReserveCredits(0)
	defer r()

	twenty := int64(20)
	storeCreditsFor("g-ov-only", 5, &twenty, nil)
	if !guardSkipCredential("g-ov-only") {
		t.Fatal("per-credential override must bite even when global is off")
	}
}

func TestGuardrailsDailyCap(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	restore := setDailyCreditsCap(30)
	defer restore()

	// First sight: snapshot at 100.
	storeCreditsFor("g-cap", 100, nil, nil)
	if guardSkipCredential("g-cap") {
		t.Fatal("day-start first sight must not guard (spend 0)")
	}
	// Spend 30 → spend == cap → guarded.
	storeCreditsFor("g-cap", 70, nil, nil)
	if !guardSkipCredential("g-cap") {
		t.Fatal("spend == cap should guard")
	}
	// Spend below cap → not guarded.
	storeCreditsFor("g-cap2", 85, nil, nil)
	if guardSkipCredential("g-cap2") {
		t.Fatal("spend 15 < cap 30 should not guard")
	}
}

func TestGuardrailsDailyCapGrantNeverGuards(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	restore := setDailyCreditsCap(10)
	defer restore()

	// Check-in grants mid-day raise the remaining: spend goes negative.
	storeCreditsFor("g-grant", 100, nil, nil)
	guardSkipCredential("g-grant") // snapshot at 100
	storeCreditsFor("g-grant", 150, nil, nil)
	if guardSkipCredential("g-grant") {
		t.Fatal("negative spend (credits granted) must never guard")
	}
}

func TestGuardrailsDayRolloverResets(t *testing.T) {
	resetGuardSpendForTest()
	defer resetGuardSpendForTest()
	restore := setDailyCreditsCap(10)
	defer restore()

	realDay := beijingDayNow
	day := "2026-10-25"
	beijingDayNow = func() string { return day }
	defer func() { beijingDayNow = realDay }()

	storeCreditsFor("g-roll", 100, nil, nil)
	guardSkipCredential("g-roll") // snapshot day 10-25
	storeCreditsFor("g-roll", 85, nil, nil)
	if !guardSkipCredential("g-roll") {
		t.Fatal("spend 15 >= cap 10 within the day should guard")
	}

	// Next UTC+8 day: snapshot resets, spend counts from the new baseline.
	day = "2026-10-26"
	storeCreditsFor("g-roll", 84, nil, nil)
	if guardSkipCredential("g-roll") {
		t.Fatal("day rollover must reset the day-start snapshot (spend 0)")
	}
}

func TestGuardrailsBeijingDay(t *testing.T) {
	// 2026-10-25 23:30 UTC == 2026-10-26 07:30 UTC+8.
	utc := time.Date(2026, 10, 25, 23, 30, 0, 0, time.UTC)
	got := utc.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02")
	if got != "2026-10-26" {
		t.Fatalf("UTC+8 day roll: got %s, want 2026-10-26", got)
	}
}

func TestConfigureGuardrailKnobs(t *testing.T) {
	r1 := setReserveCredits(0)
	defer r1()
	r2 := setDailyCreditsCap(0)
	defer r2()

	wbConfigureWire(t, "enabled: true\nreserve_credits: 50\ndaily_credits_cap: 200\n")
	if got := loadedReserveCredits(); got != 50 {
		t.Fatalf("reserve_credits=50 not applied: %d", got)
	}
	if got := loadedDailyCreditsCap(); got != 200 {
		t.Fatalf("daily_credits_cap=200 not applied: %d", got)
	}
	// Bare reconfigure resets to defaults.
	wbConfigureWire(t, "enabled: true\n")
	if got := loadedReserveCredits(); got != 0 {
		t.Fatalf("bare reconfigure must reset reserve_credits: %d", got)
	}
	// Non-positive and unparseable normalize to disabled.
	wbConfigureWire(t, "enabled: true\nreserve_credits: -5\ndaily_credits_cap: \"x\"\n")
	if got := loadedReserveCredits(); got != 0 {
		t.Fatalf("negative reserve must normalize to 0: %d", got)
	}
	if got := loadedDailyCreditsCap(); got != 0 {
		t.Fatalf("unparseable cap must normalize to 0: %d", got)
	}
	// Flow-style mapping.
	wbConfigureWire(t, "{enabled: true, reserve_credits: 7, daily_credits_cap: 9}\n")
	if got := loadedReserveCredits(); got != 7 {
		t.Fatalf("flow-style reserve not applied: %d", got)
	}
	if got := loadedDailyCreditsCap(); got != 9 {
		t.Fatalf("flow-style cap not applied: %d", got)
	}
}
