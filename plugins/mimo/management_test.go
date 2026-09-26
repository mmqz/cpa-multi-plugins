package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// management_test.go covers the /oauth_submit paste-to-complete fallback
// end-to-end with REAL blob crypto: start a login, encrypt the callback blob
// against the pending flow's static X25519 key (the same wire shape the
// platform's redirect carries), paste it, and watch the host's next poll
// complete the login.

const mimoSubmitRoute = "/v0/resource/plugins/mimo/oauth_submit"

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

func TestMimoOAuthSubmitCompletesPendingLogin(t *testing.T) {
	state := startTestLogin(t)
	v, ok := loginStates.Load(state)
	if !ok {
		t.Fatal("pending login not registered")
	}
	lc := v.(*loginCtx)

	want := oauthResult{SK: "sk-paste-1234567890", UID: "20260926", URL: "https://api.xiaomimimo.com/v1"}
	blob := encryptOAuthBlob(t, mustPub(lc.privKey), want)

	body := submitPaste(t, "http://localhost:51999/?u="+blob)
	if !strings.Contains(body, "登录完成") {
		t.Fatalf("paste should complete the login, got: %s", body)
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
	// No params → the instruction form.
	if body := submitPaste(t, ""); !strings.Contains(body, `name="cb_url"`) || !strings.Contains(body, "完成登录") {
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

func TestMimoExtractBlobForms(t *testing.T) {
	const blob = "AbCdEf0123456789-_"
	inputs := []string{
		"http://localhost:51000/?u=" + blob,
		"http://localhost:51000/?x=1&u=" + blob,
		"localhost:51000/?u=" + blob, // scheme-less
		"?u=" + blob,                 // bare query
		"u=" + blob,                  // bare pair
		blob,                         // bare blob
		"  \"" + blob + "\"  ",       // quoted IM paste
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
