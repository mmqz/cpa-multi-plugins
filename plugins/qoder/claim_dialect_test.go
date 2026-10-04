// claim_dialect_test.go — v0.8.53: pins the zero-Cosy-Machine* dialect on the
// campaigns WRITE path (claim) and the auxiliary campaign reads (reward,
// limited-number).
//
// Root cause this pins (live-verified 2026-10-04, CN account ud2d62d72): the
// server's anti-fraud layer rejects a claim POST carrying a DERIVED
// (simulated) Cosy-Machine* identity with 503 RISK_DEPENDENCY_UNAVAILABLE
// and grants nothing. The same claim WITHOUT any machine headers — the exact
// wire shape of the official growth-page activity iframe and the user's
// working Python script — answers 200 CLAIMED and grants the benefit.
// v0.8.52 fixed only the campaigns LIST read (fetchCampaignStatusNoMachine
// fallback); the claim still rode the derived headers, so the list showed
// the daily CLAIMABLE row while the claim itself was risk-blocked.
//
// The test does NOT try to reproduce the server's risk decision (that lives
// upstream); it pins the wire contract: a claim POST must carry ZERO
// Cosy-Machine* headers regardless of the account's machine-identity shape,
// and the reward / limited-number reads share the same dialect.
package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestClaimPostSendsNoMachineHeaders: the claim POST must never carry
// Cosy-Machine* headers, even though the account has a (derived) machine
// identity registered and the list fetch still attaches one.
func TestClaimPostSendsNoMachineHeaders(t *testing.T) {
	var claimMachineHeaders []string
	var claimContentType, claimAuth string
	srv := newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			for name := range r.Header {
				if strings.HasPrefix(strings.ToLower(name), "cosy-machine") {
					claimMachineHeaders = append(claimMachineHeaders, name)
				}
			}
			claimContentType = r.Header.Get("Content-Type")
			claimAuth = r.Header.Get("Authorization")
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
		// launch-sync + legacy status supplements are best-effort; answer
		// minimal OK shapes so stray calls never flip the test.
		"/sash/api/v1/me/campaigns/" + clientLaunchCampaignKey + "/limited-number": func(r *http.Request) (int, string) {
			if n := machineHeaderCount(r); n > 0 {
				t.Errorf("limited-number GET carried %d Cosy-Machine* headers, want 0", n)
			}
			return http.StatusOK, `{"hasNumber":true}`
		},
		"/sash/api/v1/me/daily-check-in/status": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"DISABLED"}`
		},
	})
	_ = srv

	sa := cnAuth()
	// Force a derived machine identity so the OLD code path would have
	// attached it — the claim must still go out without it.
	if _, err := performCheckinCall(sa); err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if len(claimMachineHeaders) != 0 {
		t.Fatalf("claim POST carried Cosy-Machine* headers %v — derived identity must never ride the claim (risk-layer 503 RISK_DEPENDENCY_UNAVAILABLE)", claimMachineHeaders)
	}
	if claimAuth == "" || !strings.HasPrefix(claimAuth, "Bearer ") {
		t.Fatalf("claim POST lost the Bearer authorization header: %q", claimAuth)
	}
	if claimContentType != "application/json" {
		t.Fatalf("claim POST Content-Type = %q, want application/json (POST with body)", claimContentType)
	}
}

// TestRewardReadSendsNoMachineHeaders pins the same dialect on the read-only
// reward probe (the VIEW_DETAILS face-value source).
func TestRewardReadSendsNoMachineHeaders(t *testing.T) {
	srv := newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns/camp-x/reward": func(r *http.Request) (int, string) {
			if n := machineHeaderCount(r); n > 0 {
				t.Errorf("reward GET carried %d Cosy-Machine* headers, want 0", n)
			}
			return http.StatusOK, `{"benefit":{"kind":"CREDITS","amount":100}}`
		},
	})
	_ = srv
	if _, err := fetchCampaignReward(cnAuth(), "camp-x"); err != nil {
		t.Fatalf("fetchCampaignReward: %v", err)
	}
}

// machineHeaderCount counts Cosy-Machine* request headers.
func machineHeaderCount(r *http.Request) int {
	n := 0
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "cosy-machine") {
			n++
		}
	}
	return n
}
