package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// management_test.go covers the /oauth_submit paste-to-complete fallback
// end-to-end with REAL blob crypto: start a login, encrypt the callback blob
// against the pending flow's static X25519 key (the same wire shape the
// platform's redirect carries), paste it, and watch the login complete —
// since v0.2.10 the paste path ALSO persists the credential straight to the
// host auth store (stubbed here), while a live poller still completes the
// dialog via the result channel.

const mimoSubmitRoute = "/v0/resource/plugins/mimo/oauth_submit"

// clearPendingLogins drops every pending login (same sweep a fresh start
// performs). Tests that assert the IDLE oauth_submit shape need this —
// earlier tests legitimately leave pending flows behind.
func clearPendingLogins(t *testing.T) {
	t.Helper()
	loginStates.Range(func(key, value any) bool {
		if lc, ok := value.(*loginCtx); ok {
			lc.shutdown()
		}
		loginStates.Delete(key)
		return true
	})
}

func startTestLogin(t *testing.T) (state string) {
	t.Helper()
	raw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatalf("handleStartLogin: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("start envelope: err=%v ok=%v", err, env.OK)
	}
	var start pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &start); err != nil {
		t.Fatalf("start result: %v", err)
	}
	if start.State == "" || start.URL == "" {
		t.Fatalf("start missing state/url: %+v", start)
	}
	// v0.2.9 passthrough: the URL IS the real platform authorize page
	// (absolute — opens from any panel origin), identical to the loginCtx's
	// stored authorizeURL the menu page links as the redundant entry.
	v, ok := loginStates.Load(start.State)
	if !ok {
		t.Fatal("pending login not registered")
	}
	lc := v.(*loginCtx)
	if start.URL != lc.authorizeURL {
		t.Fatalf("start URL = %q, want stored authorizeURL %q", start.URL, lc.authorizeURL)
	}
	if !strings.HasPrefix(start.URL, loadedPlatformURL()+"/authorize?") {
		t.Fatalf("start URL = %q, want authorize page under %q", start.URL, loadedPlatformURL())
	}
	return start.State
}

func submitPaste(t *testing.T, cbURL string) string {
	t.Helper()
	q := url.Values{}
	if cbURL != "" {
		q.Set("cb_url", cbURL)
	}
	raw, err := handleMimoManagement(testJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   mimoSubmitRoute,
		Query:  q,
	}))
	if err != nil {
		t.Fatalf("handleMimoManagement: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("submit envelope: err=%v ok=%v", err, env.OK)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("submit result: %v", err)
	}
	return string(resp.Body)
}

func testJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func gatePage(t *testing.T, state string) string {
	t.Helper()
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	raw, err := handleMimoManagement(testJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/mimo/login_gate",
		Query:  q,
	}))
	if err != nil {
		t.Fatalf("login_gate: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("gate envelope: err=%v ok=%v", err, env.OK)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("gate result: %v", err)
	}
	return string(resp.Body)
}

func TestMimoLoginGateRendersLiveFlow(t *testing.T) {
	state := startTestLogin(t)
	v, ok := loginStates.Load(state)
	if !ok {
		t.Fatal("pending login not registered")
	}
	lc := v.(*loginCtx)
	body := gatePage(t, state)
	// The live gate page links the stored authorize URL and carries the paste
	// box with the RELATIVE oauth_submit action (resolves on the CPA origin).
	if !strings.Contains(body, `href="`+html.EscapeString(lc.authorizeURL)+`"`) {
		t.Fatalf("gate page must link the stored authorize URL, got: %s", body)
	}
	for _, want := range []string{"重新打开小米授权页", `action="oauth_submit"`, `name="cb_url"`, html.EscapeString(lc.keyName)} {
		if !strings.Contains(body, want) {
			t.Fatalf("gate page missing %q, got: %s", want, body)
		}
	}
}

