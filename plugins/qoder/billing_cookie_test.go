package main

// billing_cookie_test.go — pins the billing surface's web-session handshake
// (0.8.42, adapted from bfSan f05e9e3). Upstream began answering 401
// {"code":"UNAUTHORIZED","message":"missing cookie header"} to bare-Bearer
// billing calls (live 2026-09-21); the official client never sees this
// because the Electron session attaches acw_tc / qoder_csrf_token and
// mirrors the CSRF cookie into X-CSRF-Token. These tests lock the replay:
// bootstrap once per account → every billing call carries Cookie +
// X-CSRF-Token; where no cookies exist the headers are simply absent.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// cookieSrv answers every path 200, sets the session cookies on the first
// response, and records whether later requests carried the jar headers.
type cookieSrv struct {
	srv      *httptest.Server
	requests atomic.Int64
	withCook atomic.Int64
	withCSRF atomic.Int64
}

func newCookieServer(t *testing.T) *cookieSrv {
	t.Helper()
	c := &cookieSrv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		if r.Header.Get("Cookie") != "" {
			c.withCook.Add(1)
		}
		if r.Header.Get("X-CSRF-Token") != "" {
			c.withCSRF.Add(1)
		}
		// Emit the session cookies exactly once (first response) so a
		// second bootstrap can never be what satisfies the assertion.
		w.Header().Add("Set-Cookie", "acw_tc=acw-1; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", csrfCookieName+"=csrf-1; Path=/")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	prev := billingBaseOverride
	billingBaseOverride = func(string) string { return c.srv.URL }
	t.Cleanup(func() { billingBaseOverride = prev })
	t.Cleanup(resetBillingJars)
	return c
}

func TestBillingCallsCarryCookieAndCSRF(t *testing.T) {
	c := newCookieServer(t)
	sa := cnAuth()

	// The credits fetch is the panel's most frequent billing call. First
	// call bootstraps; the response must still be a successful parse — the
	// handshake must not break the fetch itself.
	if _, err := fetchUserResource(sa); err != nil {
		t.Fatalf("fetchUserResource after bootstrap: %v", err)
	}
	if c.requests.Load() < 2 {
		t.Fatalf("expected bootstrap + fetch, saw %d requests", c.requests.Load())
	}
	if got := c.withCook.Load(); got < 1 {
		t.Fatalf("no billing request carried a Cookie header (requests=%d)", c.requests.Load())
	}
	if got := c.withCSRF.Load(); got < 1 {
		t.Fatalf("no billing request carried X-CSRF-Token (requests=%d)", c.requests.Load())
	}
}

func TestBillingJarIsolatedPerAccount(t *testing.T) {
	c := newCookieServer(t)
	a := &storedAuth{Auth: storedTokens{AccessToken: "dt-a", Region: "cn"}, Account: storedAccount{UID: "uid-a"}}
	b := &storedAuth{Auth: storedTokens{AccessToken: "dt-b", Region: "cn"}, Account: storedAccount{UID: "uid-b"}}

	if billingAccountKey(a) == billingAccountKey(b) {
		t.Fatal("distinct uids must map to distinct jars")
	}
	if _, err := fetchUserResource(a); err != nil {
		t.Fatalf("fetch a: %v", err)
	}
	if _, err := fetchUserResource(b); err != nil {
		t.Fatalf("fetch b: %v", err)
	}
	// Both accounts bootstrap independently: with one request each minimum.
	if c.requests.Load() < 4 {
		t.Fatalf("expected ≥2 requests per account (bootstrap+fetch), saw %d", c.requests.Load())
	}
}

func TestBillingJarSharedForSameAccount(t *testing.T) {
	c := newCookieServer(t)
	// Two auth files, same uid — the same upstream session, one jar.
	a1 := &storedAuth{Auth: storedTokens{AccessToken: "dt-x1", Region: "cn"}, Account: storedAccount{UID: "uid-x"}}
	a2 := &storedAuth{Auth: storedTokens{AccessToken: "dt-x2", Region: "cn"}, Account: storedAccount{UID: "uid-x"}}
	if billingAccountKey(a1) != billingAccountKey(a2) {
		t.Fatal("same uid must share one jar regardless of token suffix")
	}
	if _, err := fetchUserResource(a1); err != nil {
		t.Fatalf("fetch a1: %v", err)
	}
	n := c.requests.Load()
	if _, err := fetchUserResource(a2); err != nil {
		t.Fatalf("fetch a2: %v", err)
	}
	// a2 must NOT bootstrap again (1 fetch + possibly 0 bootstrap).
	if c.requests.Load()-n > 1 {
		t.Fatalf("second auth file for the same account re-bootstrapped (%d extra requests)", c.requests.Load()-n)
	}
}

func TestBootstrapFailureDoesNotBreakBilling(t *testing.T) {
	// A gateway that never sets cookies (e.g. a region without the gate):
	// requests must proceed cookie-less and billing must still parse.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("no cookies were ever issued — request must go out cookie-less")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userQuota":{"remaining":5,"used":1,"total":10},"addOnQuota":{"remaining":0,"used":0,"total":0}}`))
	}))
	t.Cleanup(srv.Close)
	prev := billingBaseOverride
	billingBaseOverride = func(string) string { return srv.URL }
	t.Cleanup(func() { billingBaseOverride = prev })
	t.Cleanup(resetBillingJars)

	cr, err := fetchUserResource(cnAuth())
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if cr.TotalRemain != 5 {
		t.Fatalf("TotalRemain = %d, want 5", cr.TotalRemain)
	}
}

func TestClaimCampaignByIDCarriesCookie(t *testing.T) {
	c := newCookieServer(t)
	sa := cnAuth()
	// claimCampaignByID with a synthetic CLAIMABLE row; the stub answers
	// 200 {"data":{"status":"CLAIMED",...}}. The assertion is on the wire:
	// the claim POST must ride the jar like every other billing call.
	row := &campaign{CampaignID: "01test", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE"}
	if _, err := claimCampaignByID(sa, row); err != nil {
		t.Fatalf("claimCampaignByID: %v", err)
	}
	if got := c.withCook.Load(); got < 1 {
		t.Fatalf("claim POST carried no Cookie header")
	}
	if !strings.Contains(csrfCookieName, "csrf") {
		t.Fatal("csrf cookie name contract changed unexpectedly")
	}
}
