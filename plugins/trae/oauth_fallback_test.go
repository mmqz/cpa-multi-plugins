package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

func TestEnsureHTTPSScheme(t *testing.T) {
	cases := []struct{ in, want string }{
		{"www.trae.cn", "https://www.trae.cn"},
		{"https://www.trae.ai", "https://www.trae.ai"},
		{"http://api.trae.cn", "http://api.trae.cn"},
		{"  www.trae.cn  ", "https://www.trae.cn"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ensureHTTPSScheme(c.in); got != c.want {
			t.Errorf("ensureHTTPSScheme(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// The production 404 regression: GetLoginGuidance returns a BARE domain
// ("www.trae.cn"). buildVerificationURI must produce a full https:// URL —
// a scheme-less URL is resolved relative to the CPA panel origin and lands
// on the CPA server itself → gin default 404 "404 page not found".
func TestBuildVerificationURIAlwaysHasScheme(t *testing.T) {
	for _, loginHost := range []string{"www.trae.cn", "https://www.trae.cn", "www.trae.ai"} {
		u := buildVerificationURI(loginHost, verificationURIParams{
			AuthFrom:      oauthAuthFrom,
			PluginVersion: oauthPluginVersion,
			ClientID:      upstream.ClientID,
			LoginTraceID:  "trace-123",
			CallbackURL:   "http://127.0.0.1:9/authorize",
			MachineID:     "m",
			DeviceID:      "d",
			CodeChallenge: "cc",
		})
		if !strings.HasPrefix(u, "https://") {
			t.Fatalf("verification URI for loginHost %q lacks https scheme: %q", loginHost, u)
		}
		if !strings.Contains(u, "/authorization?") {
			t.Fatalf("verification URI missing /authorization path: %q", u)
		}
	}
}

// candidateAPIOorigins must rewrite www.→api. (cockpit-tools
// candidate_api_origins) so ExchangeToken never posts to the www HTML host.
func TestCandidateAPIOoriginsRewrite(t *testing.T) {
	origins := candidateAPIOorigins("https://www.trae.cn", true)
	joined := strings.Join(origins, ",")
	for _, want := range []string{"https://www.trae.cn", "https://api.trae.cn", "https://api.trae.com.cn"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CN origins missing %s: %v", want, origins)
		}
	}
	// cockpit-tools candidate order: loginHost origin first, then the api.
	// rewrite, then platform defaults — the loop SKIPS candidates that fail
	// (www hosts return HTML), so membership is what matters, not order.

	intl := candidateAPIOorigins("www.trae.ai", false)
	ij := strings.Join(intl, ",")
	for _, want := range []string{"https://www.trae.ai", "https://api.trae.ai", "https://grow-normal.trae.ai", "https://grow-normal.traeapi.us"} {
		if !strings.Contains(ij, want) {
			t.Errorf("Intl origins missing %s: %v", want, intl)
		}
	}
}

// exchangeTokenCandidates must skip failing candidates (404 / HTML bodies)
// and return the first URL that yields an access token.
func TestExchangeTokenCandidatesFallback(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer notFound.Close()

	htmlHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// www.* style host: HTTP 200 with an HTML page (not JSON).
		_, _ = w.Write([]byte("<!doctype html><html><body>spa</body></html>"))
	}))
	defer htmlHost.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"AccessToken": "at-123", "RefreshToken": "rt-456"},
		})
	}))
	defer good.Close()

	urls := []string{notFound.URL + "/trae/api/v3/oauth/ExchangeToken", htmlHost.URL + "/trae/api/v3/oauth/ExchangeToken", good.URL + "/trae/api/v3/oauth/ExchangeToken"}
	raw, err := exchangeTokenCandidates(urls, []byte(`{}`))
	if err != nil {
		t.Fatalf("exchangeTokenCandidates: %v", err)
	}
	if !strings.Contains(string(raw), "at-123") {
		t.Fatalf("unexpected body: %s", raw)
	}
	if at := extractExchangeAccessToken(raw); at != "at-123" {
		t.Fatalf("extractExchangeAccessToken=%q want at-123", at)
	}
}

// All candidates failing (the reported upstream behavior) must aggregate the
// per-URL errors into one message so the panel shows what actually happened.
func TestExchangeTokenCandidatesAllFail(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer notFound.Close()

	urls := []string{notFound.URL + "/x", notFound.URL + "/y"}
	_, err := exchangeTokenCandidates(urls, []byte(`{}`))
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should mention the upstream status: %v", err)
	}
}
