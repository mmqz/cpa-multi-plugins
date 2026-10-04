package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestUsageLedgerObserveAndBuckets(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	base := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "id-1", 1000, false, base)
	usageLedgerObserve("idx-1", "id-1", 500, true, base.Add(10*time.Minute))
	usageLedgerObserve("idx-1", "id-1", 1500, false, base.Add(20*time.Minute))
	usageLedgerObserve("idx-2", "id-2", 10, false, base)

	usageLedgerState.Lock()
	led := usageLedgerState.ledger
	a := led.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if a == nil {
		t.Fatal("account idx-1 missing")
	}
	if a.Total.Requests != 3 || a.Total.Success != 2 || a.Total.Failed != 1 || a.Total.Tokens != 3000 {
		t.Fatalf("totals wrong: %+v", a.Total)
	}
	if a.AuthID != "id-1" {
		t.Fatalf("auth id not recorded: %q", a.AuthID)
	}
	day := a.Days["2026-09-27"]
	if day == nil || day.Requests != 3 || day.Tokens != 3000 {
		t.Fatalf("day bucket wrong: %+v", day)
	}
	if h := a.Hours["2026-09-27T12"]; h == nil || h.Requests != 3 {
		t.Fatalf("hour bucket wrong: %+v", h)
	}
	if other := led.Accounts["idx-2"]; other == nil || other.Total.Requests != 1 {
		t.Fatalf("second credential crossed into first: %+v", other)
	}
}

func TestUsageLedgerPrunesOldBuckets(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	old := now.Add(-usageHourRetention - 2*time.Hour)
	usageLedgerObserve("idx-1", "", 10, false, old)
	usageLedgerObserve("idx-1", "", 20, false, now)

	usageLedgerState.Lock()
	a := usageLedgerState.ledger.Accounts["idx-1"]
	usageLedgerState.Unlock()
	if _, ok := a.Hours[old.UTC().Format("2006-01-02T15")]; ok {
		t.Fatal("stale hour bucket survived pruning")
	}
	if a.Total.Requests != 2 {
		t.Fatalf("all-time totals must survive pruning: %+v", a.Total)
	}
}

func TestUsageNoteSegmentRendersAndFallsBack(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now.Add(-30*time.Minute))
	usageLedgerObserve("idx-1", "", 2000, true, now.Add(-10*time.Minute))

	seg := usageNoteSegmentFor("idx-1", now)
	if !strings.HasPrefix(seg, usageSegmentMarker) {
		t.Fatalf("segment marker missing: %q", seg)
	}
	if !strings.Contains(seg, "今日 请求2") || !strings.Contains(seg, "Tok 3.0k") {
		t.Fatalf("today stats missing: %q", seg)
	}
	if !strings.Contains(seg, "该账号暂未解析出额度窗口") {
		t.Fatalf("no-window fallback missing: %q", seg)
	}
	if !strings.Contains(seg, "累计 请求2 · 成功率50%") {
		t.Fatalf("all-time stats missing: %q", seg)
	}

	// No requests → no segment (never write placeholders over user notes).
	if got := usageNoteSegmentFor("idx-unknown", now); got != "" {
		t.Fatalf("unknown credential must render empty, got %q", got)
	}
}

func TestUsageWindowIntervalStats(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	now := time.Date(2026, 9, 27, 14, 30, 0, 0, time.UTC)
	usageLedgerObserve("idx-1", "", 1000, false, now.Add(-3*time.Hour)) // 11:30 — outside window
	usageLedgerObserve("idx-1", "", 2000, false, now.Add(-30*time.Minute))
	usageLedgerObserve("idx-1", "", 500, true, now.Add(-10*time.Minute))

	usageQuotaStamp("idx-1", "", "Pro",
		now.Add(-1*time.Hour).Format(time.RFC3339),
		now.Add(3*time.Hour).Format(time.RFC3339),
		820, 180, 1000)

	seg := usageNoteSegmentFor("idx-1", now)
	if strings.Contains(seg, "该账号暂未解析出额度窗口") {
		t.Fatalf("window bounds were stamped but the note says unparsed: %q", seg)
	}
	if !strings.Contains(seg, "窗口09-27 13:00→09-27 17:59 请求2 · Tok 2.5k") {
		t.Fatalf("window interval stats wrong: %q", seg)
	}
}

func TestUsageNoteWriteChangeGuarded(t *testing.T) {
	resetUsageNoteTestState(t)
	defer resetUsageNoteTestState(t)

	baseDoc := []byte(`{"type":"qoder","provider":"qoder","disabled":false,"note":"CN · 余820 已用180 池1000","auth":{"accessToken":"tok","region":"cn"},"account":{"uid":"u1"}}`)
	restoreSeams := stubPersistSeams(t, map[string][]byte{"a1": baseDoc})
	defer restoreSeams()

	now := time.Now()
	usageLedgerObserve("a1", "", 1234, false, now)

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
	if !strings.HasPrefix(note, "CN · 余820 已用180 池1000 · ") {
		t.Fatalf("lifecycle base not preserved ahead of the usage segment: %q", note)
	}
	if !strings.Contains(note, usageSegmentMarker) {
		t.Fatalf("usage segment missing: %q", note)
	}

	// Second write with unchanged data → change-guard skips the save.
	usageNoteWrite("a1")
	if n := len(saveCalls()); n != 1 {
		t.Fatalf("unchanged write must not save (watcher churn), got %d saves", n)
	}
}

func TestUsageStripAndExtractSegments(t *testing.T) {
	note := "CN · 余100 已用5 · 【用量】(自2026-09-24) 今日 请求1 · Tok 1k"
	if got := usageStripNote(note); got != "CN · 余100 已用5" {
		t.Fatalf("strip wrong: %q", got)
	}
	if got := usageSegmentFromNote(note); !strings.HasPrefix(got, usageSegmentMarker) {
		t.Fatalf("extract wrong: %q", got)
	}
	if got := usageStripNote("CN · 余100"); got != "CN · 余100" {
		t.Fatalf("strip must leave usage-free notes alone: %q", got)
	}
}

func TestDisplayNoteWithPrevCarriesUsageSegment(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{Nickname: "n1"}}
	prev := "CN · 余820 已用180 池1000 · 【用量】(自2026-09-24) 累计 请求3"
	note := displayNoteWithPrev(sa, nil, false, prev)
	if !strings.Contains(note, "余820") || !strings.Contains(note, usageSegmentMarker) {
		t.Fatalf("usage segment lost in display note: %q", note)
	}
	// The credits extractor must not swallow the usage segment.
	if got := creditSegmentFromNote(prev); strings.Contains(got, usageSegmentMarker) {
		t.Fatalf("credit segment absorbed the usage segment: %q", got)
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
