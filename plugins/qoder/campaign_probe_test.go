// campaign_probe_test.go — v0.8.39: pins the three mechanisms that turn the
// u673e7fcc field report ("当前没有可领取的活动") from a dead end into either
// a landed grant or an authoritative verdict:
//
//  1. per-uid derived machine identities (the region cache must never glue a
//     multi-account deployment onto one pseudo-device — that is what hides
//     the daily row behind upstream's per-person dedup);
//  2. empty-actionType daily rows are claimable (hub parity);
//  3. the bypass-list verdict probe: when the list hides the row, the last-
//     seen daily campaign id is POSTed once and the upstream answer decides
//     (+100 / ALREADY_CLAIMED / BLOCKED 同人已领), never a guess.
package main

import (
        "encoding/json"
        "fmt"
        "net/http"
        "strings"
        "testing"
        "time"
)

// TestDerivedIdentityCacheIsPerUID: a DERIVED identity must never be served
// from the region cache — two accounts on one host must present two different
// pseudo-devices (hub isolation doctrine), while a NATIVE identity stays
// region-cached (machine-level by design).
func TestDerivedIdentityCacheIsPerUID(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        a1 := machineIdentityFor("cn", "uid-a", false)
        b1 := machineIdentityFor("cn", "uid-b", false)
        if a1.Source != "derived" || b1.Source != "derived" {
                t.Fatalf("sources = %q / %q, want derived/derived", a1.Source, b1.Source)
        }
        if a1.MachineID == b1.MachineID || a1.MachineToken == b1.MachineToken {
                t.Fatal("two uids must derive two different pseudo-devices (cache bled one into the other)")
        }
        a2 := machineIdentityFor("cn", "uid-a", false)
        if a2.MachineID != a1.MachineID || a2.MachineToken != a1.MachineToken {
                t.Fatalf("per-uid derivation not stable across calls: %s vs %s", a1.MachineID, a2.MachineID)
        }

        // A cached NATIVE entry keeps serving every account in the region.
        native := machineIdentity{MachineID: "mid", MachineToken: "mtok", MachineType: "mtype",
                MachineCode: "mcode", MachineOS: "x86_64_win32", MachineHostname: "host",
                Source: identitySourceNative}
        machineIdentityCache.Store("cn", machineIdentityCacheEntry{at: time.Now(), id: native})
        t.Cleanup(func() { machineIdentityCache.Delete("cn") })
        for _, uid := range []string{"uid-a", "uid-b", "uid-c"} {
                if got := machineIdentityFor("cn", uid, false); got.MachineID != "mid" || got.MachineToken != "mtok" {
                        t.Fatalf("uid %s: native cache miss: %+v", uid, got)
                }
        }
}

// TestClaimableCampaignEmptyActionType: rows the server ships without an
// actionType are claimable when the benefit is credits-shaped, and skipped
// when it is not (subscription/Pro-shaped rows belong to the Pro flow).
func TestClaimableCampaignEmptyActionType(t *testing.T) {
        status := &campaignStatusResponse{Campaigns: []campaign{
                {CampaignID: "c-pro", ActionType: "", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "PRO_TRIAL", Amount: 1}},
                {CampaignID: "c-daily", ActionType: "", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}},
        }}
        got := claimableCampaign(status)
        if got == nil || got.CampaignID != "c-daily" {
                t.Fatalf("empty-actionType CREDITS row must be claimable, got %+v", got)
        }
        // No benefit listed at all → still claimable (face value unreadable).
        status.Campaigns = []campaign{{CampaignID: "c-plain", ActionType: "", ClaimStatus: "CLAIMABLE"}}
        if got := claimableCampaign(status); got == nil {
                t.Fatal("empty-actionType benefit-less row must stay claimable")
        }
        // CLAIMED row never counts.
        status.Campaigns = []campaign{{CampaignID: "c-done", ActionType: "", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}}}
        if got := claimableCampaign(status); got != nil {
                t.Fatalf("CLAIMED row must not be claimable: %+v", got)
        }
}

// TestCheckinSummaryActiveWithEmptyActionType: the panel summary must report
// an active day when only an empty-actionType row exists — otherwise the
// dashboard shows 不可签 while the round is actually claimable.
func TestCheckinSummaryActiveWithEmptyActionType(t *testing.T) {
        sum := campaignCheckinSummary(&campaignStatusResponse{
                ShowCampaign: true,
                Campaigns: []campaign{{
                        CampaignID: "c1", ActionType: "", ClaimStatus: "CLAIMABLE",
                        Benefit: &campaignBen{Kind: "CREDITS", Amount: 100},
                }},
        })
        if !sum.Active || sum.DailyCredit != 100 {
                t.Fatalf("summary = %+v, want active with DailyCredit=100", sum)
        }
}

