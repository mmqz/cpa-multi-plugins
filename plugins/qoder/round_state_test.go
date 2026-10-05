package main

// round_state_test.go — v0.8.57: pins the three fixes behind the field
// report "init 还是有概率不能签到" (account u673e7fcc, Intl, 2026-10-05):
//
//  1. the bypass-probe round memo survives host restarts (on-disk state,
//     fail-open);
//  2. the probe cooldown is per-ROUND, and a NOT_ELIGIBLE verdict latches
//     for 30min — not 6h across the 10:00 UTC+8 refresh;
//  3. the probe's skip/failure reasons surface in the check-in message
//     instead of the blind "今日暂无可领取权益";
//  plus the claimedCampaign extension: an empty-actionType CLAIMED row with
//  a credits-shaped benefit renders 今日已签到 instead of falling into the
//  probe dance.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withRoundStatePersistence points the state file at a temp dir and turns
// persistence on for the duration of one test.
func withRoundStatePersistence(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	on := true
	prevOn := roundStateEnabledTestOverride
	prevPath := roundStatePathOverride
	roundStateEnabledTestOverride = &on
	roundStatePathOverride = filepath.Join(dir, "rounds", roundStateFilename)
	resetRoundStateTest()
	t.Cleanup(func() {
		roundStateEnabledTestOverride = prevOn
		roundStatePathOverride = prevPath
		resetRoundStateTest()
	})
	return roundStatePathOverride
}

// TestRoundStateSurvivesRestart: remember → save → (simulated restart: fresh
// memo + re-armed load) → the probe candidate is back.
func TestRoundStateSurvivesRestart(t *testing.T) {
	path := withRoundStatePersistence(t)

	status := &campaignStatusResponse{Campaigns: []campaign{{
		CampaignID:  "camp-restart-daily",
		CampaignKey: "act-20260930-295",
		ActionType:  "CLAIM_BENEFIT",
		ClaimStatus: "CLAIMED",
		Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
	}}}
	rememberCampaignRound("intl", "u673e7fcc", status)
	forceSaveRoundState()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(raw), "camp-restart-daily") {
		t.Fatalf("state file missing the campaign id: %s", raw)
	}
	// 0600 file, 0700 dir — credentials-adjacent state stays private.
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file perm = %v, want 0600", fi.Mode().Perm())
	}

	// Simulated restart: brand-new memo, load re-armed.
	roundMemo = &campaignRoundMemo{perAccount: map[string]campaignRoundEntry{}, perRegion: map[string]campaignRoundEntry{}}
	resetRoundStateTest()
	loadRoundStateOnce()

	c, ok := roundMemo.probeFor("intl", "u673e7fcc")
	if !ok || c.CampaignID != "camp-restart-daily" {
		t.Fatalf("after restart probeFor = %+v ok=%v, want camp-restart-daily", c, ok)
	}
	c2, ok := roundMemo.probeFor("intl", "another-intl-account")
	if !ok || c2.CampaignID != "camp-restart-daily" {
		t.Fatalf("region fallback after restart = %+v ok=%v", c2, ok)
	}
}

// TestRoundStateLoadDropsStaleEntries: ids older than the memo freshness
// window must not come back from disk (they could have been recycled by a
// new round).
func TestRoundStateLoadDropsStaleEntries(t *testing.T) {
	path := withRoundStatePersistence(t)

	st := roundStateFile{
		Version: 1,
		SavedAt: time.Now().UTC().Format(time.RFC3339),
		PerRegion: map[string]campaignRoundEntry{
			"intl": {CampaignID: "camp-fresh", CampaignKey: "k1", SeenAt: time.Now().Add(-time.Hour)},
			"cn":   {CampaignID: "camp-stale", CampaignKey: "k2", SeenAt: time.Now().Add(-72 * time.Hour)},
		},
	}
	raw, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	roundMemo = &campaignRoundMemo{perAccount: map[string]campaignRoundEntry{}, perRegion: map[string]campaignRoundEntry{}}
	resetRoundStateTest()
	loadRoundStateOnce()

	if _, ok := roundMemo.probeFor("intl", "u"); !ok {
		t.Fatal("fresh persisted entry must stay probeable")
	}
	if _, ok := roundMemo.probeFor("cn", "u"); ok {
		t.Fatal("stale persisted entry must be dropped on load")
	}
}

