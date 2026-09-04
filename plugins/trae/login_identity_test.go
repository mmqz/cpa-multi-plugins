// login_identity_test.go — v0.12.25 duplicate-account regression tests.
//
// The bug: the same account logged in twice landed in two credential files
// (trae-<loginTraceID-1>.json + trae-<loginTraceID-2>.json) because the uid
// fallback was the PER-LOGIN trace id. The identity chain is now
// GetUserInfo → callback userInfo echo → stable per-realm unknown name.
package main

import (
	"net/url"
	"testing"
)

func TestParseCallbackIdentity(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantUID  string
		wantNick string
	}{
		{
			name:     "plain json userInfo",
			raw:      `{"UserID":"u-123","ScreenName":"init"}`,
			wantUID:  "u-123",
			wantNick: "init",
		},
		{
			name:     "alternate key spellings",
			raw:      `{"userId":"u-9","nickname":"nick"}`,
			wantUID:  "u-9",
			wantNick: "nick",
		},
		{
			name:     "json-quoted json (double encoded)",
			raw:      `"{\"UserID\":\"u-77\",\"ScreenName\":\"nn\"}"`,
			wantUID:  "u-77",
			wantNick: "nn",
		},
		{
			name:    "absent parameter",
			raw:     "",
			wantUID: "",
		},
		{
			name:    "garbage value ignored",
			raw:     `not json at all`,
			wantUID: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals := url.Values{}
			if tc.raw != "" {
				vals.Set("userInfo", tc.raw)
			}
			id := parseCallbackIdentity(vals)
			if id.UID != tc.wantUID {
				t.Fatalf("uid = %q, want %q", id.UID, tc.wantUID)
			}
			if tc.wantNick != "" && id.Nickname != tc.wantNick {
				t.Fatalf("nickname = %q, want %q", id.Nickname, tc.wantNick)
			}
		})
	}
}

func TestParseCallbackIdentityAlternateSpellings(t *testing.T) {
	vals := url.Values{}
	vals.Set("user_info", `{"uid":"intl-1","name":"Intl Guy"}`)
	id := parseCallbackIdentity(vals)
	if id.UID != "intl-1" || id.Nickname != "Intl Guy" {
		t.Fatalf("uid=%q nickname=%q", id.UID, id.Nickname)
	}
}

// TestResolveLoginUID_NoPerLoginFallback pins the duplicate-account fix: two
// consecutive logins of the same account with GetUserInfo unavailable resolve
// to the SAME file identity (previously: two fresh UUIDs → two files → the
// same account listed multiple times in the panel).
func TestResolveLoginUID_NoPerLoginFallback(t *testing.T) {
	first := resolveLoginUID("", "", "cn")
	second := resolveLoginUID("", "", "cn")
	if first == "" {
		t.Fatal("empty fallback identity")
	}
	if first != second {
		t.Fatalf("fallback identity not stable: %q vs %q", first, second)
	}
	if first == "some-login-trace-id" {
		t.Fatal("fallback must never be the per-login trace id")
	}

	intlFirst := resolveLoginUID("", "", "intl")
	intlSecond := resolveLoginUID("", "", "intl")
	if intlFirst != intlSecond {
		t.Fatalf("intl fallback not stable: %q vs %q", intlFirst, intlSecond)
	}
	if intlFirst == first {
		t.Fatal("cn and intl unknown fallbacks must not collide (shared file name space)")
	}

	// v0.12.27: solo has its own fallback — a solo login with an
	// unresolvable identity must not overwrite (flip the variant of) a
	// cn login in the same situation.
	soloFirst := resolveLoginUID("", "", "solo")
	soloSecond := resolveLoginUID("", "", "solo")
	if soloFirst == "" || soloFirst != soloSecond {
		t.Fatalf("solo fallback not stable: %q vs %q", soloFirst, soloSecond)
	}
	if soloFirst == first || soloFirst == intlFirst {
		t.Fatalf("solo fallback must not collide with cn/intl: got %q", soloFirst)
	}
}

func TestResolveLoginUID_Precedence(t *testing.T) {
	if got := resolveLoginUID("real-uid", "cb-uid", "cn"); got != "real-uid" {
		t.Fatalf("GetUserInfo must win: got %q", got)
	}
	if got := resolveLoginUID("", "cb-uid", "cn"); got != "cb-uid" {
		t.Fatalf("callback echo must win over fallback: got %q", got)
	}
	if got := resolveLoginUID("  ", "  cb-2  ", "cn"); got != "cb-2" {
		t.Fatalf("trimmed callback echo expected, got %q", got)
	}
}

func TestResolveLoginNickname(t *testing.T) {
	if got := resolveLoginNickname("fresh", "echo"); got != "fresh" {
		t.Fatalf("GetUserInfo nickname must win, got %q", got)
	}
	if got := resolveLoginNickname("", "echo"); got != "echo" {
		t.Fatalf("callback nickname expected, got %q", got)
	}
	if got := resolveLoginNickname("", ""); got != "" {
		t.Fatalf("empty expected, got %q", got)
	}
}