func TestMimoLoginGateStaleAndRotates(t *testing.T) {
	first := startTestLogin(t)
	// Missing / no state → stale notice with the paste box.
	for _, s := range []string{"", "mimo-gone"} {
		if body := gatePage(t, s); !strings.Contains(body, "不存在或已失效") || !strings.Contains(body, `name="cb_url"`) {
			t.Fatalf("state %q must render the stale notice with paste box, got: %s", s, body)
		}
	}
	// v0.2.7 single-active policy: a second start SHUTS DOWN the first flow
	// (loopback server closed) and the first gate page flips to the stale
	// notice — stale authorize tabs can never complete against an unpollled
	// flow.
	second := startTestLogin(t)
	if first == second {
		t.Fatal("states must rotate")
	}
	if _, ok := loginStates.Load(first); ok {
		t.Fatal("first flow must be dropped after re-login")
	}
	if body := gatePage(t, first); !strings.Contains(body, "不存在或已失效") {
		t.Fatalf("first gate page must flip to stale, got: %s", body)
	}
	if body := gatePage(t, second); !strings.Contains(body, "重新打开小米授权页") {
		t.Fatalf("second gate page must be live, got: %s", body)
	}
}

func TestMimoOAuthSubmitCompletesPendingLogin(t *testing.T) {
	state := startTestLogin(t)
	v, ok := loginStates.Load(state)
	if !ok {
		t.Fatal("pending login not registered")
	}
	lc := v.(*loginCtx)

	want := oauthResult{SK: "sk-paste-1234567890", UID: "20260926", URL: "https://api.xiaomimimo.com/v1"}
	blob := encryptOAuthBlob(t, mustPub(lc.privKey), want)

	// v0.2.10: stub the direct-save RPC and capture the payload (restored
	// on exit so other tests keep the real host bridge).
	var savedName string
	var savedRaw []byte
	hostAuthPersistFn = func(name string, raw []byte) error {
		savedName, savedRaw = name, raw
		return nil
	}
	defer func() { hostAuthPersistFn = hostAuthPersist }()

	// Real redirect shape (user paste 2026-09-26): the platform 302s to
	// {redirect_uri}auth?u=… — the path is /auth, not /.
	body := submitPaste(t, "http://localhost:36945/auth?u="+blob)
	if !strings.Contains(body, "登录完成") {
		t.Fatalf("paste should complete the login, got: %s", body)
	}

	// The paste path must have persisted the credential directly — the
	// canonical mimo-key-<uid>.json name both paths converge on, carrying
	// the top-level type the host needs to attribute the provider.
	if savedName != "mimo-key-"+want.UID+".json" {
		t.Fatalf("persist name = %q, want %q", savedName, "mimo-key-"+want.UID+".json")
	}
	var fileProbe struct {
		Type string `json:"type"`
		Auth struct {
			SK  string `json:"sk"`
			UID string `json:"uid"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(savedRaw, &fileProbe); err != nil {
		t.Fatalf("persisted auth JSON: %v", err)
	}
	if fileProbe.Type != providerName || fileProbe.Auth.SK != want.SK || fileProbe.Auth.UID != want.UID {
		t.Fatalf("persisted auth mismatch: type=%q sk=%q uid=%q", fileProbe.Type, fileProbe.Auth.SK, fileProbe.Auth.UID)
	}
	if !strings.Contains(body, "已直接保存") || !strings.Contains(body, html.EscapeString(savedName)) {
		t.Fatalf("page must name the saved credential file, got: %s", body)
	}
	if strings.Contains(body, "重新登录一次即可") {
		t.Fatal("page must not advise a blind re-login (it orphans the authorized key)")
	}

	// The host's next poll must now succeed and carry the decrypted payload —
	// and the poll itself performs the state cleanup.
	pollRaw, err := handlePollLogin(testJSON(t, pluginapi.AuthLoginPollRequest{State: state}))
	if err != nil {
		t.Fatalf("poll after paste: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(pollRaw, &env); err != nil || !env.OK {
		t.Fatalf("poll envelope: err=%v ok=%v", err, env.OK)
	}
	var poll pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &poll); err != nil {
		t.Fatalf("poll result: %v", err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", poll.Status)
	}
	var sa storedAuth
	if err := json.Unmarshal(poll.Auth.StorageJSON, &sa); err != nil {
		t.Fatalf("auth parse: %v", err)
	}
	if sa.Auth.SK != want.SK || sa.Auth.UID != want.UID || sa.Auth.BaseURL != want.URL {
		t.Fatalf("auth mismatch: got sk=%q uid=%q url=%q", sa.Auth.SK, sa.Auth.UID, sa.Auth.BaseURL)
	}
}

func TestMimoOAuthSubmitWrongBlobLeavesLoginPending(t *testing.T) {
	state := startTestLogin(t)
	// A blob encrypted against a DIFFERENT static key must fail the GCM tag
	// for the pending login and leave it pending.
	_, otherPriv, err := generateX25519()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	blob := encryptOAuthBlob(t, mustPub(otherPriv), oauthResult{SK: "sk-other", UID: "u-other", URL: "https://x"})
	body := submitPaste(t, "http://localhost:1/?u="+blob)
	if !strings.Contains(body, "登录未完成") {
		t.Fatalf("wrong-key blob must be rejected, got: %s", body)
	}
	_, err = handlePollLogin(testJSON(t, pluginapi.AuthLoginPollRequest{State: state}))
	if err != nil {
		t.Fatalf("poll must still be live (pending), got error: %v", err)
	}
}

func TestMimoOAuthSubmitFormAndGuards(t *testing.T) {
	clearPendingLogins(t) // earlier tests leave pending flows behind
	// No params → the idle instruction form (with the auto-refresh that
	// flips the page to the guided flow once a login starts).
	if body := submitPaste(t, ""); !strings.Contains(body, `name="cb_url"`) || !strings.Contains(body, "完成登录") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("empty paste must render the form, got: %s", body)
	}
	// Truncated copy → explicit message, not a decrypt error.
	if body := submitPaste(t, "http://localhost:1/?u=abc…"); !strings.Contains(body, "链接不完整") {
		t.Fatalf("truncated paste must say so, got: %s", body)
	}
	// Unknown route → 404 envelope.
	raw, err := handleMimoManagement(testJSON(t, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/mimo/nope"}))
	if err != nil {
		t.Fatalf("unknown route: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("unknown route envelope: err=%v ok=%v", err, env.OK)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unknown route result: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", resp.StatusCode)
	}
}

func TestMimoOAuthSubmitShowsPendingLogin(t *testing.T) {
	clearPendingLogins(t)
	// Idle → instructions + auto-refresh (picks up a login started later).
	idle := submitPaste(t, "")
	if !strings.Contains(idle, `name="cb_url"`) || !strings.Contains(idle, `http-equiv="refresh"`) {
		t.Fatalf("idle menu page must render the form with auto-refresh, got: %s", idle)
	}
	// A pending login turns the SAME page into the guided flow — the
	// cross-origin-safe entry (the panel's OAuth dialog link breaks on
	// hosted origins; the menu page iframe is apiBase-prefixed and works).
	state := startTestLogin(t)
	v, ok := loginStates.Load(state)
	if !ok {
		t.Fatal("pending login not registered")
	}
	lc := v.(*loginCtx)
	body := submitPaste(t, "")
	if !strings.Contains(body, "重新打开小米授权页") || !strings.Contains(body, html.EscapeString(lc.authorizeURL)) {
		t.Fatalf("menu page must render the live flow, got: %s", body)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("live flow page must not auto-refresh")
	}
	for _, want := range []string{`action="oauth_submit"`, `name="cb_url"`, html.EscapeString(lc.keyName)} {
		if !strings.Contains(body, want) {
			t.Fatalf("live flow page missing %q, got: %s", want, body)
		}
	}
}

func TestMimoExtractBlobForms(t *testing.T) {
	const blob = "AbCdEf0123456789-_"
	inputs := []string{
		"http://localhost:51000/?u=" + blob,
		"http://localhost:51000/?x=1&u=" + blob,
		"http://localhost:36945/auth?u=" + blob, // real 302 shape (2026-09-26 paste): path is /auth
		"localhost:36945/auth?u=" + blob,        // scheme-less real shape
		"localhost:51000/?u=" + blob,            // scheme-less
		"?u=" + blob,                            // bare query
		"u=" + blob,                             // bare pair
		blob,                                    // bare blob
		"  \"" + blob + "\"  ",                  // quoted IM paste
	}
	for _, in := range inputs {
		got, trunc := mimoExtractBlob(in)
		if trunc != "" || got != blob {
			t.Errorf("mimoExtractBlob(%q) = %q (trunc=%q), want %q", in, got, trunc, blob)
		}
	}
	// A padded bare blob keeps its padding — decodeBase64URL accepts both
	// forms, and the pad is part of the pasted value.
	padded := blob + "="
	if got, trunc := mimoExtractBlob(padded); trunc != "" || got != padded {
		t.Errorf("mimoExtractBlob(%q) = %q (trunc=%q), want %q", padded, got, trunc, padded)
	}
}
