// campaign_taxonomy_test.go — v0.8.40: pins the fixes for the field report
// "签到的积分概率性不成功或者成功后积分无变化":
//
//  1. eligibility failureCodes (REDEMPTION_CODE_OUT_OF_STOCK et al.) arrive
//     as 200 bodies, not errors — the daily rounds are 先到先得 with a
//     10:00 UTC+8 refresh, so "probabilistic" failures were the stock window,
//     not a plugin bug, but the old claimer rendered them as 上游未确认签到
//     成功 error toasts anyway. Typed NOT_ELIGIBLE verdicts now, from both
//     the claim body and the list's unavailableReason.
//  2. coupon/redemption rows answer CLAIMED with a redemptionCode and NO
//     credits (hub: act-20260928-620 奶茶免单卡) — the code must surface and
//     the reward must read as 0 credits, or the panel invites "成功后积分无
//     变化" reports.
//  3. the check-in is a MULTI-claim pass: a credits row and a coupon row can
//     both be claimable, and claiming only the first stranded the rest.
//  4. the bypass-probe cooldown arms only on conclusive upstream verdicts —
//     one transport failure must not silence retries for 6h.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestStockOutVerdictFromClaimBody: the claim POST answers 200 with an
// eligibility failureCode — a typed NOT_ELIGIBLE verdict with the upstream's
// Chinese rendering, never the generic 上游未确认签到成功 bucket.
func TestStockOutVerdictFromClaimBody(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"CLAIMABLE","failureCode":"REDEMPTION_CODE_OUT_OF_STOCK"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if result, _ := res["result"].(string); result != "NOT_ELIGIBLE" {
		t.Fatalf("result = %v, want NOT_ELIGIBLE", res["result"])
	}
	if fc, _ := res["failure_code"].(string); fc != "REDEMPTION_CODE_OUT_OF_STOCK" {
		t.Fatalf("failure_code = %v", res["failure_code"])
	}
	if msg, _ := res["message"].(string); msg == "" {
		t.Fatal("typed verdict must carry the upstream's Chinese message")
	}
}

// TestStockOutVerdictFromListUnavailableReason: when the LIST itself explains
// the state (claimStatus non-claimable + unavailableReason), the typed
// verdict short-circuits without a claim POST — the hub's outOfStock/locked
// state machine.
func TestStockOutVerdictFromListUnavailableReason(t *testing.T) {
	claimHit := false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			b, _ := json.Marshal(campaignStatusResponse{
				ShowCampaign: true,
				Campaigns: []campaign{{
					CampaignID:        "camp-daily",
					CampaignKey:       "act-20260930-125",
					ActionType:        "CLAIM_BENEFIT",
					ClaimStatus:       "UNAVAILABLE",
					UnavailableReason: "REDEMPTION_CODE_OUT_OF_STOCK",
				}},
			})
			return http.StatusOK, string(b)
		},
		"/sash/api/v1/me/campaigns/camp-daily/claim": func(r *http.Request) (int, string) {
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
		t.Fatal("a list-explained verdict must not POST a claim")
	}
	if result, _ := res["result"].(string); result != "NOT_ELIGIBLE" {
		t.Fatalf("result = %v, want NOT_ELIGIBLE", res["result"])
	}
}

