// usage_note.go — v0.8.28: per-credential usage & quota summary rendered
// into the auth file's `note` — the note is the credential display field the
// HOST's local credential management (本地凭据管理) already renders, so this
// puts 用量/额度 where the user actually looks, NOT inside the plugin panel
// (the panel keeps its richer views; this is the credential-card summary).
//
// Design (mirrors the cockpit-tools credential usage card, adapted to what a
// static note can carry):
//   - attribution: every request is matched to its credential by the host's
//     UsagePlugin record (AuthID / AuthIndex — 当前登录凭证和凭证序号);
//   - 凭证总体用量: request count, token count, success rate over
//     今日 / 累计 (ledger lifetime);
//   - 额度窗口: when the plugin's billing snapshots expose window
//     boundaries (qoder package cycles), interval stats are computed for
//     [start, end) from the hourly buckets; otherwise the note says
//     该账号暂未解析出额度窗口;
//   - 标准额度: the account-level pool (余/已用/池) already rides the
//     credits segment owned by the lifecycle writer — not duplicated here.
//   - 预估花费: omitted — qoder exposes no authoritative per-token price
//     (price_factor is a relative multiplier); inventing a price would be
//     a wrong answer, not an estimate.
//
// Data: the host delivers one UsagePlugin record per completed request.
// The ledger keeps all-time totals, 35 daily buckets and 72 hourly buckets
// per credential, persisted to `<authDirParent>/.workbuddy-usage.json` — the
// PARENT of the auth dir (v0.9.46: the host's 本地凭据管理 lists every
// auth-dir file no plugin claims as a credential, so even this dot-prefixed
// sidecar showed up there as 平台 unknown 未识别凭证 noise — user report
// 2026-09-30; the parent dir is not scanned by the host). The dot prefix
// stays as belt-and-braces; a pre-relocation ledger inside the auth dir
// migrates once on first load. Flushes are throttled and change-guarded.
// Note writes are per-credential throttled (60s) and change-guarded — every
// host.auth.save re-fires the watcher, so the loop must converge.
//
// Ownership: the note is shared. The lifecycle writer owns the prefix +
// credits segment and preserves the 【用量】 segment; this writer owns the
// 【用量】 segment and preserves everything else. Both read-merge-write the
// whole document through the hostAuthSaveJSON funnel (which keeps the
// model_cache snapshot alive).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// usageNoteDocKey is the note prefix marker owned by this writer. Everything
// after it in the note belongs here; everything before belongs to the
// lifecycle writer.
const usageSegmentMarker = "【用量】"

// usageLedgerRetention bounds the bucket maps so the JSON stays small.
const (
	usageHourRetention = 72 * time.Hour
	usageDayRetention  = 35 * 24 * time.Hour
	usageFlushInterval = 30 * time.Second
	usageNoteThrottle  = 60 * time.Second
	usageLedgerVersion = 1
)

// usageTotals is one bucket of counters.
type usageTotals struct {
	Requests int64 `json:"requests"`
	Success  int64 `json:"success"`
	Failed   int64 `json:"failed"`
	Tokens   int64 `json:"tokens"`
}

func (t *usageTotals) add(tokens int64, failed bool) {
	t.Requests++
	if failed {
		t.Failed++
	} else {
		t.Success++
	}
	t.Tokens += tokens
}

// usageQuotaState is the last-known billing snapshot folded into the ledger
// whenever the plugin refreshes billing data anyway (panel, scheduler) — the
// note writer makes NO upstream calls of its own.
type usageQuotaState struct {
	Plan        string `json:"plan,omitempty"`
	WindowStart string `json:"window_start,omitempty"` // RFC3339 or provider-native
	WindowEnd   string `json:"window_end,omitempty"`
	Remain      int64  `json:"remain,omitempty"`
	Used        int64  `json:"used,omitempty"`
	Total       int64  `json:"total,omitempty"`
	FetchedAt   string `json:"fetched_at,omitempty"`
}

