// preview_test.go pins the billing/preview wire contract: URL, header set
// (Authorization + X-Device-Mid — the campaign gateway 3001 gate; TV plane
// without X-ZCode-Agent), the snake/camel tolerant parse with the desktop
// parser's skip rules, the 404-as-empty semantics, and the dedupe merge.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func previewTestAuth() *storedAuth {
	return &storedAuth{
		Auth: zcodeTokens{
			JWT:       "test-jwt",
			DeviceMid: "11111111-2222-3333-4444-555555555555",
			Provider:  providerZai,
		},
	}
}

func TestFetchClaimablePreview(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotDevice, gotAccept, gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Mid")
		gotAccept = r.Header.Get("Accept")
		gotAgent = r.Header.Get("X-ZCode-Agent")
		w.Header().Set("Content-Type", "application/json")
		// Live line shape adapted from the wk-0918 campaign fixtures: one
		// 300,000,000-token GLM-5.3-Flash trial pack plus parser edge cases
		// (entitlement without id is skipped, plan without plan_id dropped).
		_, err := io.WriteString(w, `{"code":0,"msg":"ok","data":{"plans":[`+
			`{"plan_id":"weekend-free-1024","name":"Weekend Free","description":"weekend trial","priority":10,`+
			`"starts_at":1758000000,"ends_at":1758600000,"entitlements":[`+
			`{"entitlement_id":"e-flash","show_name":"GLM-5.3-Flash trial pack","grant_units":300000000,"unit_type":"token","capabilities":["chat"],"period":"weekly","priority":1},`+
			`{"entitlement_id":"","show_name":"skipped-no-id"}]},`+
			`{"plan_id":"","name":"dropped-no-id"}]}}`)
		if err != nil {
			t.Errorf("write fixture: %v", err)
		}
	}))
	defer srv.Close()
	old := zcodeAPIBase
	zcodeAPIBase = srv.URL + "/api/v1"
	t.Cleanup(func() { zcodeAPIBase = old })

	plans, err := fetchClaimablePreview(previewTestAuth())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/api/v1/zcode-plan/billing/preview" {
		t.Fatalf("path = %q", gotPath)
	}
	for _, key := range []string{"app_version=", "platform="} {
		if !strings.Contains(gotQuery, key) {
			t.Fatalf("query %q missing %q", gotQuery, key)
		}
	}
	if gotAuth != "Bearer test-jwt" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotDevice != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("X-Device-Mid = %q (campaign gateway 3001 gate)", gotDevice)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q", gotAccept)
	}
	if gotAgent != "" {
		t.Fatalf("X-ZCode-Agent = %q (TV plane must drop it)", gotAgent)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1 (id-less plan dropped)", len(plans))
	}
	p := plans[0]
	if p.PlanID != "weekend-free-1024" || p.Name != "Weekend Free" || p.Priority != 10 {
		t.Fatalf("plan meta = %+v", p)
	}
	if p.StartsAt != 1758000000 || p.EndsAt != 1758600000 {
		t.Fatalf("window = %d..%d", p.StartsAt, p.EndsAt)
	}
	if len(p.Entitlements) != 1 {
		t.Fatalf("entitlements = %d, want 1 (id-less skipped)", len(p.Entitlements))
	}
	e := p.Entitlements[0]
	if e.Name != "GLM-5.3-Flash trial pack" || e.Grant != 300000000 || e.Unit != "token" {
		t.Fatalf("entitlement = %+v", e)
	}
	if len(e.Capabilities) != 1 || e.Capabilities[0] != "chat" {
		t.Fatalf("capabilities = %v", e.Capabilities)
	}
}

func TestFetchClaimablePreviewNoCampaign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	old := zcodeAPIBase
	zcodeAPIBase = srv.URL + "/api/v1"
	t.Cleanup(func() { zcodeAPIBase = old })

	plans, err := fetchClaimablePreview(previewTestAuth())
	if err != nil {
		t.Fatalf("404 must read as no-campaign, got error: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans = %d, want 0", len(plans))
	}
}

func TestFetchClaimablePreviewBizError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":3001,"msg":"parameter error"}`)
	}))
	defer srv.Close()
	old := zcodeAPIBase
	zcodeAPIBase = srv.URL + "/api/v1"
	t.Cleanup(func() { zcodeAPIBase = old })

	if _, err := fetchClaimablePreview(previewTestAuth()); err == nil || !strings.Contains(err.Error(), "code=3001") {
		t.Fatalf("want biz 3001 error, got %v", err)
	}
}

func TestFetchClaimablePreviewNoJWT(t *testing.T) {
	sa := previewTestAuth()
	sa.Auth.JWT = ""
	if _, err := fetchClaimablePreview(sa); err == nil || !strings.Contains(err.Error(), "no plan JWT") {
		t.Fatalf("want no-JWT error, got %v", err)
	}
}

func TestMergePreviewResults(t *testing.T) {
	results := []accountPreview{
		{Account: "a1", Plans: []previewPlan{
			{PlanID: "p1", Name: "Weekend Free"},
			{PlanID: "p2", Name: "Night Free"},
		}},
		{Account: "a2", Plans: []previewPlan{
			{PlanID: "p1", Name: "Weekend Free"},
		}},
		{Account: "a3", Skipped: true}, // no JWT — invisible
		{Account: "a4", Err: errString("boom")},
	}
	plans, errs, checked := mergePreviewResults(results)
	if checked != 3 {
		t.Fatalf("checked = %d, want 3 (skipped excluded)", checked)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	if plans[0].PlanID != "p1" || plans[0].Accounts != 2 {
		t.Fatalf("p1 merge = %+v", plans[0])
	}
	if plans[1].PlanID != "p2" || plans[1].Accounts != 1 {
		t.Fatalf("p2 merge = %+v", plans[1])
	}
	if len(errs) != 1 || errs[0]["account"] != "a4" {
		t.Fatalf("errs = %v", errs)
	}
}

// errString is a tiny error impl keeping the merge test dependency-free.
type errString string

func (e errString) Error() string { return string(e) }
