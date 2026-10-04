// claim_test.go pins the billing/claim wire contract: the minimal header
// bundle (Authorization + captcha param + app version + platform + device
// mid — and deliberately NO TV identity extras), the {"plan_id"} body, the
// success envelope (2xx + code 0 + data.plan), and the desktop
// classifyClaimCode mapping incl. the 3007 captcha-retry and 401 fallbacks.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func claimTestAuth() *storedAuth {
	return &storedAuth{
		Auth: zcodeTokens{
			JWT:       "test-jwt",
			DeviceMid: "11111111-2222-3333-4444-555555555555",
			Provider:  providerZai,
		},
	}
}

func TestFetchClaimPlanSuccess(t *testing.T) {
	var gotPath, gotBody, gotAuth, gotCap, gotRegion, gotVersion, gotPlatform, gotDevice, gotAgent, gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotCap = r.Header.Get("X-Aliyun-Captcha-Verify-Param")
		gotRegion = r.Header.Get("X-Aliyun-Captcha-Verify-Region")
		gotVersion = r.Header.Get("X-ZCode-App-Version")
		gotPlatform = r.Header.Get("X-Platform")
		gotDevice = r.Header.Get("X-Device-Mid")
		gotAgent = r.Header.Get("X-ZCode-Agent")
		gotSig = r.Header.Get("X-Client-Sig")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"plan":{"plan_id":"weekend-free-1024","starts_at":1758000000,"ends_at":1758600000}}}`)
	}))
	defer srv.Close()
	old := zcodeAPIBase
	zcodeAPIBase = srv.URL + "/api/v1"
	t.Cleanup(func() { zcodeAPIBase = old })

	out := fetchClaimPlan(claimTestAuth(), "weekend-free-1024", "cap-param-blob", "sgp")
	if !out.OK {
		t.Fatalf("expected ok, got %+v", out)
	}
	if gotPath != "/api/v1/zcode-plan/billing/claim" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"plan_id":"weekend-free-1024"`) {
		t.Fatalf("body = %q", gotBody)
	}
	if gotAuth != "Bearer test-jwt" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCap != "cap-param-blob" {
		t.Fatalf("captcha header = %q", gotCap)
	}
	if gotRegion != "sgp" {
		t.Fatalf("region header = %q", gotRegion)
	}
	if gotVersion == "" || gotPlatform == "" {
		t.Fatalf("app version/platform missing: %q %q", gotVersion, gotPlatform)
	}
	if gotDevice != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("X-Device-Mid = %q (campaign gate)", gotDevice)
	}
	// claim plane is the minimal bundle: no TV identity extras
	if gotAgent != "" || gotSig != "" {
		t.Fatalf("claim must not carry the TV identity set: agent=%q sig=%q", gotAgent, gotSig)
	}
	if out.StartsAt != 1758000000 || out.EndsAt != 1758600000 {
		t.Fatalf("window not parsed: %+v", out)
	}
}

func TestFetchClaimPlanBizCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   string
	}{
		{http.StatusOK, `{"code":1003,"msg":"already claimed"}`, "already_claimed"},
		{http.StatusOK, `{"code":1004,"msg":"not eligible"}`, "ineligible"},
		{http.StatusOK, `{"code":1005,"msg":"quota exhausted"}`, "quota_exhausted"},
		{http.StatusOK, `{"code":1001,"msg":"no such plan"}`, "not_found"},
		{http.StatusOK, `{"code":1002,"msg":"unavailable"}`, "unavailable"},
		{http.StatusOK, `{"code":3001,"msg":"parameter error"}`, "invalid_request"},
		{http.StatusOK, `{"code":3007,"msg":"captcha"}`, "captcha"},
		{http.StatusOK, `{"code":9999,"msg":"?"}`, "unknown"},
		{http.StatusUnauthorized, `{"error":"token expired"}`, "login_required"},
		{http.StatusBadGateway, `<html>bad gateway</html>`, "http_error"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		old := zcodeAPIBase
		zcodeAPIBase = srv.URL + "/api/v1"
		out := fetchClaimPlan(claimTestAuth(), "p1", "cap", "sgp")
		zcodeAPIBase = old
		srv.Close()
		if out.OK {
			t.Fatalf("status %d body %s: unexpected ok", tc.status, tc.body)
		}
		if out.Kind != tc.kind {
			t.Fatalf("status %d body %s: kind = %q, want %q", tc.status, tc.body, out.Kind, tc.kind)
		}
	}
}

func TestFetchClaimPlanNoJWT(t *testing.T) {
	out := fetchClaimPlan(&storedAuth{}, "p1", "cap", "sgp")
	if out.OK || out.Kind != "login_required" {
		t.Fatalf("expected login_required, got %+v", out)
	}
	out = fetchClaimPlan(claimTestAuth(), "p1", "", "sgp")
	if out.OK || out.Kind != "captcha" {
		t.Fatalf("expected captcha kind for empty param, got %+v", out)
	}
}

func TestClassifyClaimCode(t *testing.T) {
	if classifyClaimCode(3007) != "captcha" || classifyClaimCode(1003) != "already_claimed" {
		t.Fatal("mapping drifted")
	}
}