// usageAccount is one credential's ledger.
type usageAccount struct {
	AuthID    string                  `json:"auth_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Total     usageTotals             `json:"total"`
	Days      map[string]*usageTotals `json:"days,omitempty"`  // UTC "2006-01-02"
	Hours     map[string]*usageTotals `json:"hours,omitempty"` // UTC "2006-01-02T15"
	Quota     *usageQuotaState        `json:"quota,omitempty"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
}

// usageLedger is the persisted document shape.
type usageLedger struct {
	Version   int                      `json:"version"`
	StartedAt string                   `json:"started_at"`
	Accounts  map[string]*usageAccount `json:"accounts"` // key: auth_index (fallback auth_id)
}

// usageLedgerState is the in-memory ledger + flush bookkeeping.
var usageLedgerState struct {
	sync.Mutex
	ledger    *usageLedger
	loaded    bool
	dirty     bool
	lastFlush time.Time
	lastWrite map[string]time.Time // authIndex → last note write
}

// usageDataDir resolves the directory for the ledger sidecar file — the
// auth dir of any credential known to the host (same source peerAuthDir
// uses for deletes). Cached for the process lifetime after first hit; empty
// until then (ledger stays in-memory only).
var usageDataDirOnce struct {
	sync.Mutex
	dir  string
	done bool
}

func usageDataDir() string {
	usageDataDirOnce.Lock()
	defer usageDataDirOnce.Unlock()
	if usageDataDirOnce.done {
		return usageDataDirOnce.dir
	}
	usageDataDirOnce.done = true
	if dir := strings.TrimSpace(peerAuthDir()); dir != "" {
		usageDataDirOnce.dir = dir
	}
	return usageDataDirOnce.dir
}

// usageLedgerPath resolves the ledger file — in the PARENT of the auth dir
// (the host's unrecognized-credential list scans the auth dir itself; the
// parent is not scanned). Empty when the auth dir is not known yet
// (in-memory only until the first host listing provides a path).
func usageLedgerPath() string {
	dir := usageLedgerDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "."+providerName+"-usage.json")
}

// usageLedgerDir is the ledger's home directory: the parent of the auth
// dir. Empty while the auth dir is unknown.
func usageLedgerDir() string {
	dir := usageDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Dir(dir)
}

// usageLedgerLegacyPath is the pre-relocation location inside the auth dir.
func usageLedgerLegacyPath() string {
	dir := usageDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "."+providerName+"-usage.json")
}

