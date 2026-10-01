// pro_upgrade_retired_test.go — v0.8.33: the 领取Pro flow must tolerate the
// gateway-prefix ambiguity. Field report (2026-10-01): a healthy CN account
// got "领取失败 eligibility: http 404" — our calls carry the /sash activity
// prefix (same prefix campaigns verifiably requires) while the desktop
// client's binary strings show the bare /api/v1/me/pro-upgrade/* form. Two
// candidate roots: prefix drift (fixable by fallback) or the one-time promo
// being retired (must render as an actionable "已下线" line, not a bare
// "http 404"). These tests pin both behaviors on both call sites.
package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestCheckProUpgradeEligibilityFallsBackToBarePrefix(t *testing.T) {
	sashHits, bareHits := 0, 0
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/pro-upgrade/eligibility": func(r *http.Request) (int, string) {
			sashHits++
			return http.StatusNotFound, `{"error":"not found"}`
		},
		"/api/v1/me/pro-upgrade/eligibility": func(r *http.Request) (int, string) {
			bareHits++
			return http.StatusOK, `{"eligible":true}`
		},
	})
	eligible, err := checkProUpgradeEligibility(cnAuth())
	if err != nil {
		t.Fatalf("checkProUpgradeEligibility: %v", err)
	}
	if !eligible {
		t.Fatal("eligible = false, want true (bare-prefix response)")
	}
	if sashHits != 1 || bareHits != 1 {
		t.Fatalf("probe order wrong: sash=%d bare=%d, want 1/1", sashHits, bareHits)
	}
}

func TestCheckProUpgradeEligibilityRetiredSentinel(t *testing.T) {
	hits := 0
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/pro-upgrade/eligibility": func(r *http.Request) (int, string) {
			hits++
			return http.StatusNotFound, `{}`
		},
		"/api/v1/me/pro-upgrade/eligibility": func(r *http.Request) (int, string) {
			hits++
			return http.StatusNotFound, `{}`
		},
	})
	_, err := checkProUpgradeEligibility(cnAuth())
	if !errors.Is(err, errProUpgradeRetired) {
		t.Fatalf("err = %v, want errProUpgradeRetired", err)
	}
	if hits != 2 {
		t.Fatalf("probes = %d, want both prefixes tried (2)", hits)
	}
}

func TestCheckProUpgradeEligibilityNon404ErrorStays(t *testing.T) {
	// A 403 on the primary prefix must surface as a plain "http 403" error —
	// fallback is a 404-only escape hatch, never a blanket retry.
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/pro-upgrade/eligibility": func(r *http.Request) (int, string) {
			return http.StatusForbidden, `{"error":"forbidden"}`
		},
	})
	_, err := checkProUpgradeEligibility(cnAuth())
	if err == nil || err.Error() != "http 403" {
		t.Fatalf("err = %v, want plain http 403", err)
	}
}

func TestClaimProUpgradeFallsBackToBarePrefix(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/pro-upgrade/claim": func(r *http.Request) (int, string) {
			return http.StatusNotFound, `{}`
		},
		"/api/v1/me/pro-upgrade/claim": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"result":"CLAIMED"}`
		},
	})
	res, err := claimProUpgrade(cnAuth())
	if err != nil {
		t.Fatalf("claimProUpgrade: %v", err)
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("success = %+v, want true (bare-prefix claim)", res)
	}
}

func TestClaimProUpgradeRetiredMap(t *testing.T) {
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/pro-upgrade/claim": func(r *http.Request) (int, string) {
			return http.StatusNotFound, `{}`
		},
		"/api/v1/me/pro-upgrade/claim": func(r *http.Request) (int, string) {
			return http.StatusNotFound, `{}`
		},
	})
	res, err := claimProUpgrade(cnAuth())
	if err != nil {
		t.Fatalf("claimProUpgrade: %v", err)
	}
	if success, _ := res["success"].(bool); success {
		t.Fatal("success = true, want false for retired promo")
	}
	if reason, _ := res["reason"].(string); reason != "retired" {
		t.Fatalf("reason = %v, want retired", res["reason"])
	}
	if msg, _ := res["message"].(string); msg == "" {
		t.Fatal("message empty, want actionable 已下线 line")
	}
}
