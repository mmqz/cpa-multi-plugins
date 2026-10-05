package main

// round_state.go — v0.8.57: persist the bypass-probe round memo across host
// restarts.
//
// Field report 2026-10-05 (account u673e7fcc, Intl): "init 还是有概率不能签到"
// — check-in reported the generic "今日暂无可领取权益" (reason=none) on days
// the upstream hid the daily CLAIM_BENEFIT row (per-person dedup /
// device-targeted filtering / gray cohorts, issue #27). The bypass-list
// verdict probe (probeHiddenRound) is exactly the mechanism that still gets
// a real answer in that state — but its campaign-id memo (roundMemo) lived
// only in process memory, so EVERY host restart dropped it. On a deployment
// with a single Intl account there is no sibling to re-warm the region memo,
// and a restart before the first visible row meant the probe stayed dead for
// the whole day: the list hid the row, no id existed to POST, and check-in
// fell through to the blind "none" diagnosis. Whether a given day worked
// thus depended on whether the host happened to restart — "还是有概率".
//
// Fix: serialize the memo (per-account + per-region entries) to a small JSON
// state file under the user's home dir, debounced. Load happens once when
// the plugin process starts serving campaigns reads (first remember/probe).
// Everything is fail-open: any read/write error silently degrades to the
// previous in-memory-only behavior — a broken state file must never take
// check-in down.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// roundStateEnabledTestOverride: nil in production. Tests set it explicitly
// (persistence tests flip it on with a temp-dir path; all other tests leave
// it nil and get the lazy test-binary detection below, which keeps disk
// I/O off so the suite stays hermetic).
var roundStateEnabledTestOverride *bool

var (
	roundStateEnabledOnce sync.Once
	roundStateEnabledVal  bool
)

// roundStatePersistEnabled reports whether disk persistence is active.
// Detected lazily (not at package init) because the testing flag "test.v"
// is only registered after testing.Init() runs — an init-time flag.Lookup
// would misread test binaries as production and let the suite touch $HOME.
func roundStatePersistEnabled() bool {
	if roundStateEnabledTestOverride != nil {
		return *roundStateEnabledTestOverride
	}
	roundStateEnabledOnce.Do(func() {
		roundStateEnabledVal = flag.Lookup("test.v") == nil
	})
	return roundStateEnabledVal
}

// roundStatePathOverride lets tests point the state file at a temp dir.
var roundStatePathOverride string

const roundStateFilename = "qoder_campaign_rounds.json"

// roundStateSaveInterval debounces writes: dashboard reads refresh the memo
// on every campaigns fetch, but the file only needs the latest state — one
// write per active window is plenty (a crash may lose the last few minutes
// of memo refreshes; the next visible row re-warms it).
const roundStateSaveInterval = 30 * time.Second

var (
	roundStateMu      sync.Mutex
	roundStateLoaded  bool
	roundStateLastMod time.Time
)

// loadRoundStateOnce hydrates the memo from disk the first time it runs
// (idempotent — roundStateLoaded guards under roundStateMu, and
// resetRoundStateTest re-arms it for hermetic persistence tests).
func loadRoundStateOnce() {
	loadRoundState()
}

// roundStateFile is the on-disk shape. Keys mirror roundMemo's maps:
// perAccount "region:uid" → entry, perRegion region → entry.
type roundStateFile struct {
	Version    int                           `json:"version"`
	SavedAt    string                        `json:"saved_at"`
	PerAccount map[string]campaignRoundEntry `json:"per_account,omitempty"`
	PerRegion  map[string]campaignRoundEntry `json:"per_region,omitempty"`
}

func roundStatePath() string {
	if roundStatePathOverride != "" {
		return roundStatePathOverride
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cpa-multi-plugins", roundStateFilename)
}

// loadRoundState hydrates roundMemo from disk once per process. Entries
// older than roundMemoMaxAge are dropped on load (a stale id must not be
// probed — same rule probeFor enforces in memory).
func loadRoundState() {
	roundStateMu.Lock()
	defer roundStateMu.Unlock()
	if roundStateLoaded || !roundStatePersistEnabled() {
		return
	}
	path := roundStatePath()
	if path == "" {
		roundStateLoaded = true
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		roundStateLoaded = true // first run / unreadable → in-memory only
		return
	}
	var st roundStateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		roundStateLoaded = true // corrupt file → ignore, never block check-in
		return
	}
	cutoff := time.Now().Add(-roundMemoMaxAge)
	roundMemo.mu.Lock()
	for k, e := range st.PerAccount {
		if e.CampaignID != "" && e.SeenAt.After(cutoff) {
			roundMemo.perAccount[k] = e
		}
	}
	for k, e := range st.PerRegion {
		if e.CampaignID != "" && e.SeenAt.After(cutoff) {
			roundMemo.perRegion[k] = e
		}
	}
	roundMemo.mu.Unlock()
	roundStateLoaded = true
}

// saveRoundState writes the current memo to disk (debounced, atomic
// tmp+rename, 0600). Callers treat every failure as a no-op.
func saveRoundState() {
	roundStateMu.Lock()
	defer roundStateMu.Unlock()
	if !roundStatePersistEnabled() {
		return
	}
	if !roundStateLastMod.IsZero() && time.Since(roundStateLastMod) < roundStateSaveInterval {
		return // debounced
	}
	path := roundStatePath()
	if path == "" {
		return
	}
	roundMemo.mu.Lock()
	st := roundStateFile{
		Version:    1,
		SavedAt:    time.Now().UTC().Format(time.RFC3339),
		PerAccount: make(map[string]campaignRoundEntry, len(roundMemo.perAccount)),
		PerRegion:  make(map[string]campaignRoundEntry, len(roundMemo.perRegion)),
	}
	for k, e := range roundMemo.perAccount {
		st.PerAccount[k] = e
	}
	for k, e := range roundMemo.perRegion {
		st.PerRegion[k] = e
	}
	roundMemo.mu.Unlock()
	raw, err := json.MarshalIndent(&st, "", " ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, roundStateFilename+".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		return
	}
	ok = true
	roundStateLastMod = time.Now()
}

// forceSaveRoundState bypasses the debounce (tests; shutdown paths).
func forceSaveRoundState() {
	roundStateMu.Lock()
	roundStateLastMod = time.Time{}
	roundStateMu.Unlock()
	saveRoundState()
}

// resetRoundStateTest re-arms load state for tests (temp-path override set).
func resetRoundStateTest() {
	roundStateMu.Lock()
	roundStateLoaded = false
	roundStateLastMod = time.Time{}
	roundStateMu.Unlock()
}