// TestRoundStateDisabledLeavesNoFile: with persistence off (test-binary
// default), remember must not write anything anywhere.
func TestRoundStateDisabledLeavesNoFile(t *testing.T) {
	off := false
	prevOn := roundStateEnabledTestOverride
	prevPath := roundStatePathOverride
	roundStateEnabledTestOverride = &off
	roundStatePathOverride = filepath.Join(t.TempDir(), "should-not-exist.json")
	resetRoundStateTest()
	t.Cleanup(func() {
		roundStateEnabledTestOverride = prevOn
		roundStatePathOverride = prevPath
		resetRoundStateTest()
	})

	rememberCampaignRound("cn", "u", &campaignStatusResponse{Campaigns: []campaign{
		{CampaignID: "c1", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED"},
	}})
	if _, err := os.Stat(roundStatePathOverride); !os.IsNotExist(err) {
		t.Fatal("persistence disabled must not write a state file")
	}
}

// TestProbeLatchPerRound: a conclusive verdict on round R must not silence
// the probe for a NEW round id (region:uid:campaignID keying), and the TTL
// mapping is verdict-dependent (NOT_ELIGIBLE → 30min, claimed/blocked → 6h).
func TestProbeLatchPerRound(t *testing.T) {
	if got := probeCooldownFor("NOT_ELIGIBLE"); got != roundProbeEligibleCooldown {
		t.Fatalf("NOT_ELIGIBLE ttl = %v, want %v", got, roundProbeEligibleCooldown)
	}
	if got := probeCooldownFor("ALREADY_CLAIMED"); got != roundProbeCooldown {
		t.Fatalf("ALREADY ttl = %v, want %v", got, roundProbeCooldown)
	}
	if got := probeCooldownFor("BLOCKED"); got != roundProbeCooldown {
		t.Fatalf("BLOCKED ttl = %v, want %v", got, roundProbeCooldown)
	}

	claimHits := 0
	newBillingServer(t, "intl", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
		},
		"/sash/api/v1/me/campaigns/camp-new-round/claim": func(r *http.Request) (int, string) {
			claimHits++
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-intl-test", Region: "intl"}, Account: storedAccount{UID: "u673e7fcc"}}

	prevLast := roundProbeLast
	t.Cleanup(func() { roundProbeLast = prevLast })
	roundProbeLast = map[string]roundProbeLatch{}
	// A sibling claim already answered for the OLD round an hour ago (6h
	// latch fresh).
	roundProbeLast["intl:u673e7fcc:camp-old-round"] = roundProbeLatch{At: time.Now().Add(-time.Hour), TTL: roundProbeCooldown}

	// The list hid the old round and a NEW round id was seen this morning —
	// the old latch must not block the new round's probe.
	status := &campaignStatusResponse{Campaigns: []campaign{{
		CampaignID: "camp-new-round", CampaignKey: "act-20261005-001",
		ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE",
		Benefit: &campaignBen{Kind: "CREDITS", Amount: 100},
	}}}
	rememberCampaignRound("intl", "u673e7fcc", status)

	res, note := probeHiddenRound(sa)
	if claimHits != 1 {
		t.Fatalf("new-round probe blocked by old-round latch (hits=%d): note=%q", claimHits, note)
	}
	if res == nil || res["success"] != true {
		t.Fatalf("probe res = %v note=%q, want a landed grant", res, note)
	}
}

// TestProbeNotesSurface: every silent path of the old probe now returns a
// panel-renderable note.
func TestProbeNotesSurface(t *testing.T) {
	t.Run("cold memo", func(t *testing.T) {
		newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
			"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
				return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
			},
		})
		res, err := performCheckinCall(cnAuth())
		if err != nil {
			t.Fatalf("performCheckinCall: %v", err)
		}
		msg, _ := res["message"].(string)
		if !strings.Contains(msg, "绕过列表直探未执行") || !strings.Contains(msg, "每日轮次 id") {
			t.Fatalf("cold-memo probe must explain itself, got %q", msg)
		}
		if result, _ := res["result"].(string); result != "NOTHING_CLAIMABLE" {
			t.Fatalf("result = %v, want NOTHING_CLAIMABLE", res["result"])
		}
	})

	t.Run("inconclusive upstream stays retryable and says so", func(t *testing.T) {
		claimHits := 0
		newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
			"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
				return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
			},
			"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
				claimHits++
				return http.StatusServiceUnavailable, `{"errorCode":"RISK_DEPENDENCY_UNAVAILABLE"}`
			},
		})
		seedRoundMemo(t, "camp-cn-daily", "CLAIMABLE")
		for i := 0; i < 2; i++ {
			res, err := performCheckinCall(cnAuth())
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
			msg, _ := res["message"].(string)
			if !strings.Contains(msg, "http 503") {
				t.Fatalf("run %d: inconclusive probe must carry the upstream answer, got %q", i, msg)
			}
		}
		if claimHits != 2 {
			t.Fatalf("inconclusive probes must never latch (hits=%d, want 2)", claimHits)
		}
	})
}

