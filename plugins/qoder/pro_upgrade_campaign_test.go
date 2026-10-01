// pro_upgrade_campaign_test.go — v0.8.34: the 领取Pro flow rides the
// campaigns channel. Upstream forensics (official CN client v0.4.3, sha256
// a796a175…05084f5): /sash/api/v1/me/pro-upgrade/* has ZERO matches in the
// shipped app.asar — the eligibility→claim flow this plugin ran since
// v0.8.18 called endpoints that do not exist, and the field 404
// ("领取失败 eligibility: http 404", CN account ud2d62d72) was upstream
// truthfully answering "no such route". These tests pin the replacement:
// claim pro-looking campaign rows via the verifiable
// GET /me/campaigns + POST /{id}/claim family, and answer a diagnostic
// listing (never a bare http error) when no such row is present.
package main

import (
	"net/http"
	"strings"
	"testing"
)

func proCampaignList(dailyStatus, proStatus string) string {
	return `{"showCampaign":true,"campaigns":[` +
		`{"campaignId":"camp-cn-daily","campaignKey":"cn_daily_check_in","actionType":"CLAIM_BENEFIT","claimStatus":"` + dailyStatus + `","benefit":{"kind":"CREDITS","amount":100}},` +
		`{"campaignId":"camp-pro-up","campaignKey":"pro_upgrade_pack","actionType":"CLAIM_BENEFIT","claimStatus":"` + proStatus + `","benefit":{"kind":"CREDITS","amount":1800}}` +
		`]}`
}

// TestClaimProClaimsProRowOnly: with both a daily and a pro row claimable,
// 领取Pro must claim ONLY the pro-looking row — the daily row belongs to the
// check-in flow, and the pro claim must carry the listed benefit amount.
func TestClaimProClaimsProRowOnly(t *testing.T) {
	dailyClaimed, proClaimed := false, false
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, proCampaignList("CLAIMABLE", "CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			dailyClaimed = true
			return http.StatusOK, `{"status":"CLAIMED"}`
		},
		"/sash/api/v1/me/campaigns/camp-pro-up/claim": func(r *http.Request) (int, string) {
			proClaimed = true
			return http.StatusOK, `{"status":"CLAIMED"}`
		},
	})

	res, err := claimProViaCampaigns(cnAuth())
	if err != nil {
		t.Fatalf("claimProViaCampaigns: %v", err)
	}
	if !proClaimed {
		t.Fatal("pro row claim endpoint was never called")
	}
	if dailyClaimed {
		t.Fatal("领取Pro must not claim the daily check-in row")
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("claim not successful: %v", res)
	}
	if id, _ := res["campaign_id"].(string); id != "camp-pro-up" {
		t.Fatalf("campaign_id = %v, want camp-pro-up", res["campaign_id"])
	}
	if rc, _ := res["rewardCredits"].(float64); rc != 1800 {
		t.Fatalf("rewardCredits = %v, want 1800 (listed benefit)", res["rewardCredits"])
	}
}

// TestClaimProAlreadyClaimedRow: a CLAIMED pro row renders as an
// actionable「已领取过」line, not an error.
func TestClaimProAlreadyClaimedRow(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, proCampaignList("CLAIMABLE", "CLAIMED")
		},
	})

	res, err := claimProViaCampaigns(cnAuth())
	if err != nil {
		t.Fatalf("claimProViaCampaigns: %v", err)
	}
	if success, _ := res["success"].(bool); success {
		t.Fatalf("success = true, want false for already-claimed: %v", res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "已领取过") {
		t.Fatalf("message = %q, want 已领取过 line", msg)
	}
}

// TestClaimProNoProRowDiagnostics: when the account's campaign list has no
// pro-looking row at all, the answer must list what the server actually
// returned (campaignKey/actionType/claimStatus) instead of a bare failure —
// this is what makes a mis-guessed key correctable from one field report.
func TestClaimProNoProRowDiagnostics(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"c1","campaignKey":"client_launch_26","actionType":"ACTIVITY","claimStatus":"IN_PROGRESS"}]}`
		},
	})

	res, err := claimProViaCampaigns(cnAuth())
	if err != nil {
		t.Fatalf("claimProViaCampaigns: %v", err)
	}
	if success, _ := res["success"].(bool); success {
		t.Fatalf("success = true, want false for no-pro-row: %v", res)
	}
	if reason, _ := res["reason"].(string); reason != "no_pro_row" {
		t.Fatalf("reason = %v, want no_pro_row", res["reason"])
	}
	msg, _ := res["message"].(string)
	for _, want := range []string{"client_launch_26", "ACTIVITY", "IN_PROGRESS"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic message missing %q: %q", want, msg)
		}
	}
}

// TestClaimProEmptyListClientSessionHint: an empty campaign list most likely
// means the account's eligibility never synced (#27: rows appear only after
// the account opens the activity once inside the official client) — the
// message must say so.
func TestClaimProEmptyListClientSessionHint(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
		},
	})

	res, err := claimProViaCampaigns(cnAuth())
	if err != nil {
		t.Fatalf("claimProViaCampaigns: %v", err)
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "客户端") {
		t.Fatalf("message = %q, want client-session hint", msg)
	}
}

// TestClaimProCampaignsErrorPropagates: a campaigns listing failure surfaces
// as an error carrying the upstream status — no silent success.
func TestClaimProCampaignsErrorPropagates(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusInternalServerError, `{"err":"boom"}`
		},
	})

	_, err := claimProViaCampaigns(cnAuth())
	if err == nil || !strings.Contains(err.Error(), "campaigns http 500") {
		t.Fatalf("err = %v, want campaigns http 500", err)
	}
}

// TestClaimProIntlSkipsBeforeAnyRequest: the Intl skip is a capability fact
// (panel contract since v0.8.18) — no request may fire for an Intl account.
func TestClaimProIntlSkipsBeforeAnyRequest(t *testing.T) {
	hit := false
	newBillingServer(t, "intl", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			hit = true
			return http.StatusOK, `{"campaigns":[]}`
		},
	})

	res, err := claimProViaCampaigns(&storedAuth{Auth: storedTokens{AccessToken: "dt-intl", Region: "intl"}})
	if err != nil {
		t.Fatalf("claimProViaCampaigns: %v", err)
	}
	if hit {
		t.Fatal("Intl must not reach the campaigns endpoint from the Pro flow")
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("Intl result must carry skipped=true: %v", res)
	}
}
