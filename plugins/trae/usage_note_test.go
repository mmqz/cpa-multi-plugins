package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// resetUsageNoteTestState clears the in-memory ledger and throttle maps so
// tests start from a fresh process state.
func resetUsageNoteTestState(t *testing.T) {
	t.Helper()
	usageLedgerState.Lock()
	usageLedgerState.ledger = nil
	usageLedgerState.loaded = false
	usageLedgerState.dirty = false
	usageLedgerState.lastFlush = time.Time{}
	usageLedgerState.lastWrite = nil
	usageLedgerState.Unlock()
	usageDataDirOnce.Lock()
	usageDataDirOnce.dir = ""
	usageDataDirOnce.done = true // keep tests off the real filesystem
	usageDataDirOnce.Unlock()
}

func TestUsageLedgerObserveAndBucketsTrae(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	base := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "id-1", 1000, false, base)
	usageLedgerObserve("idx-1", "id-1", 500, true, base.Add(10*time.Minute))
	usageLedgerObserve("idx-1", "id-1", 1500, false, base.Add(20*time.Minute))

	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a == nil || a.Total.Requests != 3 || a.Total.Success != 2 || a.Total.Failed != 1 || a.Total.Tokens != 3000 {
		t.Fatalf("totals wrong: %+v", a)
	}
	if a.AuthID != "id-1" {
		t.Fatalf("auth id not recorded: %q", a.AuthID)
	}
	if a.Days["2026-09-27"] == nil || a.Hours["2026-09-27T12"] == nil {
		t.Fatalf("buckets missing: %+v", a)
	}
}

func TestUsageNoteSegmentFallsBackWithoutWindow(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now.Add(-30*time.Minute))
	usageLedgerObserve("idx-1", "", 2000, true, now.Add(-10*time.Minute))

	seg := usageNoteSegmentFor("idx-1", now)
	if !strings.HasPrefix(seg, usageSegmentMarker) {
		t.Fatalf("segment marker missing: %q", seg)
	}
	if !strings.Contains(seg, "该账号暂未解析出额度窗口") {
		t.Fatalf("no-window fallback missing (trae exposes no window bounds): %q", seg)
	}
	if !strings.Contains(seg, "累计 请求2 · 成功率50%") {
		t.Fatalf("all-time stats missing: %q", seg)
	}
	if got := usageNoteSegmentFor("idx-unknown", now); got != "" {
		t.Fatalf("unknown credential must render empty, got %q", got)
	}
}

func TestUsageNoteWritePreservesHandWrittenBase(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	// trae has no other note writer; a user's hand-written note must survive
	// as the base ahead of the usage segment.
	baseDoc := []byte(`{"type":"trae","provider":"trae","auth":{"accessToken":"tok","variant":"cn"},"account":{"uid":"u1"},"note":"张三的账号"}`)
	restoreSeams := stubPersistSeams(t, map[string][]byte{"a1": baseDoc})
	defer restoreSeams()

	usageLedgerObserve("a1", "", 1234, false, time.Now())
	usageNoteWrite("a1")
	if n := len(saveCalls()); n != 1 {
		t.Fatalf("first write should save once, got %d", n)
	}
	var saved map[string]json.RawMessage
	parts := strings.SplitN(saveCalls()[0], "=", 2)
	if err := json.Unmarshal([]byte(parts[1]), &saved); err != nil {
		t.Fatalf("saved doc unreadable: %v", err)
	}
	var note string
	if err := json.Unmarshal(saved["note"], &note); err != nil {
		t.Fatalf("note unreadable: %v", err)
	}
	if !strings.HasPrefix(note, "张三的账号 · ") || !strings.Contains(note, usageSegmentMarker) {
		t.Fatalf("hand-written base lost: %q", note)
	}
	// The saved doc must keep the model_cache snapshot (save-funnel guard).
	if _, ok := saved["model_cache"]; ok {
		t.Fatalf("usage writer must not invent model_cache (funnel's job)")
	}

	// Unchanged second write → change-guard skips the save.
	usageNoteWrite("a1")
	if n := len(saveCalls()); n != 1 {
		t.Fatalf("unchanged write must not save (watcher churn), got %d", n)
	}
}

func TestUsageQuotaStampFromSummary(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now)
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		PlanType:    "Pro",
		Remain:      42,
		RemainKnown: true,
		Used:        58,
		Total:       100,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 1234, Known: true},
	})

	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a.Quota == nil {
		t.Fatal("quota not stamped")
	}
	if a.Quota.Remain != 1234 {
		t.Fatalf("credits pool remain must win: %+v", a.Quota)
	}
	if a.Quota.Plan != "Pro" {
		t.Fatalf("plan not recorded: %+v", a.Quota)
	}
	if a.Quota.WindowStart != "" || a.Quota.WindowEnd != "" {
		t.Fatalf("trae exposes no window bounds: %+v", a.Quota)
	}
}

func TestUsageQuotaUnknownStampsNothingTrae(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now)
	// Both dimensions unknown (failed/empty ent_usage) must NOT stamp a 0 —
	// 0 is a real number (exhausted); unknown keeps the previous state.
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		PlanType:    "Pro",
		Remain:      0,
		RemainKnown: false,
		Used:        0,
		Total:       0,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 0, Known: false},
	})

	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a == nil {
		t.Fatal("account entry missing")
	}
	if a.Quota != nil {
		t.Fatalf("unknown quota must not stamp (would fabricate 余0): %+v", a.Quota)
	}

	// A previously stamped quota survives the unknown fetch (prev-guard).
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		RemainKnown: true, Remain: 3000,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 3210, Known: true},
	})
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{RemainKnown: false})
	usageLedgerState.Lock()
	a = usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a.Quota == nil || a.Quota.Remain != 3210 {
		t.Fatalf("prev quota must survive an unknown fetch: %+v", a.Quota)
	}
}

