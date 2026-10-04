// billing_headers_test.go — v0.8.22: the billing surface must present the
// desktop Cosy identity. bfSan 30d6c16 live-verified (2026-09-21,
// openapi.qoder.com.cn + openapi.qoder.sh) that the campaigns response is
// gated on Cosy-ClientType: a bare request gets showCampaign:false while the
// same credential returns the live daily "100 Credits" campaign once the
// header is present. Without it a CN day can render "今日暂无可领取权益" and
// be silently skipped — the CLAIMABLE-row inference (v0.12.80) never sees a
// row at all. These HTTP-level tests pin the headers on the campaigns path
// (the endpoint where the gate was observed) and on the legacy stats path.
package main

import (
	"net/http"
	"testing"
)

func TestCampaignsRequestCarriesCosyIdentity(t *testing.T) {
	var gotCT, gotCV, gotUA, gotAuth string
	srv := newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			gotCT = r.Header.Get("Cosy-ClientType")
			gotCV = r.Header.Get("Cosy-Version")
			gotUA = r.Header.Get("User-Agent")
			gotAuth = r.Header.Get("Authorization")
			return http.StatusOK, campaignList("CLAIMABLE")
		},
	})
	sum, err := fetchCampaignCheckinSummary(cnAuth())
	if err != nil {
		t.Fatalf("fetchCampaignCheckinSummary: %v", err)
	}
	if sum == nil || !sum.Active {
		t.Fatalf("campaign summary should be active, got %+v", sum)
	}
	srv.Close() // stop serving before asserting (not required, just tidy)

	if gotCT != billingClientType {
		t.Fatalf("Cosy-ClientType = %q, want %q (bfSan 30d6c16: response is gated on this header)", gotCT, billingClientType)
	}
	if gotCV != billingClientVer {
		t.Fatalf("Cosy-Version = %q, want %q", gotCV, billingClientVer)
	}
	if gotUA != "Qoder" {
		t.Fatalf("User-Agent = %q, want \"Qoder\"", gotUA)
	}
	if gotAuth != "Bearer dt-checkin-test" {
		t.Fatalf("Authorization must be untouched, got %q", gotAuth)
	}
}

func TestLegacyStatsRequestCarriesCosyIdentity(t *testing.T) {
	var gotCT string
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		// v0.12.80: fetchCheckinStatus leads with the campaigns dialect and
		// merges legacy stats as a CN-only read-only supplement — both paths
		// ride billingHeaders, so both must present the identity.
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMED")
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			gotCT = r.Header.Get("Cosy-ClientType")
			return http.StatusOK, `{"status":"CLAIMED","rewardCredits":100}`
		},
	})
	if _, err := fetchCheckinStatus(cnAuth()); err != nil {
		t.Fatalf("fetchCheckinStatus: %v", err)
	}
	if gotCT != billingClientType {
		t.Fatalf("Cosy-ClientType = %q, want %q — every billing call presents the desktop identity", gotCT, billingClientType)
	}
}
