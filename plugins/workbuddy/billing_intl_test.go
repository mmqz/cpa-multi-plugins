package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBillingBaseFor_Regions pins the region→billing-gateway routing.
// Regression guard for the v0.12.10 fix: Intl (codebuddy.ai) accounts used to
// be routed to the CN gas station (www.codebuddy.cn), whose APISIX gateway
// answered with the 401 HTML page that surfaced as
// "parse failed: invalid character '<'" right after a successful Intl login.
func TestBillingBaseFor_Regions(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{"codebuddy.ai", "https://www.codebuddy.ai"},        // Intl → Intl gateway
		{"www.codebuddy.ai", "https://www.codebuddy.ai"},    // subdomain variant
		{"workbuddy.ai", "https://www.workbuddy.ai"},        // Global → Global gateway
		{"copilot.tencent.com", "https://www.codebuddy.cn"}, // CN → CN gas station
		{"", "https://www.codebuddy.cn"},                    // legacy files default CN
	}
	for _, c := range cases {
		sa := &storedAuth{}
		if c.domain != "" {
			sa.Auth.Domain = c.domain
		}
		got := billingBaseFor(sa)
		if got != c.want {
			t.Errorf("domain=%q: got %s, want %s", c.domain, got, c.want)
		}
	}
}

// TestBillingHeaders_IntlRealm verifies the Intl gateway's IDE client header
// set is applied for Intl accounts (the gateway treats header-less calls as
// browser traffic and bounces them).
func TestBillingHeaders_IntlRealm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-IDE-Type"); got != "IDE" {
			t.Errorf("X-IDE-Type = %q, want IDE", got)
		}
		if r.Header.Get("X-Requested-With") != "" {
			t.Error("X-Requested-With should be dropped for Intl accounts")
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want Bearer ...", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	sa := &storedAuth{}
	sa.Auth.Domain = "codebuddy.ai"
	sa.Auth.AccessToken = "intl-test-token"
	if _, err := billingCallOnce(sa, "/probe", nil); err != nil {
		t.Fatalf("billingCallOnce: %v", err)
	}
}