// TestMultiRowClaimAggregatesCreditsAndCode: a credits row AND a coupon row
// both claimable — the check-in claims BOTH, reports +100 credits and the
// redemption code in one result.
func TestMultiRowClaimAggregatesCreditsAndCode(t *testing.T) {
	claimed := map[string]bool{}
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			b, _ := json.Marshal(campaignStatusResponse{
				ShowCampaign: true,
				Campaigns: []campaign{
					{
						CampaignID:  "camp-coupon",
						CampaignKey: "act-20260928-620",
						ActionType:  "CLAIM_BENEFIT",
						ClaimStatus: "CLAIMABLE",
						Benefit:     &campaignBen{Kind: "REDEMPTION_CODE", Amount: 1},
					},
					{
						CampaignID:  "camp-daily",
						CampaignKey: "act-20260930-125",
						ActionType:  "CLAIM_BENEFIT",
						ClaimStatus: "CLAIMABLE",
						Benefit:     &campaignBen{Kind: "CREDITS", Amount: 100},
					},
				},
			})
			return http.StatusOK, string(b)
		},
		"/sash/api/v1/me/campaigns/camp-daily/claim": func(r *http.Request) (int, string) {
			claimed["daily"] = true
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
		"/sash/api/v1/me/campaigns/camp-coupon/claim": func(r *http.Request) (int, string) {
			claimed["coupon"] = true
			return http.StatusOK, `{"status":"CLAIMED","redemptionCode":"MILK-1234"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if !claimed["daily"] || !claimed["coupon"] {
		t.Fatalf("both rows must be claimed, got %v", claimed)
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("aggregate must be a success: %v", res)
	}
	if rc, _ := res["reward_credits"].(float64); rc != 100 {
		t.Fatalf("reward_credits = %v, want 100", res["reward_credits"])
	}
	codes, _ := res["redemption_codes"].([]string)
	if len(codes) != 1 || codes[0] != "MILK-1234" {
		t.Fatalf("redemption_codes = %v, want [MILK-1234]", res["redemption_codes"])
	}
}

// TestCouponOnlyClaimSurfacesCode: only a coupon row claimable — the result
// is a success whose reward is a CODE and explicitly NOT credits.
func TestCouponOnlyClaimSurfacesCode(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			b, _ := json.Marshal(campaignStatusResponse{
				ShowCampaign: true,
				Campaigns: []campaign{{
					CampaignID:  "camp-coupon",
					CampaignKey: "act-20260928-620",
					ActionType:  "CLAIM_BENEFIT",
					ClaimStatus: "CLAIMABLE",
					Benefit:     &campaignBen{Kind: "REDEMPTION_CODE", Amount: 1},
				}},
			})
			return http.StatusOK, string(b)
		},
		"/sash/api/v1/me/campaigns/camp-coupon/claim": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"CLAIMED","redemptionCode":"MILK-5678"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})

	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("coupon claim is a success: %v", res)
	}
	if rc, _ := res["reward_credits"].(float64); rc != 0 {
		t.Fatalf("coupon reward_credits = %v, want 0 (non-credits grant)", res["reward_credits"])
	}
	if code, _ := res["redemption_code"].(string); code != "MILK-5678" {
		t.Fatalf("redemption_code = %v, want MILK-5678", res["redemption_code"])
	}
	if msg, _ := res["message"].(string); msg == "" || !containsAny(msg, "兑换券", "非积分") {
		t.Fatalf("message must say the reward is a coupon, got %q", res["message"])
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// TestProbeCooldownNotArmedOnTransportFailure: a probe POST that never gets
// an upstream verdict (HTTP 500) must NOT arm the 6h cooldown — the next
// check-in tick must retry. The first conclusive verdict arms it.
func TestProbeCooldownNotArmedOnTransportFailure(t *testing.T) {
	claimHits := 0
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
		},
		"/sash/api/v1/me/campaigns/camp-hidden/claim": func(r *http.Request) (int, string) {
			claimHits++
			if claimHits <= 2 {
				return http.StatusInternalServerError, `{"error":"boom"}`
			}
			return http.StatusOK, `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	// Seed the round memo: the account saw this daily row earlier.
	rememberCampaignRound("cn", cnAuth().Account.UID, &campaignStatusResponse{
		Campaigns: []campaign{{CampaignID: "camp-hidden", CampaignKey: "act-20260930-125", ActionType: "CLAIM_BENEFIT"}},
	})

	// Attempt 1: probe POSTs, upstream 500s → inconclusive, no latch.
	if _, err := performCheckinCall(cnAuth()); err != nil {
		t.Fatalf("performCheckinCall #1: %v", err)
	}
	if claimHits != 1 {
		t.Fatalf("claim hits after attempt 1 = %d, want 1", claimHits)
	}
	// Attempt 2: cooldown must NOT be armed — the probe fires again.
	if _, err := performCheckinCall(cnAuth()); err != nil {
		t.Fatalf("performCheckinCall #2: %v", err)
	}
	if claimHits != 2 {
		t.Fatalf("claim hits after attempt 2 = %d, want 2 (transport failure must not arm the cooldown)", claimHits)
	}
	// Attempt 3: the probe gets a CONCLUSIVE verdict (BLOCKED) → latch.
	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall #3: %v", err)
	}
	if claimHits != 3 {
		t.Fatalf("claim hits after attempt 3 = %d, want 3", claimHits)
	}
	if result, _ := res["result"].(string); result != "BLOCKED" {
		t.Fatalf("conclusive verdict must surface: %v", res["result"])
	}
	// Attempt 4: cooldown armed by the conclusive verdict → probe skipped.
	if _, err := performCheckinCall(cnAuth()); err != nil {
		t.Fatalf("performCheckinCall #4: %v", err)
	}
	if claimHits != 3 {
		t.Fatalf("claim hits after attempt 4 = %d, want 3 (conclusive verdict must arm the cooldown)", claimHits)
	}
	_ = fmt.Sprint()
}