func TestUsageQuotaRealZeroStampsTrae(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now)
	// Known pool with Remain=0 is a REAL exhausted state — it must stamp so
	// the card shows the truth (0) instead of hiding it.
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		RemainKnown: false,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 0, Known: true},
	})
	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a.Quota == nil || a.Quota.Remain != 0 {
		t.Fatalf("real zero must stamp: %+v", a.Quota)
	}
	seg := usageNoteSegmentFor("idx-1", now)
	if !strings.Contains(seg, "余0") {
		t.Fatalf("real zero must render 余0: %q", seg)
	}
}

func TestUsageQuotaZeroRequestAccountGetsEntryTrae(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	// No usage records observed at all — the quota fold-in (panel 刷新积分)
	// must still create the ledger entry so the card gets a credit segment.
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		RemainKnown: true, Remain: 7,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 1234, Known: true},
	})
	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a == nil {
		t.Fatal("zero-request account must get a ledger entry for quota fold-in")
	}
	seg := usageNoteSegmentFor("idx-1", now)
	if !strings.Contains(seg, "余1234") || !strings.Contains(seg, usageSegmentMarker) {
		t.Fatalf("credit segment must render on a zero-request account: %q", seg)
	}
	if strings.Contains(seg, "该账号暂未解析出额度窗口") == false {
		t.Fatalf("no-window fallback expected: %q", seg)
	}
}

func TestUsageNoteSegmentRendersCreditSegmentTrae(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now.Add(-30*time.Minute))
	usageQuotaStampFromSummary("idx-1", upstream.UsageSummary{
		PlanType:    "SOLO Pro",
		RemainKnown: true, Remain: 12,
		CreditsPool: upstream.CreditsPoolInfo{Remain: 3000, Known: true},
		Used:        8,
	})
	seg := usageNoteSegmentFor("idx-1", now)
	for _, want := range []string{"【用量】", "余3000", "已用8", "SOLO Pro", "累计 请求1"} {
		if !strings.Contains(seg, want) {
			t.Fatalf("segment missing %q: %q", want, seg)
		}
	}
}

// TestUsageLedgerMigratesLegacyAuthDirFile verifies the one-time relocation:
// a pre-relocation ledger inside the auth dir moves to the auth dir's PARENT
// on first load, and its accounts survive the trip. The auth-dir copy must
// be gone afterwards — that is the whole point (the host lists auth-dir
// contents as unrecognized credentials).
func TestUsageLedgerMigratesLegacyAuthDirFile(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	authDir := t.TempDir()
	usageDataDirOnce.Lock()
	usageDataDirOnce.dir = authDir
	usageDataDirOnce.done = true
	usageDataDirOnce.Unlock()

	legacy := filepath.Join(authDir, "."+providerName+"-usage.json")
	doc := usageLedger{Version: usageLedgerVersion, StartedAt: "2026-09-27T00:00:00Z", Accounts: map[string]*usageAccount{}}
	doc.Accounts["idx-legacy"] = &usageAccount{AuthID: "id-legacy", Total: usageTotals{Requests: 3, Success: 2, Failed: 1, Tokens: 3000}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	usageLedgerState.Lock()
	led := usageLedgerLocked()
	a := led.Accounts["idx-legacy"]
	usageLedgerState.Unlock()
	if a == nil || a.AuthID != "id-legacy" || a.Total.Requests != 3 {
		t.Fatalf("legacy ledger not loaded after migration: %+v", a)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file still present in auth dir (err=%v)", err)
	}
	migrated := filepath.Join(filepath.Dir(authDir), "."+providerName+"-usage.json")
	if _, err := os.Stat(migrated); err != nil {
		t.Fatalf("migrated ledger missing at %s: %v", migrated, err)
	}
	os.Remove(migrated)
}

// TestUsageLedgerMigrationKeepsNewWorldFile verifies the migration is not a
// clobber: when a new-world ledger already exists at the relocated path, the
// stale legacy copy is left untouched and the new-world content wins.
func TestUsageLedgerMigrationKeepsNewWorldFile(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	authDir := t.TempDir()
	usageDataDirOnce.Lock()
	usageDataDirOnce.dir = authDir
	usageDataDirOnce.done = true
	usageDataDirOnce.Unlock()

	legacy := filepath.Join(authDir, "."+providerName+"-usage.json")
	doc := usageLedger{Version: usageLedgerVersion, StartedAt: "2026-09-01T00:00:00Z", Accounts: map[string]*usageAccount{}}
	doc.Accounts["idx-stale"] = &usageAccount{AuthID: "id-stale"}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(filepath.Dir(authDir), "."+providerName+"-usage.json")
	newDoc := usageLedger{Version: usageLedgerVersion, StartedAt: "2026-09-29T00:00:00Z", Accounts: map[string]*usageAccount{}}
	newDoc.Accounts["idx-current"] = &usageAccount{AuthID: "id-current"}
	newRaw, err := json.Marshal(newDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, newRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	usageLedgerState.Lock()
	led := usageLedgerLocked()
	usageLedgerState.Unlock()
	if led.Accounts["idx-current"] == nil || led.Accounts["idx-stale"] != nil {
		t.Fatalf("new-world ledger must win: %+v", led.Accounts)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy copy must be left alone when new-world exists: %v", err)
	}
	os.Remove(newPath)
	os.Remove(legacy)
}