// TestProbeNotEligibleShortLatch: a NOT_ELIGIBLE verdict arms only the 30min
// cooldown — the 10:00 UTC+8 refresh can flip the state, so a latch armed
// before the refresh must be gone by the afternoon ticks, while a 6h
// claimed/blocked latch would still hold.
func TestProbeNotEligibleShortLatch(t *testing.T) {
	claimHits := 0
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			claimHits++
			return http.StatusOK, `{"status":"FAILED","failureCode":"REDEMPTION_CODE_OUT_OF_STOCK"}`
		},
	})
	seedRoundMemo(t, "camp-cn-daily", "CLAIMABLE")
	sa := cnAuth()

	prevLast := roundProbeLast
	t.Cleanup(func() { roundProbeLast = prevLast })
	roundProbeLast = map[string]roundProbeLatch{}

	// First tick: probe fires, upstream says out-of-stock, latch arms.
	if _, err := performCheckinCall(sa); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if claimHits != 1 {
		t.Fatalf("hits=%d, want 1 after the first tick", claimHits)
	}
	latch, ok := roundProbeLast["cn::camp-cn-daily"]
	if !ok {
		t.Fatal("NOT_ELIGIBLE verdict must arm the per-round latch")
	}
	if latch.TTL != roundProbeEligibleCooldown {
		t.Fatalf("latch TTL = %v, want %v (30min — must not span the refresh)", latch.TTL, roundProbeEligibleCooldown)
	}

	// Second tick 31 minutes later (simulate the clock having passed the
	// TTL): the probe must fire again.
	latch.At = latch.At.Add(-31 * time.Minute)
	roundProbeLast["cn::camp-cn-daily"] = latch
	if _, err := performCheckinCall(sa); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if claimHits != 2 {
		t.Fatalf("hits=%d, want 2 — an expired NOT_ELIGIBLE latch must re-probe", claimHits)
	}

	// Contrast: a 6h claimed/blocked latch armed 5h ago would still hold.
	roundProbeLast["cn::camp-cn-daily"] = roundProbeLatch{At: time.Now().Add(-5 * time.Hour), TTL: roundProbeCooldown}
	if _, err := performCheckinCall(sa); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if claimHits != 2 {
		t.Fatalf("hits=%d, want 2 — a fresh 6h latch must still suppress the probe", claimHits)
	}
}

// TestClaimedCampaignEmptyActionType: the intl face sometimes ships the
// daily row without an actionType; a CLAIMED such row must read as 今日已签,
// not as nothing-claimable.
func TestClaimedCampaignEmptyActionType(t *testing.T) {
	claimed := &campaignStatusResponse{Campaigns: []campaign{
		{CampaignID: "c-view", CampaignKey: "act-20260901-922", ActionType: "VIEW_DETAILS", ClaimStatus: "CLAIMABLE"},
		{CampaignID: "c-daily", CampaignKey: "act-20260930-295", ActionType: "", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}},
	}}
	if got := claimedCampaign(claimed); got == nil || got.CampaignID != "c-daily" {
		t.Fatalf("claimedCampaign = %+v, want c-daily (empty actionType + credits)", got)
	}

	// A CLAIMED redemption-shaped row is NOT today's check-in (coupons ride
	// their own verdict path).
	coupon := &campaignStatusResponse{Campaigns: []campaign{
		{CampaignID: "c-coupon", ActionType: "", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "REDEMPTION_CODE"}},
	}}
	if got := claimedCampaign(coupon); got != nil {
		t.Fatalf("claimedCampaign must skip redemption-shaped rows, got %+v", got)
	}

	// Explicit CLAIM_BENEFIT + CLAIMED still matches (historical shape).
	explicit := &campaignStatusResponse{Campaigns: []campaign{
		{CampaignID: "c-x", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED"},
	}}
	if got := claimedCampaign(explicit); got == nil || got.CampaignID != "c-x" {
		t.Fatalf("claimedCampaign lost the CLAIM_BENEFIT match, got %+v", got)
	}
}

// TestCheckinHoursIncludeAfternoonCompensation: the schedule must hold a
// mid-day tick between the 10:00 refresh and the 21:00 companion.
func TestCheckinHoursIncludeAfternoonCompensation(t *testing.T) {
	var has10, has15, has21 bool
	for _, h := range checkinHours {
		switch h {
		case 10:
			has10 = true
		case 15:
			has15 = true
		case 21:
			has21 = true
		}
	}
	if !has10 || !has15 || !has21 {
		t.Fatalf("checkinHours = %v, want 10/15/21 (refresh + compensation + evening)", checkinHours)
	}
}