// TestProbeHiddenRoundGrants: the daily row vanished from the list (hub:
// per-person dedup hides it from the losers) but the round's claim endpoint
// still grants — the bypass probe must land the +100 with the response's own
// face value.
func TestProbeHiddenRoundGrants(t *testing.T) {
        claimHit := false
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
                },
                "/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
                        claimHit = true
                        return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
                },
        })
        seedRoundMemo(t, "camp-cn-daily", "CLAIMED")

        res, err := performCheckinCall(cnAuth())
        if err != nil {
                t.Fatalf("performCheckinCall: %v", err)
        }
        if !claimHit {
                t.Fatal("hidden row: the bypass probe must POST the last-seen campaign id")
        }
        if success, _ := res["success"].(bool); !success {
                t.Fatalf("probe grant must surface as success: %+v", res)
        }
        if rc, _ := res["rewardCredits"].(float64); rc != 100 {
                t.Fatalf("rewardCredits = %v, want 100 (from the claim response's benefit)", res["rewardCredits"])
        }
}

// TestProbeHiddenRoundAuthoritativeVerdicts: upstream answers ALREADY_CLAIMED
// (idempotent replay) and BLOCKED (same-person dedup) — both must come back
// typed instead of a guess.
func TestProbeHiddenRoundAuthoritativeVerdicts(t *testing.T) {
        t.Run("already", func(t *testing.T) {
                newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                        "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                                return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
                        },
                        "/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
                                return http.StatusConflict, `{"errorCode":"ALREADY_CLAIMED"}`
                        },
                })
                seedRoundMemo(t, "camp-cn-daily", "CLAIMED")
                res, err := performCheckinCall(cnAuth())
                if err != nil {
                        t.Fatalf("performCheckinCall: %v", err)
                }
                if result, _ := res["result"].(string); result != "ALREADY_CLAIMED" {
                        t.Fatalf("result = %v, want ALREADY_CLAIMED", res["result"])
                }
        })
        t.Run("blocked", func(t *testing.T) {
                newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                        "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                                return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
                        },
                        "/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
                                return http.StatusOK, `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`
                        },
                })
                seedRoundMemo(t, "camp-cn-daily", "CLAIMED")
                res, err := performCheckinCall(cnAuth())
                if err != nil {
                        t.Fatalf("performCheckinCall: %v", err)
                }
                if result, _ := res["result"].(string); result != "BLOCKED" {
                        t.Fatalf("result = %v, want BLOCKED", res["result"])
                }
                if msg, _ := res["message"].(string); !strings.Contains(msg, "同人已领取") {
                        t.Fatalf("blocked message must carry the dedup verdict, got %q", msg)
                }
        })
}

// TestProbeHiddenRoundCooldownAndNoFabrication: the probe fires at most once
// per account per cooldown, and never fabricates a claim when this deployment
// has never seen a daily row.
func TestProbeHiddenRoundCooldownAndNoFabrication(t *testing.T) {
        claimHits := 0
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
                },
                "/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
                        claimHits++
                        return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
                },
        })
        seedRoundMemo(t, "camp-cn-daily", "CLAIMED")

        for i := 0; i < 3; i++ {
                if _, err := performCheckinCall(cnAuth()); err != nil {
                        t.Fatalf("run %d: %v", i, err)
                }
        }
        if claimHits != 1 {
                t.Fatalf("claim POSTed %d times, want exactly 1 (cooldown must suppress the rest)", claimHits)
        }

        // Fresh deployment: no memo at all → the probe must not fire and the
        // typed NOTHING_CLAIMABLE verdict comes back with the idle diagnosis.
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
                },
                "/sash/api/v1/me/campaigns/camp-any/claim": func(r *http.Request) (int, string) {
                        t.Error("probe fired without a seen campaign id — fabricated claim")
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
        })
        res, err := performCheckinCall(cnAuth())
        if err != nil {
                t.Fatalf("performCheckinCall: %v", err)
        }
        if result, _ := res["result"].(string); result != "NOTHING_CLAIMABLE" {
                t.Fatalf("result = %v, want NOTHING_CLAIMABLE", res["result"])
        }
}

