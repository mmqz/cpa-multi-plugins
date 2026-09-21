// checkin_campaign_test.go — v0.12.80: pins the CN check-in dialect switch.
// Field report ("Qoder CN 账户仍然不能签到"): upstream DISABLED the legacy
// daily-check-in system — claim answers 409 even on unclaimed days and
// grants no credits (verified upstream 2026-09-21, cross-checked against
// the qoder2api project's packet-captured campaigns flow). Both regions now
// claim via /sash/api/v1/me/campaigns; the legacy status endpoint survives
// as a READ-ONLY stats supplement for CN. These HTTP-level tests pin the
// routing (campaigns claim, legacy claim NEVER called) and the stats merge
// (non-zero legacy values win, DISABLED zeros never clobber).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newBillingServer spins up an httptest upstream and points billingBaseFor
// at it. The handler records every visited path and answers from the
// per-path responder map. The legacy claim endpoint always fails the test —
// no code path may call it anymore.
func newBillingServer(t *testing.T, region string, respond map[string]func(r *http.Request) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/daily-check-in/claim") {
			t.Errorf("legacy daily-check-in/claim was called (%s) — DISABLED upstream, must never be hit", path)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"result":"ALREADY_CLAIMED"}`))
			return
		}
		h, ok := respond[path]
		if !ok {
			t.Logf("unstubbed path %s — 404", path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		code, body := h(r)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	prev := billingBaseOverride
	billingBaseOverride = func(string) string { return srv.URL }
	t.Cleanup(func() { billingBaseOverride = prev })
	return srv
}

func cnAuth() *storedAuth {
	return &storedAuth{Auth: storedTokens{AccessToken: "dt-checkin-test", Region: "cn"}}
}

func campaignList(status string) string {
	b, _ := json.Marshal(campaignStatusResponse{
		ShowCampaign: true,
		Campaigns: []campaign{{
			CampaignID:  "camp-cn-daily",
			CampaignKey: "cn_daily_check_in",
			ActionType:  "CLAIM_BENEFIT",
			ClaimStatus: status,
			Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
		}},
	})
	return string(b)
}

// TestCNCheckinClaimsViaCampaigns: a CN account's manual check-in must list
// campaigns, POST the claim, and never touch the legacy claim endpoint.
func TestCNCheckinClaimsViaCampaigns(t *testing.T) {
	claimHit := false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			claimHit = true
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED","currentStreakDays":0,"totalClaimDays":0,"totalRewardCredits":0}`
		},
	})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if !claimHit {
		t.Fatal("campaigns claim endpoint was never called")
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("claim not successful: %v", res)
	}
	if rc, _ := res["rewardCredits"].(float64); rc != 100 {
		t.Fatalf("rewardCredits = %v, want 100 (from the listed benefit)", res["rewardCredits"])
	}
}

// TestCNCheckinReplayedClaimIsAlready: an idempotent replay (claim already
// landed earlier today) must surface as ALREADY_CLAIMED, not as a fresh
// success — the panel then shows 今日已签 instead of inviting a re-claim.
func TestCNCheckinReplayedClaimIsAlready(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns":                     func(r *http.Request) (int, string) { return http.StatusOK, campaignList("CLAIMABLE") },
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) { return http.StatusOK, `{"status":"CLAIMED","replayed":true}` },
		"/sash/api/v1/me/daily-check-in/status":         func(r *http.Request) (int, string) { return http.StatusOK, `{"status":"DISABLED"}` },
	})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if result, _ := res["result"].(string); result != "ALREADY_CLAIMED" {
		t.Fatalf("result = %v, want ALREADY_CLAIMED for replayed claim", res["result"])
	}
}

// TestCNCheckinSummaryMergesLegacyStats: the legacy status endpoint stays
// readable — its non-zero streak/total stats must reach the panel summary
// even though the claim itself rides campaigns.
func TestCNCheckinSummaryMergesLegacyStats(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) { return http.StatusOK, campaignList("CLAIMABLE") },
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED","rewardCredits":100,"currentStreakDays":5,"totalClaimDays":12,"totalRewardCredits":600}`
		},
	})

	sum, err := fetchCheckinStatus(cnAuth())
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if !sum.Active {
		t.Fatal("claimable campaign must render as active")
	}
	if sum.StreakDays != 5 || sum.WeekCheckinDays != 12 || sum.TotalCredits != 600 {
		t.Fatalf("legacy stats not merged: streak=%d days=%d total=%d", sum.StreakDays, sum.WeekCheckinDays, sum.TotalCredits)
	}
	if sum.DailyCredit != 100 {
		t.Fatalf("DailyCredit = %d, want 100 (campaign benefit already set)", sum.DailyCredit)
	}
}

// TestCNCheckinLegacyZerosNeverClobber: the DISABLED legacy system reports
// zeros — they must not overwrite campaign-derived state, and a legacy probe
// failure must not fail the whole summary (best-effort supplement).
func TestCNCheckinLegacyZerosNeverClobber(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) { return http.StatusOK, campaignList("CLAIMABLE") },
		// Legacy status errors (500) — merge must stay silent.
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) { return http.StatusInternalServerError, `{"err":"boom"}` },
	})

	sum, err := fetchCheckinStatus(cnAuth())
	if err != nil {
		t.Fatalf("legacy probe failure must not fail the summary: %v", err)
	}
	if !sum.Active || sum.DailyCredit != 100 {
		t.Fatalf("campaign summary clobbered: %+v", sum)
	}
	if sum.StreakDays != 0 || sum.TotalCredits != 0 {
		t.Fatalf("zeros must not be merged: streak=%d total=%d", sum.StreakDays, sum.TotalCredits)
	}
}

// TestIntlCheckinSkipsLegacyProbe: Intl has no legacy daily-check-in
// endpoint — probing it would only 404. The stats merge must be a no-op.
func TestIntlCheckinSkipsLegacyProbe(t *testing.T) {
	legacyHit := false
	newBillingServer(t, "intl", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) { return http.StatusOK, campaignList("CLAIMABLE") },
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			legacyHit = true
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})

	sum, err := fetchCheckinStatus(&storedAuth{Auth: storedTokens{AccessToken: "dt-intl", Region: "intl"}})
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if legacyHit {
		t.Fatal("Intl summary must not probe the legacy CN daily-check-in endpoint")
	}
	if !sum.Active || sum.DailyCredit != 100 {
		t.Fatalf("Intl summary wrong: %+v", sum)
	}
}

// TestCNCheckinNoCampaignIsNoOp: an empty campaign list renders as inactive,
// which checkinOneAccount turns into reason=none (今日暂无可领取权益) — never
// into a claim against the dead legacy endpoint.
func TestCNCheckinNoCampaignIsNoOp(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) { return http.StatusOK, `{"status":"DISABLED"}` },
	})

	sum, err := fetchCheckinStatus(cnAuth())
	if err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if sum.Active {
		t.Fatal("empty campaign list must not render as active")
	}
	if sum.TodayCheckedIn {
		t.Fatal("empty campaign list must not render as already checked in")
	}
}
