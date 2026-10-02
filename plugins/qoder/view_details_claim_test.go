// view_details_claim_test.go — v0.8.41: the check-in path now ATTEMPTS the
// CLAIMABLE VIEW_DETAILS rows instead of stranding them behind a
// "需在官方活动页完成领取" note. Evidence: issue #27 finding 3 — the
// official client's captured launch flow POSTs the same claim endpoint on a
// VIEW_DETAILS (bogo) row and gets 200 CLAIMED. Pins:
//   - readable CREDITS face value → claimed, +N lands in the result;
//   - subscription-shaped reward → NOT claimed from check-in (Pro flow owns
//     those rows), verdict surfaced as a note;
//   - unreadable face value + claim_unverified opt-in → blind-claimed, the
//     claim body's face value wins;
//   - the bypass probe's NOT_ELIGIBLE verdict survives into the panel
//     message (it used to be discarded before the generic idle diagnosis).
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func viewDetailsList(id, key string) string {
	b, _ := json.Marshal(campaignStatusResponse{
		ShowCampaign: true,
		Campaigns: []campaign{{
			CampaignID:  id,
			CampaignKey: key,
			ActionType:  "VIEW_DETAILS",
			ClaimStatus: "CLAIMABLE",
		}},
	})
	return string(b)
}

// TestCheckinClaimsViewDetailsWithReadableCreditsReward: bogo parity — a
// VIEW_DETAILS row whose /reward probe reveals a CREDITS face value is
// claimed by the check-in and the grant lands with the real amount.
func TestCheckinClaimsViewDetailsWithReadableCreditsReward(t *testing.T) {
	claimHit := false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, viewDetailsList("camp-bogo", "act-20260901-922")
		},
		"/sash/api/v1/me/campaigns/camp-bogo/reward": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"benefit":{"kind":"CREDITS","amount":100}}`
		},
		"/sash/api/v1/me/campaigns/camp-bogo/claim": func(r *http.Request) (int, string) {
			claimHit = true
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if !claimHit {
		t.Fatal("VIEW_DETAILS row with a readable CREDITS reward must be claimed")
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("want success, got %v", res)
	}
	if rc, _ := res["reward_credits"].(float64); rc != 100 {
		t.Fatalf("reward_credits = %v, want 100", res["reward_credits"])
	}
}

// TestCheckinViewDetailsSubscriptionRewardNotClaimed: a subscription-shaped
// reward belongs to the Pro-upgrade flow; check-in must not ride a
// subscription claim, and the note must say so.
func TestCheckinViewDetailsSubscriptionRewardNotClaimed(t *testing.T) {
	claimHit := false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, viewDetailsList("camp-sub", "act-20260901-493")
		},
		"/sash/api/v1/me/campaigns/camp-sub/reward": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"benefit":{"kind":"SUBSCRIPTION","days":14}}`
		},
		"/sash/api/v1/me/campaigns/camp-sub/claim": func(r *http.Request) (int, string) {
			claimHit = true
			return http.StatusOK, `{"status":"CLAIMED"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if claimHit {
		t.Fatal("subscription-shaped VIEW_DETAILS row must not be claimed from check-in")
	}
	if result, _ := res["result"].(string); result != "NOTHING_CLAIMABLE" {
		t.Fatalf("result = %v, want NOTHING_CLAIMABLE", res["result"])
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "SUBSCRIPTION") || !strings.Contains(msg, "Pro/订阅流程") {
		t.Fatalf("diagnosis must carry the subscription verdict, got %q", msg)
	}
}

// TestCheckinViewDetailsOptInBlindClaim: with claim_unverified on, an
// unreadable-face-value VIEW_DETAILS row is blind-claimed and the claim
// body's face value wins (the reward endpoint never answered).
func TestCheckinViewDetailsOptInBlindClaim(t *testing.T) {
	claimUnverifiedMu.Lock()
	prev := claimUnverified
	claimUnverified = true
	claimUnverifiedMu.Unlock()
	t.Cleanup(func() {
		claimUnverifiedMu.Lock()
		claimUnverified = prev
		claimUnverifiedMu.Unlock()
	})
	claimHit := false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, viewDetailsList("camp-blind", "act-20260901-922")
		},
		// no /reward stub → 404 → face value unreadable
		"/sash/api/v1/me/campaigns/camp-blind/claim": func(r *http.Request) (int, string) {
			claimHit = true
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":50}}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if !claimHit {
		t.Fatal("claim_unverified opt-in must let check-in claim the unreadable-face-value row")
	}
	if rc, _ := res["reward_credits"].(float64); rc != 50 {
		t.Fatalf("reward_credits = %v, want 50 (claim body face value)", res["reward_credits"])
	}
}

// TestProbeNotEligibleVerdictSurvivesInDiagnosis: the bypass probe's typed
// refusal (stale round id → CAMPAIGN_NOT_ACTIVE) must reach the panel
// message — 0.8.40 silently discarded it and the user only ever saw the
// generic idle line.
func TestProbeNotEligibleVerdictSurvivesInDiagnosis(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
		},
		"/sash/api/v1/me/campaigns/camp-old/claim": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"FAILED","failureCode":"CAMPAIGN_NOT_ACTIVE"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	// Seed the memo the way a previous round's list fetch would have (the
	// round is hidden from today's list — that is the probe's whole point).
	roundMemo.remember("cn", "", &campaign{CampaignID: "camp-old", CampaignKey: "act-20260930-125"})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if result, _ := res["result"].(string); result != "NOT_ELIGIBLE" {
		t.Fatalf("result = %v, want NOT_ELIGIBLE", res["result"])
	}
	if fc, _ := res["failure_code"].(string); fc != "CAMPAIGN_NOT_ACTIVE" {
		t.Fatalf("failure_code = %v, want CAMPAIGN_NOT_ACTIVE", res["failure_code"])
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "活动已结束或未开始") {
		t.Fatalf("probe verdict must survive in the message, got %q", msg)
	}
}
