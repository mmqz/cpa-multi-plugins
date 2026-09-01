package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsLegacyCodebuddyAuthName(t *testing.T) {
	cases := map[string]bool{
		"codebuddy-cn-123.json": true,
		"CodeBuddy-CN-123.json": true,
		"codebuddy-cn.json":     true,
		"workbuddy-123.json":    false,
		"workbuddy.json":        false,
		"codebuddy-intl-1.json": false,
		"codebuddy-cn-backup":   true, // loose prefix match, same as hostAuthList
	}
	for name, want := range cases {
		if got := isLegacyCodebuddyAuthName(name); got != want {
			t.Errorf("isLegacyCodebuddyAuthName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestApplyPlatformHeaders_IDE(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyPlatformHeaders(req, "ide")
	if req.Header.Get("X-IDE-Type") != "CodeBuddyIDE" ||
		req.Header.Get("X-IDE-Name") != "CodeBuddyIDE" ||
		req.Header.Get("X-IDE-Version") == "" ||
		req.Header.Get("X-Product-Version") == "" {
		t.Fatal("ide platform must set X-IDE-* headers")
	}
}

func TestApplyPlatformHeaders_CLI(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyPlatformHeaders(req, "CLI")
	if req.Header.Get("X-IDE-Type") != "" || req.Header.Get("X-Product-Version") != "" {
		t.Fatal("CLI platform must NOT set X-IDE-* headers")
	}
}

func TestPlatformForAuth_Defaults(t *testing.T) {
	// nil and empty loginPlatform fall back to CLI (historical workbuddy files)
	if got := platformForAuth(nil); got != "CLI" {
		t.Errorf("platformForAuth(nil) = %q, want CLI", got)
	}
	sa := &storedAuth{}
	if got := platformForAuth(sa); got != "CLI" {
		t.Errorf("platformForAuth(empty) = %q, want CLI", got)
	}
	sa.Auth.LoginPlatform = " ide "
	if got := platformForAuth(sa); got != "ide" {
		t.Errorf("platformForAuth(trimmed) = %q, want ide", got)
	}
}

func TestStoredAuth_LoginPlatformRoundTrip(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "t", LoginPlatform: "ide"}}
	raw, err := buildAuthFileJSON(sa, false, "note", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"loginPlatform":"ide"`) {
		t.Fatalf("loginPlatform must round-trip through auth JSON, got %s", raw)
	}
}
