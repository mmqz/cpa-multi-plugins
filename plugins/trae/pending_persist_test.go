package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// v0.12.17: disk-persisted pending login + completed-outcome cache + paste
// sanitization. The user-visible failure "回调未完成登录" traced to three
// real branches: stale loginTraceID after a process bounce / re-login,
// double-paste after the panel poll drained the state, and truncated copies
// of the address-bar URL. These tests pin all three.

func setAuthDirCacheForTest(t *testing.T, dir string) {
	t.Helper()
	authDirCache.Lock()
	prev := authDirCache.dir
	authDirCache.dir = dir
	authDirCache.Unlock()
	t.Cleanup(func() {
		authDirCache.Lock()
		authDirCache.dir = prev
		authDirCache.Unlock()
	})
}

func testPendingRecord(state string) pendingLoginRecord {
	return pendingLoginRecord{
		Flow: "cn", State: state, Variant: variantCN,
		CbURL:        "http://127.0.0.1:41890/authorize",
		CodeVerifier: "verifier-" + state, CodeChallenge: "challenge-" + state,
		DeviceID: "1234567890123456", MachineID: "0f0e1d2c-3b4a-5968-7c8d-9e0f1a2b3c4d",
		AuthDir:   "", // filled by caller
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}
}

func TestPendingPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	rec := testPendingRecord("persist-rt-1")
	rec.AuthDir = dir
	persistPendingLogin(rec)
	got, ok := loadPendingLogin(dir)
	if !ok {
		t.Fatalf("load failed after persist")
	}
	if got.State != rec.State || got.CodeVerifier != rec.CodeVerifier || got.Flow != rec.Flow {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, rec)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingLoginFileName)); err != nil {
		t.Fatalf("file missing: %v", err)
	}
	clearPendingLogin(dir)
	if _, ok := loadPendingLogin(dir); ok {
		t.Fatalf("load succeeded after clear")
	}
}

func TestPendingPersistTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	rec := testPendingRecord("persist-ttl-1")
	rec.AuthDir = dir
	rec.ExpiresAt = time.Now().Add(-time.Minute).Unix() // expired
	persistPendingLogin(rec)
	if _, ok := loadPendingLogin(dir); ok {
		t.Fatalf("expired record must not load")
	}
	if _, err := os.Stat(filepath.Join(dir, pendingLoginFileName)); !os.IsNotExist(err) {
		t.Fatalf("expired record must be removed on load")
	}
}

func TestRestorePendingLoginState(t *testing.T) {
	dir := t.TempDir()
	setAuthDirCacheForTest(t, dir)

	// cn flow
	rec := testPendingRecord("restore-cn-1")
	rec.AuthDir = dir
	persistPendingLogin(rec)
	if got := restorePendingLoginState("restore-cn-1"); got != "restore-cn-1" {
		t.Fatalf("cn restore = %q", got)
	}
	v, ok := loginStates.Load("restore-cn-1")
	if !ok {
		t.Fatalf("cn state not materialized")
	}
	lc := v.(*loginCtx)
	if lc.codeVerifier != rec.CodeVerifier || lc.variant != variantCN || lc.listener != nil {
		t.Fatalf("cn rebuilt ctx wrong: %+v", lc)
	}
	if !stateIsLive("restore-cn-1") {
		t.Fatalf("stateIsLive false after restore")
	}
	loginStates.Delete("restore-cn-1")

	// intl flow
	irec := testPendingRecord("restore-intl-1")
	irec.Flow = "intl"
	irec.AuthDir = dir
	persistPendingLogin(irec)
	if got := restorePendingLoginState("restore-intl-1"); got != "restore-intl-1" {
		t.Fatalf("intl restore = %q", got)
	}
	iv, ok := intlloginStates.Load("restore-intl-1")
	if !ok {
		t.Fatalf("intl state not materialized")
	}
	if ilc := iv.(*intlloginCtx); ilc.codeVerifier != irec.CodeVerifier || ilc.listener != nil {
		t.Fatalf("intl rebuilt ctx wrong: %+v", ilc)
	}
	intlloginStates.Delete("restore-intl-1")
}

func TestRestoreRefusesCrossState(t *testing.T) {
	dir := t.TempDir()
	setAuthDirCacheForTest(t, dir)
	rec := testPendingRecord("restore-own-1")
	rec.AuthDir = dir
	persistPendingLogin(rec)
	if got := restorePendingLoginState("a-different-state"); got != "" {
		t.Fatalf("cross-state restore must be refused, got %q", got)
	}
	// empty state adopts the single record (bare URL paste after restart)
	if got := restorePendingLoginState(""); got != "restore-own-1" {
		t.Fatalf("empty-state adoption = %q", got)
	}
	loginStates.Delete("restore-own-1")
}

