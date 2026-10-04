package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// parseLoginAccount — tolerant account payload shapes
// -----------------------------------------------------------------------------

func TestParseLoginAccountShapes(t *testing.T) {
	ok := map[string]accountData{
		`{"uid":"12345","nickname":"n","enterpriseId":"e"}`: {UID: "12345", Nickname: "n", EnterpriseID: "e"},
		`{"uid":12345}`: {UID: "12345"},
		`{"user":{"uid":"u-1","nickname":"nick"}}`: {UID: "u-1", Nickname: "nick"},
		`{"account":{"uid":"777"}}`:                {UID: "777"},
		`{"info":{"uid":"i-9"}}`:                   {UID: "i-9"},
		`{"profile":{"uid":"p-0"}}`:                {UID: "p-0"},
	}
	for raw, want := range ok {
		got, gotOK := parseLoginAccount(json.RawMessage(raw))
		if !gotOK {
			t.Errorf("parseLoginAccount(%s) = not ok, want ok", raw)
			continue
		}
		if got != want {
			t.Errorf("parseLoginAccount(%s) = %+v, want %+v", raw, got, want)
		}
	}
	bad := []string{
		``,
		`null`,
		`{"nickname":"no-uid"}`,
		`{"uid":""}`,
		`{"uid":null}`,
		`{"user":{"nickname":"nested-no-uid"}}`,
		`[1,2,3]`,
		`"plain string"`,
	}
	for _, raw := range bad {
		if _, gotOK := parseLoginAccount(json.RawMessage(raw)); gotOK {
			t.Errorf("parseLoginAccount(%s) = ok, want not ok", raw)
		}
	}
}

// -----------------------------------------------------------------------------
// uidFromTokenClaims — JWT identity fallback
// -----------------------------------------------------------------------------

func jwtWithPayload(t *testing.T, payload string) string {
	t.Helper()
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return enc(`{"alg":"none"}`) + "." + enc(payload) + ".sig"
}

func TestUIDFromTokenClaims(t *testing.T) {
	cases := []struct {
		name string
		tok  string
		want string
	}{
		{"uid claim", jwtWithPayload(t, `{"iss":"codebuddy.cn","uid":"424242"}`), "424242"},
		{"numeric uid", jwtWithPayload(t, `{"uid":424242}`), "424242"},
		{"user_id fallback", jwtWithPayload(t, `{"user_id":"777"}`), "777"},
		{"userId fallback", jwtWithPayload(t, `{"userId":"888"}`), "888"},
		{"sub fallback", jwtWithPayload(t, `{"sub":"999"}`), "999"},
		{"sub uri rejected", jwtWithPayload(t, `{"sub":"https://accounts.example.com/999"}`), ""},
		{"sub email rejected", jwtWithPayload(t, `{"sub":"a@b.c"}`), ""},
		{"empty claims", jwtWithPayload(t, `{}`), ""},
		{"not a jwt", "opaque-token", ""},
		{"bad base64", "a.!!!.c", ""},
		{"uid preferred over sub", jwtWithPayload(t, `{"uid":"1","sub":"2"}`), "1"},
	}
	for _, tc := range cases {
		if got := uidFromTokenClaims(tc.tok); got != tc.want {
			t.Errorf("%s: uidFromTokenClaims = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// fetchLoginAccountAt — retry ladder over a real HTTP server
// -----------------------------------------------------------------------------

func withCollapsedRetryDelays(t *testing.T) {
	t.Helper()
	old := loginAcctRetryDelays
	loginAcctRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { loginAcctRetryDelays = old })
}

func TestFetchLoginAccountRetriesThroughTransientFailure(t *testing.T) {
	withCollapsedRetryDelays(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			// openresty race: 401 right after token success
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"uid":"42","nickname":"acct"}}`))
	}))
	defer srv.Close()

	acct, err := fetchLoginAccountAt(srv.Client(), srv.URL+"/acct?state=", "cn", "s1", "tok")
	if err != nil {
		t.Fatalf("fetchLoginAccountAt returned error: %v", err)
	}
	if acct.UID != "42" || acct.Nickname != "acct" {
		t.Fatalf("account = %+v, want uid 42", acct)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two transient failures then success)", attempts)
	}
}

func TestFetchLoginAccountRetriesThroughNoUIDPayload(t *testing.T) {
	withCollapsedRetryDelays(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"nickname":"nameless"}}`))
	}))
	defer srv.Close()

	acct, err := fetchLoginAccountAt(srv.Client(), srv.URL+"/acct?state=", "cn", "s1", "tok")
	if err == nil {
		t.Fatalf("fetchLoginAccountAt = %+v, want error for uid-less payloads", acct)
	}
	if !strings.Contains(err.Error(), "no uid") {
		t.Fatalf("error %q should mention the missing uid", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

// -----------------------------------------------------------------------------
// hostAuthList name filter — claim/list predicate symmetry
// -----------------------------------------------------------------------------

func TestHostAuthListNameFilterSymmetry(t *testing.T) {
	// Names the OLD inline prefix rule accepted must still be accepted
	// (superset guarantee), and the legacy bare names must now be listed
	// too — handleParseAuth and hostAuthList share isOurFamilyFileName.
	accepted := map[string]bool{
		"workbuddy-123.json":      true,
		"WorkBuddy-ABC.json":      true,
		"codebuddy-cn-9.json":     true,
		"codebuddy-intl-3.json":   true,
		"workbuddy.json":          true, // v0.9.43: legacy bare name, no longer orphaned
		"codebuddy.json":          true, // v0.9.43: legacy bare name
		"codebuddy-cn.json":       true,
		"codebuddy-intl.json":     true,
		".trae-usage.json":        false, // manager usage snapshot, never a credential
		"workbuddy-usage.json":    true,  // prefix-matches; rejected later by parse content, shown as broken row
		"qoder-1.json":            false,
		"trae-1.json":             false,
		"workbuddy-backup":        true, // loose prefix, same as before
		"mimo-cookie-1.json":      false,
		"random-notes.json":       false,
		"workbuddy-intl-x@y.json": true, // odd but family-prefixed; content guard decides
	}
	for name, want := range accepted {
		if got := isOurFamilyFileName(name); got != want {
			t.Errorf("isOurFamilyFileName(%q) = %v, want %v", name, got, want)
		}
	}
	// The old prefix rule's acceptance set is a subset of the new predicate.
	oldRule := func(name string) bool {
		lower := strings.ToLower(strings.TrimSpace(name))
		for _, p := range []string{"workbuddy-", "codebuddy-cn-", "codebuddy-intl-"} {
			if strings.HasPrefix(lower, p) {
				return true
			}
		}
		return false
	}
	for name := range accepted {
		if oldRule(name) && !isOurFamilyFileName(name) {
			t.Errorf("regression: old prefix rule accepted %q but new predicate rejects it", name)
		}
	}
}