// TestDesktopDialectLoginURL: v0.8.45 — both CN and Intl now always build
// the official desktop-client v0.4.3 URL (asar functions Sft + Nft):
// /users/sign-in?biz_variant=qoder&oauth_callback=<encoded selectAccounts URL>.
// The selectAccounts inner URL carries client_id=732aef47-…, machine_id,
// challenge, nonce, challenge_method=S256, and redirect_uri (omitted for CN,
// qoder-app:// for Intl). The login_dialect config is a deprecated no-op.
func TestDesktopDialectLoginURL(t *testing.T) {
	// CN — default (no dialect set).
	out, err := startLoginWithRegion(nil, regionCN)
	if err != nil {
		t.Fatalf("startLoginWithRegion CN: %v", err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope CN: %v ok=%v", err, env.OK)
	}
	var start struct {
		URL      string         `json:"url"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(env.Result, &start); err != nil {
		t.Fatalf("start payload CN: %v", err)
	}
	// Nft wrapper present.
	if !containsAny(start.URL, "https://qoder.cn/users/sign-in?biz_variant=qoder") {
		t.Fatalf("CN login URL must wrap in /users/sign-in on qoder.cn, got %q", start.URL)
	}
	// Inner selectAccounts params ride percent-encoded inside oauth_callback.
	for _, want := range []string{
		"biz_variant=qoder",
		"oauth_callback=",
		"client_id%3D732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
		"challenge_method%3DS256",
		"challenge%3D",
		"nonce%3D",
		"machine_id%3D",
	} {
		if !containsAny(start.URL, want) {
			t.Fatalf("CN login URL missing %q: %s", want, start.URL)
		}
	}
	// CN redirect_uri is empty → MUST be omitted from the inner URL.
	if containsAny(start.URL, "redirect_uri%3D") {
		t.Fatalf("CN login URL must NOT carry redirect_uri, got %q", start.URL)
	}
	// CN auth host must be qoder.cn (NOT the legacy qoder.com.cn).
	if containsAny(start.URL, "qoder.com.cn") {
		t.Fatalf("CN login URL must use qoder.cn (not the legacy qoder.com.cn), got %q", start.URL)
	}
	if prompt, _ := start.Metadata["prompt"].(string); !containsAny(prompt, "桌面") {
		t.Fatalf("CN prompt must mention the desktop-app redirect, got %q", prompt)
	}

	// Intl — default (no dialect set).
	out2, err := startLoginWithRegion(nil, regionIntl)
	if err != nil {
		t.Fatalf("startLoginWithRegion Intl: %v", err)
	}
	var env2 struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out2, &env2); err != nil || !env2.OK {
		t.Fatalf("envelope Intl: %v ok=%v", err, env2.OK)
	}
	var start2 struct {
		URL      string         `json:"url"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(env2.Result, &start2); err != nil {
		t.Fatalf("start payload Intl: %v", err)
	}
	if !containsAny(start2.URL, "https://qoder.com/users/sign-in?biz_variant=qoder") {
		t.Fatalf("Intl login URL must wrap in /users/sign-in on qoder.com, got %q", start2.URL)
	}
	for _, want := range []string{
		"biz_variant=qoder",
		"oauth_callback=",
		"client_id%3D732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
		"challenge_method%3DS256",
		"machine_id%3D",
		"redirect_uri%3Dqoder-app%253A%252F%252F",
	} {
		if !containsAny(start2.URL, want) {
			t.Fatalf("Intl login URL missing %q: %s", want, start2.URL)
		}
	}
	if containsAny(start2.URL, "e883ade2") || containsAny(start2.URL, "1c5e33e1") {
		t.Fatalf("Intl login URL must NOT carry a legacy qoderwork client_id, got %q", start2.URL)
	}

	// login_dialect is now a deprecated no-op: both values produce the
	// identical desktop URL (the v0.8.40 cockpit/desktop split is gone).
	prev := loadedLoginDialect()
	t.Cleanup(func() { setLoginDialect(prev) })
	for _, d := range []string{loginDialectCockpit, loginDialectDesktop} {
		setLoginDialect(d)
		out3, _ := startLoginWithRegion(nil, regionIntl)
		var env3 struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(out3, &env3)
		var start3 struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(env3.Result, &start3)
		if !containsAny(start3.URL, "users/sign-in") || !containsAny(start3.URL, "client_id%3D732aef47") {
			t.Fatalf("login_dialect=%s must still produce the desktop URL: %s", d, start3.URL)
		}
	}
}