func TestSubmitAfterRestartRestoresPending(t *testing.T) {
	dir := t.TempDir()
	setAuthDirCacheForTest(t, dir)
	state := "restart-e2e-1"

	// Simulate: start-login persisted the record, then the process bounced
	// (loginStates map is empty — nothing stored in memory).
	rec := testPendingRecord(state)
	rec.AuthDir = dir
	persistPendingLogin(rec)

	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(realFormRedirectURL(state)) + `}`),
	})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("post-restart paste failed: %s", body)
	}
	v, ok := loginStates.Load(state)
	if !ok {
		t.Fatalf("state not live after restore")
	}
	if lc := v.(*loginCtx); lc.authCode != "REAL-FORM-CODE-1" {
		t.Fatalf("restored ctx authCode: %+v", lc)
	}
	loginStates.Delete(state)
}

func TestSubmitDoublePasteOutcomeCache(t *testing.T) {
	state := "double-paste-1"
	lc := &loginCtx{variant: variantCN, state: state, loginTraceID: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer func() {
		loginStates.Delete(state)
	}()

	first := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(realFormRedirectURL(state)) + `}`),
	})
	if !strings.Contains(string(first), "Login successful") {
		t.Fatalf("first paste: %s", first)
	}
	// Panel poll drains the state (poll success records the outcome + deletes).
	recordLoginOutcome(state, true, "")
	clearPendingLogin("")
	loginStates.Delete(state)

	second := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(realFormRedirectURL(state)) + `}`),
	})
	if !strings.Contains(string(second), "Login successful") {
		t.Fatalf("re-paste after completion must answer already-completed: %s", second)
	}
	if !strings.Contains(string(second), "already completed") {
		t.Fatalf("re-paste page lacks completed note: %s", second)
	}
}

func TestSubmitOutcomeErrorPath(t *testing.T) {
	state := "outcome-err-1"
	recordLoginOutcome(state, false, "ExchangeToken failed: HTTP 400 (code expired)")
	defer func() {
		outcomeCache.Lock()
		delete(outcomeCache.m, state)
		outcomeCache.Unlock()
	}()
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{
		Query: url.Values{"loginTraceID": {state}, "isRedirect": {"true"}},
	})
	if !strings.Contains(string(body), "Login failed") || !strings.Contains(string(body), "ExchangeToken failed") {
		t.Fatalf("outcome error page wrong: %s", body)
	}
}

func TestSubmitTruncatedURL(t *testing.T) {
	q := url.Values{}
	q.Set("cb_url", "http://127.0.0.1:41961/authorize?isRedirect=true&authCodeInfo=%7B%22AuthCode%22%3A%22WRUotgbKzjMIfdVV…")
	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "链接不完整") || !strings.Contains(string(body), "TRUNCATED") {
		t.Fatalf("truncated link message wrong: %s", body)
	}
}

func TestSubmitBareQueryPaste(t *testing.T) {
	state := "bare-query-1"
	lc := &loginCtx{variant: variantCN, state: state, loginTraceID: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)

	// User copied only the query part (after ?) — must still complete.
	raw := strings.SplitN(realFormRedirectURL(state), "?", 2)[1]
	q := url.Values{}
	q.Set("cb_url", raw)
	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("bare query paste: %s", body)
	}
	if lc.authCode != "REAL-FORM-CODE-1" {
		t.Fatalf("bare query authCode: %+v", lc)
	}
}

func TestSanitizePastedCallback(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantTrunc      bool
	}{
		{"plain", "http://127.0.0.1:41961/authorize?a=1", "http://127.0.0.1:41961/authorize?a=1", false},
		{"quoted", `"http://127.0.0.1:41961/authorize?a=1"`, "http://127.0.0.1:41961/authorize?a=1", false},
		{"cjk-quoted", "《http://127.0.0.1:41961/authorize?a=1》", "http://127.0.0.1:41961/authorize?a=1", false},
		{"scheme-less-host", "127.0.0.1:41961/authorize?a=1", "http://127.0.0.1:41961/authorize?a=1", false},
		{"bare-query", "isRedirect=true&authCodeInfo=x", "http://127.0.0.1/authorize?isRedirect=true&authCodeInfo=x", false},
		{"leading-q", "?isRedirect=true", "http://127.0.0.1/authorize?isRedirect=true", false},
		{"ellipsis", "http://127.0.0.1:1/authorize?a=WRU…", "", true},
		{"three-dots", "http://127.0.0.1:1/authorize?a=WRU...x", "", true},
	}
	for _, c := range cases {
		got, trunc := sanitizePastedCallback(c.in)
		if c.wantTrunc {
			if trunc == "" {
				t.Fatalf("%s: expected truncation error", c.name)
			}
			continue
		}
		if trunc != "" {
			t.Fatalf("%s: unexpected truncation error %q", c.name, trunc)
		}
		if got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCaptureAuthDirProbe(t *testing.T) {
	dir := t.TempDir()
	setAuthDirCacheForTest(t, "")
	payload, _ := json.Marshal(map[string]any{
		"Provider": "trae",
		"Host":     map[string]any{"AuthDir": dir, "ProxyURL": ""},
	})
	captureAuthDir(payload)
	if got := cachedAuthDir(); got != dir {
		t.Fatalf("captureAuthDir: got %q want %q", got, dir)
	}
	// empty payload must not clobber
	captureAuthDir(nil)
	if got := cachedAuthDir(); got != dir {
		t.Fatalf("nil payload clobbered cache: %q", got)
	}
}
