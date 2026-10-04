package main

import (
	"encoding/json"
	"testing"
	"time"
)

func decodeLoginStatus(t *testing.T) loginStatusPayload {
	t.Helper()
	raw := handleLoginStatusResource()
	var p loginStatusPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("login_status payload not valid JSON: %v (%s)", err, raw)
	}
	return p
}

func TestLoginStatusResourceNoPending(t *testing.T) {
	// Defensive: other tests in this file clean up after themselves, so the
	// only way this fails is state leaked from an earlier file.
	p := decodeLoginStatus(t)
	if p.Pending {
		t.Fatalf("expected pending=false with no live logins, got %+v", p)
	}
	if p.CallbackHP != "" || p.Variant != "" {
		t.Fatalf("empty status must not carry fields: %+v", p)
	}
}

func TestLoginStatusResourcePendingCN(t *testing.T) {
	lc := &loginCtx{
		state:   "login-status-cn-state",
		variant: "cn",
		cbURL:   "http://127.0.0.1:45307/authorize",
		expires: time.Now().Add(loginTTL),
	}
	loginStates.Store(lc.state, lc)
	defer loginStates.Delete(lc.state)
	recordLoginOutcome(lc.state, false, "upstream boom")

	p := decodeLoginStatus(t)
	if !p.Pending {
		t.Fatalf("expected pending=true, got %+v", p)
	}
	if p.Variant != "cn" {
		t.Fatalf("variant = %q, want cn", p.Variant)
	}
	if p.CallbackHP != "127.0.0.1:45307" {
		t.Fatalf("callback_hostport = %q, want 127.0.0.1:45307", p.CallbackHP)
	}
	if p.TTLRemaining <= 0 || p.AgeSec < 0 {
		t.Fatalf("ttl/age out of range: ttl=%d age=%d", p.TTLRemaining, p.AgeSec)
	}
	if p.ListenerAlive {
		t.Fatalf("listener must report not-alive (none bound in test)")
	}
	if p.Restored {
		t.Fatalf("live in-memory login must not be marked restored")
	}
	if p.Outcome == nil || p.Outcome.OK || p.Outcome.Message != "upstream boom" {
		t.Fatalf("outcome missing/wrong: %+v", p.Outcome)
	}
}

func TestLoginStatusResourcePendingIntl(t *testing.T) {
	lc := &intlloginCtx{
		state:   "login-status-intl-state",
		cbURL:   "http://127.0.0.1:40001/authorize",
		expires: time.Now().Add(5 * time.Minute),
	}
	intlloginStates.Store(lc.state, lc)
	defer intlloginStates.Delete(lc.state)

	p := decodeLoginStatus(t)
	if !p.Pending || p.Variant != "intl" {
		t.Fatalf("intl pending not reported: %+v", p)
	}
	if p.CallbackHP != "127.0.0.1:40001" {
		t.Fatalf("callback_hostport = %q", p.CallbackHP)
	}
}

func TestLoginStatusResourceExpiredIgnored(t *testing.T) {
	lc := &loginCtx{
		state:   "login-status-expired-state",
		variant: "cn",
		cbURL:   "http://127.0.0.1:49999/authorize",
		expires: time.Now().Add(-time.Minute), // janitor has not reaped it yet
	}
	loginStates.Store(lc.state, lc)
	defer loginStates.Delete(lc.state)

	p := decodeLoginStatus(t)
	if p.Pending {
		t.Fatalf("expired entry must be ignored: %+v", p)
	}
}

func TestCallbackHostPort(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:45307/authorize":                                  "127.0.0.1:45307",
		"https://panel.example.com/v0/resource/plugins/trae/oauth_callback": "panel.example.com",
		"":          "",
		"not a url": "",
	}
	for in, want := range cases {
		if got := callbackHostPort(in); got != want {
			t.Fatalf("callbackHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}
