package main

import "testing"

// v0.12.15: Intl (codebuddy.ai) credentials previously surfaced as "CN" in
// the credential manager — accountRegion only knew workbuddy.ai Global and
// defaulted everything else to cn. The fix adds an explicit auth.region
// (written at login/import) plus a codebuddy.ai domain-sniff fallback, and
// the display helpers gain the INTL state.

func TestAccountRegionV01215(t *testing.T) {
	cases := []struct {
		name   string
		sa     *storedAuth
		region string
	}{
		// explicit region wins over any domain
		{"explicit intl wins", &storedAuth{Auth: storedTokens{Region: "intl", Domain: "copilot.tencent.com"}}, "intl"},
		{"explicit global wins", &storedAuth{Auth: storedTokens{Region: "global"}}, "global"},
		{"explicit cn wins", &storedAuth{Auth: storedTokens{Region: "cn", Domain: "codebuddy.ai"}}, "cn"},
		{"explicit case/space tolerant", &storedAuth{Auth: storedTokens{Region: " INTL "}}, "intl"},
		// legacy sniff: codebuddy.ai is Intl, NOT cn
		{"intl domain bare", &storedAuth{Auth: storedTokens{Domain: "codebuddy.ai"}}, "intl"},
		{"intl domain subdomain", &storedAuth{Auth: storedTokens{Domain: "api.codebuddy.ai"}}, "intl"},
		{"intl domain uppercase", &storedAuth{Auth: storedTokens{Domain: "CodeBuddy.AI"}}, "intl"},
		// legacy sniff regression guards (old behaviour must survive)
		{"global domain still global", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, "global"},
		{"cn domain still cn", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, "cn"},
		{"empty still cn", &storedAuth{}, "cn"},
		{"nil still cn", nil, "cn"},
	}
	for _, tc := range cases {
		if got := accountRegion(tc.sa); got != tc.region {
			t.Errorf("%s: accountRegion() = %q, want %q", tc.name, got, tc.region)
		}
	}
}

func TestDisplayNoteThreeWayV01215(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"intl note", &storedAuth{Auth: storedTokens{Region: "intl"}}, "INTL · 积分未知"},
		{"global note", &storedAuth{Auth: storedTokens{Domain: "workbuddy.ai"}}, "Global · 积分未知"},
		{"cn note", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, "CN · 积分未知"},
	}
	for _, tc := range cases {
		if got := displayNote(tc.sa, nil, false); got != tc.want {
			t.Errorf("%s: displayNote() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLabelForAuthThreeWayV01215(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"intl label", &storedAuth{Auth: storedTokens{Region: "intl"}}, "WorkBuddy [Intl]"},
		{"global label", &storedAuth{Auth: storedTokens{Domain: "workbuddy.ai"}}, "WorkBuddy [Global]"},
		{"cn label", &storedAuth{}, "WorkBuddy [CN]"},
		{"nickname kept", &storedAuth{Account: storedAccount{Nickname: "Alice"}, Auth: storedTokens{Region: "intl"}}, "Alice [Intl]"},
	}
	for _, tc := range cases {
		if got := labelForAuth(tc.sa); got != tc.want {
			t.Errorf("%s: labelForAuth() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