// usageLedgerMigrateLegacy moves a pre-relocation ledger to its new home.
// One-time, best effort: same-filesystem rename; on any failure the old
// file stays put and the new-world ledger simply starts empty.
func usageLedgerMigrateLegacy(newPath string) {
	legacy := usageLedgerLegacyPath()
	if legacy == "" || legacy == newPath {
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		return // new-world file already exists — keep both as they are
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	_ = os.Rename(legacy, newPath)
}

// usageLedgerLocked returns the ledger, loading it on first use. Caller must
// hold usageLedgerState.Lock — every entry point takes the mutex exactly once
// (a nested load would self-deadlock on the non-reentrant mutex).
func usageLedgerLocked() *usageLedger {
	if usageLedgerState.loaded {
		return usageLedgerState.ledger
	}
	usageLedgerState.loaded = true
	led := &usageLedger{Version: usageLedgerVersion, StartedAt: time.Now().UTC().Format(time.RFC3339), Accounts: map[string]*usageAccount{}}
	if p := usageLedgerPath(); p != "" {
		usageLedgerMigrateLegacy(p)
		if raw, err := os.ReadFile(p); err == nil {
			var stored usageLedger
			if json.Unmarshal(raw, &stored) == nil && stored.Accounts != nil {
				led = &stored
				if led.Accounts == nil {
					led.Accounts = map[string]*usageAccount{}
				}
			}
		}
	}
	usageLedgerState.ledger = led
	return led
}

// usageLedgerFlushIfDue persists the ledger when dirty and the throttle
// allows; force bypasses the throttle (billing fold-ins and note writes).
func usageLedgerFlushIfDue(force bool) {
	usageLedgerState.Lock()
	defer usageLedgerState.Unlock()
	if !usageLedgerState.dirty || usageLedgerState.ledger == nil {
		return
	}
	if !force && time.Since(usageLedgerState.lastFlush) < usageFlushInterval {
		return
	}
	p := usageLedgerPath()
	if p == "" {
		return
	}
	raw, err := json.Marshal(usageLedgerState.ledger)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	usageLedgerState.dirty = false
	usageLedgerState.lastFlush = time.Now()
}

// usageLedgerObserve records one completed request. authIndex is the
// preferred key (凭证序号); authID is the fallback when the host omits it.
func usageLedgerObserve(authIndex, authID string, tokens int64, failed bool, at time.Time) {
	if authIndex == "" {
		authIndex = authID
	}
	if authIndex == "" {
		return
	}
	usageLedgerState.Lock()
	led := usageLedgerLocked()
	a := led.Accounts[authIndex]
	if a == nil {
		a = &usageAccount{AuthID: authID, Days: map[string]*usageTotals{}, Hours: map[string]*usageTotals{}}
		led.Accounts[authIndex] = a
	}
	if a.Days == nil {
		a.Days = map[string]*usageTotals{}
	}
	if a.Hours == nil {
		a.Hours = map[string]*usageTotals{}
	}
	if authID != "" {
		a.AuthID = authID
	}
	a.Total.add(tokens, failed)
	dayKey := at.UTC().Format("2006-01-02")
	if d, ok := a.Days[dayKey]; ok {
		d.add(tokens, failed)
	} else {
		d := &usageTotals{}
		d.add(tokens, failed)
		a.Days[dayKey] = d
	}
	hourKey := at.UTC().Format("2006-01-02T15")
	if h, ok := a.Hours[hourKey]; ok {
		h.add(tokens, failed)
	} else {
		h := &usageTotals{}
		h.add(tokens, failed)
		a.Hours[hourKey] = h
	}
	usagePruneBuckets(a, at)
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	usageLedgerState.dirty = true
	usageLedgerState.Unlock()
	usageLedgerFlushIfDue(false)
}

// usagePruneBuckets drops buckets beyond retention. Caller holds the lock.
func usagePruneBuckets(a *usageAccount, now time.Time) {
	horizonH := now.UTC().Add(-usageHourRetention).Format("2006-01-02T15")
	for k := range a.Hours {
		if k < horizonH {
			delete(a.Hours, k)
		}
	}
	horizonD := now.UTC().Add(-usageDayRetention).Format("2006-01-02")
	for k := range a.Days {
		if k < horizonD {
			delete(a.Days, k)
		}
	}
}

// usageQuotaStamp folds a billing snapshot into the ledger (no upstream
// calls — this rides data the plugin fetched anyway). Safe to call with
// zero-value fields; empty Window bounds mean "no window parsed".
func usageQuotaStamp(authIndex, authID, plan, windowStart, windowEnd string, remain, used, total int64) {
	if authIndex == "" {
		authIndex = authID
	}
	if authIndex == "" {
		return
	}
	usageLedgerState.Lock()
	led := usageLedgerLocked()
	a := led.Accounts[authIndex]
	if a == nil {
		// Key-alias resolution: callers key by auth_index OR auth_id (the
		// panel cache uses auth.ID); the ledger stores whichever the usage
		// records carried. Match the stored AuthID before giving up.
		for _, cand := range led.Accounts {
			if cand.AuthID == authIndex {
				a = cand
				break
			}
		}
	}
	if a == nil {
		usageLedgerState.Unlock()
		return // no requests observed yet — the note has nothing to refresh anyway
	}
	a.Quota = &usageQuotaState{
		Plan:        plan,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Remain:      remain,
		Used:        used,
		Total:       total,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	usageLedgerState.dirty = true
	usageLedgerState.Unlock()
	usageLedgerFlushIfDue(true)
}

// usageSumHours sums the hourly buckets whose key falls in [fromKey, toKey]
// (UTC "2006-01-02T15" lexicographic order = chronological order).
func usageSumHours(a *usageAccount, fromKey, toKey string) usageTotals {
	var out usageTotals
	for k, h := range a.Hours {
		if k >= fromKey && k <= toKey {
			out.Requests += h.Requests
			out.Success += h.Success
			out.Failed += h.Failed
			out.Tokens += h.Tokens
		}
	}
	return out
}

// usageWindowBounds resolves the credential's current quota window in UTC
// hour keys. qoder's cycle bounds are provider-native strings parsed
// defensively; (false, ...) when no window is known.
func usageWindowBounds(a *usageAccount, now time.Time) (bool, string, string) {
	if a.Quota == nil || a.Quota.WindowStart == "" || a.Quota.WindowEnd == "" {
		return false, "", ""
	}
	start, sErr := parseQuotaTime(a.Quota.WindowStart)
	end, eErr := parseQuotaTime(a.Quota.WindowEnd)
	if sErr != nil || eErr != nil || !end.After(start) {
		return false, "", ""
	}
	if now.Before(start) || now.After(end.Add(time.Hour)) {
		// Window over and not renewed since — treat as no current window.
		return false, "", ""
	}
	return true, start.UTC().Format("2006-01-02T15"), end.UTC().Add(-time.Second).UTC().Format("2006-01-02T15")
}

// parseQuotaTime accepts RFC3339, "2006-01-02 15:04:05" and
// "2006-01-02T15:04" shapes seen across provider billing payloads.
func parseQuotaTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable quota time %q", s)
}

// usageHumanTokens renders token counts compactly (1.2k / 3.4M / 1.1G).
func usageHumanTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fG", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// usageNoteSegmentFor renders the 【用量】 note segment for one credential
// from the ledger. Returns "" when nothing was observed yet (never write a
// placeholder over a note the user may have hand-edited).
func usageNoteSegmentFor(authIndex string, now time.Time) string {
	usageLedgerState.Lock()
	defer usageLedgerState.Unlock()
	led := usageLedgerLocked()
	a := led.Accounts[authIndex]
	if a == nil || a.Total.Requests == 0 {
		return ""
	}
	start := led.StartedAt
	if len(start) < 10 {
		start = now.UTC().Format(time.RFC3339)
	}
	var b strings.Builder
	b.WriteString(usageSegmentMarker)
	fmt.Fprintf(&b, "(自%s)", start[:10])
	fmt.Fprintf(&b, " 今日 请求%d · Tok %s", usageDayTotal(a, now), usageHumanTokens(usageDayTokens(a, now)))
	if ok, fromKey, toKey := usageWindowBounds(a, now); ok {
		w := usageSumHours(a, fromKey, toKey)
		fmt.Fprintf(&b, " ｜ 窗口%s→%s 请求%d · Tok %s", usageHourLabel(fromKey), usageHourLabelEnd(toKey), w.Requests, usageHumanTokens(w.Tokens))
	} else {
		b.WriteString(" ｜ 该账号暂未解析出额度窗口")
	}
	fmt.Fprintf(&b, " ｜ 累计 请求%d · 成功率%.0f%% · Tok %s", a.Total.Requests, usageSuccessRate(a.Total), usageHumanTokens(a.Total.Tokens))
	return b.String()
}

// usageHourLabel renders hour key "2006-01-02T15" as "09-27 11:00".
func usageHourLabel(key string) string {
	t, err := time.Parse("2006-01-02T15", key)
	if err != nil {
		return key
	}
	return t.Format("01-02 15:00")
}

// usageHourLabelEnd renders the inclusive end hour key as "09-27 16:59".
func usageHourLabelEnd(key string) string {
	t, err := time.Parse("2006-01-02T15", key)
	if err != nil {
		return key
	}
	return t.Add(59 * time.Minute).Format("01-02 15:04")
}

func usageDayTotal(a *usageAccount, now time.Time) int64 {
	if d, ok := a.Days[now.UTC().Format("2006-01-02")]; ok {
		return d.Requests
	}
	return 0
}

func usageDayTokens(a *usageAccount, now time.Time) int64 {
	if d, ok := a.Days[now.UTC().Format("2006-01-02")]; ok {
		return d.Tokens
	}
	return 0
}

func usageSuccessRate(t usageTotals) float64 {
	if t.Requests == 0 {
		return 0
	}
	return float64(t.Success) / float64(t.Requests) * 100
}

// usageSegmentFromNote extracts the 【用量】 segment from an existing note.
func usageSegmentFromNote(note string) string {
	idx := strings.Index(note, usageSegmentMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(note[idx:])
}

// usageStripNote removes the 【用量】 segment — the lifecycle-owned base the
// change-guards compare (usage-only changes must not echo lifecycle saves).
func usageStripNote(note string) string {
	idx := strings.Index(note, usageSegmentMarker)
	if idx < 0 {
		return note
	}
	return strings.TrimSpace(strings.TrimRight(note[:idx], " ·"))
}

// usageNoteRefreshSoon re-renders and persists the 【用量】 segment for one
// credential, throttled per credential (usage records can burst). Runs in
// the caller's goroutine — callers invoke it via `go` so the host's usage
// pump never blocks on host RPCs.
func usageNoteRefreshSoon(authIndex, authID string) {
	if authIndex == "" {
		authIndex = authID
	}
	if authIndex == "" {
		return
	}
	usageLedgerState.Lock()
	last, ok := usageLedgerState.lastWrite[authIndex]
	if !ok {
		last, ok = usageLedgerState.lastWrite[authID]
	}
	if ok && time.Since(last) < usageNoteThrottle {
		usageLedgerState.Unlock()
		return
	}
	if usageLedgerState.lastWrite == nil {
		usageLedgerState.lastWrite = map[string]time.Time{}
	}
	usageLedgerState.lastWrite[authIndex] = time.Now()
	usageLedgerState.lastWrite[authID] = time.Now()
	usageLedgerState.Unlock()
	usageNoteWrite(authIndex)
}

// usageNoteWrite composes the new note (lifecycle base + fresh usage
// segment) and saves only on change.
func usageNoteWrite(authIndex string) {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil || len(phys.JSON) == 0 {
		phys = usagePhysicalByAlias(authIndex)
		if phys == nil {
			return
		}
	}
	var probe struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(phys.JSON, &probe) != nil {
		return
	}
	seg := usageNoteSegmentFor(authIndex, time.Now())
	if seg == "" {
		return
	}
	base := usageStripNote(probe.Note)
	next := base
	if next != "" {
		next += " · "
	}
	next += seg
	if next == probe.Note {
		return // change-guard: no save, no watcher churn
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(phys.JSON, &m); err != nil {
		return
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return
	}
	m["note"] = raw
	doc, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := hostAuthSaveJSONFn(phys.Name, doc); err != nil {
		return
	}
	usageLedgerFlushIfDue(true)
}

// usageQuotaStampFromCredits folds a qoder creditsSummary into the ledger.
// The window is the widest package cycle currently active (qoder's reset
// bounds are per-package; a single widest window is what a credential-card
// note can meaningfully display). Empty cycles → no window parsed.
func usageQuotaStampFromCredits(authID, plan string, cr *creditsSummary) {
	start, end := "", ""
	for i, p := range cr.Packages {
		if strings.TrimSpace(p.CycleStart) == "" || strings.TrimSpace(p.CycleEnd) == "" {
			continue
		}
		if start == "" || p.CycleStart < start {
			start = p.CycleStart
		}
		if end == "" || p.CycleEnd > end {
			end = p.CycleEnd
		}
		_ = i
	}
	usageQuotaStamp("", authID, plan, start, end, cr.TotalRemain, cr.TotalUsed, cr.TotalSize)
}

// usagePhysicalByAlias resolves a credential document when the ledger key is
// an auth_id (host record id) rather than an auth_index: the host listing is
// scanned for a matching ID / AuthIndex / Name.
func usagePhysicalByAlias(key string) *hostAuthPhysical {
	files, err := hostAuthListFn()
	if err != nil {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(key))
	for _, f := range files {
		idMatch := strings.EqualFold(strings.TrimSpace(f.ID), want)
		idxMatch := strings.EqualFold(strings.TrimSpace(f.AuthIndex), want)
		nameMatch := strings.EqualFold(strings.TrimSpace(f.Name), want) ||
			strings.EqualFold(strings.TrimSpace(f.Name), want+".json")
		if !idMatch && !idxMatch && !nameMatch {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err == nil && phys != nil {
			return phys
		}
	}
	return nil
}