// TestCampaignIdleDiagnosisRendersUpstreamTaxonomy: the idle diagnosis must
// carry the upstream's own unavailableReason verdicts in official semantics
// plus the identity hint, so "why nothing claimable" is answerable from the
// panel line alone.
func TestCampaignIdleDiagnosisRendersUpstreamTaxonomy(t *testing.T) {
        status := &campaignStatusResponse{Campaigns: []campaign{
                {CampaignID: "c1", CampaignKey: "act-20260928-620", ActionType: "CLAIM_BENEFIT", ClaimStatus: "NOT_CLAIMABLE", UnavailableReason: "REDEMPTION_CODE_OUT_OF_STOCK"},
                {CampaignID: "c2", CampaignKey: "act-20260901-922", ActionType: "VIEW_DETAILS", ClaimStatus: "CLAIMABLE"},
        }}
        msg := campaignIdleDiagnosis(status, cnAuth())
        for _, want := range []string{
                "名额已发完",
                "act-20260928-620",
                "act-20260901-922",
                "需在官方活动页完成领取",
        } {
                if !strings.Contains(msg, want) {
                        t.Fatalf("diagnosis missing %q: %q", want, msg)
                }
        }
        status.Campaigns = []campaign{
                {CampaignID: "c3", CampaignKey: "act-20260930-125", ActionType: "CLAIM_BENEFIT", ClaimStatus: "NOT_CLAIMABLE", UnavailableReason: "ACHIEVEMENT_NOT_COMPLETED"},
        }
        msg = campaignIdleDiagnosis(status, cnAuth())
        if !strings.Contains(msg, "完成新人任务") || !strings.Contains(msg, "act-20260930-125") {
                t.Fatalf("achievement-locked diagnosis wrong: %q", msg)
        }
}

// TestRoundMemoFedByListFetch: every campaigns read must refresh the probe
// memo with the daily-shaped rows the server returned (any status), so a
// later hidden round has a real id to POST.
func TestRoundMemoFedByListFetch(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        b, _ := json.Marshal(campaignStatusResponse{
                                ShowCampaign: true,
                                Campaigns: []campaign{
                                        {CampaignID: "camp-view", CampaignKey: "act-20260901-922", ActionType: "VIEW_DETAILS", ClaimStatus: "CLAIMABLE"},
                                        {CampaignID: "camp-cn-daily", CampaignKey: "cn_daily_check_in", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}},
                                },
                        })
                        return http.StatusOK, string(b)
                },
        })
        // fetchCampaignStatus routes through fetchCampaignStatusOnce → memo.
        if _, err := fetchCampaignStatus(cnAuth()); err != nil {
                t.Fatalf("fetchCampaignStatus: %v", err)
        }
        c, ok := roundMemo.probeFor("cn", "")
        if !ok || c.CampaignID != "camp-cn-daily" {
                t.Fatalf("memo = %+v ok=%v, want camp-cn-daily (VIEW_DETAILS rows must not become probe ids)", c, ok)
        }
        // Per-account entry and region entry both updated.
        if _, ok := roundMemo.perRegion["cn"]; !ok {
                t.Fatal("region-level memo entry missing")
        }
        // A different account may probe the same region-stable round id.
        c2, ok := roundMemo.probeFor("cn", "other-uid")
        if !ok || c2.CampaignID != "camp-cn-daily" {
                t.Fatalf("region fallback probe failed: %+v %v", c2, ok)
        }
}

// TestProbeExcludesExpiredMemo: a memo older than the freshness window must
// never be probed — the campaign id could have been recycled by a new round.
func TestProbeExcludesExpiredMemo(t *testing.T) {
        roundMemo = &campaignRoundMemo{
                perAccount: map[string]campaignRoundEntry{},
                perRegion:  map[string]campaignRoundEntry{},
        }
        t.Cleanup(func() {
                roundMemo = &campaignRoundMemo{perAccount: map[string]campaignRoundEntry{}, perRegion: map[string]campaignRoundEntry{}}
        })
        roundMemo.perRegion["cn"] = campaignRoundEntry{CampaignID: "camp-stale", SeenAt: time.Now().Add(-72 * time.Hour)}
        if _, ok := roundMemo.probeFor("cn", "u"); ok {
                t.Fatal("stale memo must not be probeable")
        }
        _ = fmt.Sprint() // keep fmt imported for future assertions
}

// seedRoundMemo pushes a previously-seen daily row into the memo the way a
// real campaigns read would (the round id is region-stable across days).
func seedRoundMemo(t *testing.T, campaignID, claimStatus string) {
        t.Helper()
        status := &campaignStatusResponse{Campaigns: []campaign{{
                CampaignID:  campaignID,
                CampaignKey: "cn_daily_check_in",
                ActionType:  "CLAIM_BENEFIT",
                ClaimStatus: claimStatus,
                Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
        }}}
        rememberCampaignRound("cn", "", status)
}
