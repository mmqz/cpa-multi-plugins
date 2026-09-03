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

// -----------------------------------------------------------------------------
// v0.12.23: grace-based self-completion for LIVE (non-restored) logins.
// The reported failure "提交了回调链接仍然不生成凭证" traced to the live
// login path: the paste only captured the authCode in memory and the
// credential was generated exclusively by the host's auth.login.poll — which
// never arrives once the CPA login dialog is closed. v0.12.23 arms a
// one-shot self-completion for every accepted callback: restored logins
// finish immediately (v0.12.17 behavior), live logins wait
// loginSelfCompleteGrace for the host poll to claim the state first and
// claim exclusive ownership via LoadAndDelete before finishing in-process.
// -----------------------------------------------------------------------------

func waitForGraceCompletion(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grace self-completion did not settle within 3s")
}

// Live login whose captured callback is never claimed by a host poll: after
// the grace window the plugin must claim the state, run the self-completion
// (which fails fast and cleanly on an empty token capture) and surface the
// failure through the outcome cache instead of stalling silently.
func TestLiveLoginSelfCompletesAfterGrace(t *testing.T) {
	prevGrace := loginSelfCompleteGrace
	loginSelfCompleteGrace = 80 * time.Millisecond
	t.Cleanup(func() { loginSelfCompleteGrace = prevGrace })

	dir := t.TempDir()
	state := "grace-live-cn-1"
	lc := &loginCtx{
		variant: variantCN, state: state, loginTraceID: state,
		expires: time.Now().Add(time.Minute), authDir: dir,
	}
	loginStates.Store(state, lc)
	spawnSelfCompleteCN(lc)

	waitForGraceCompletion(t, func() bool {
		_, live := loginStates.Load(state)
		return !live
	})
	o, ok := lookupLoginOutcome(state)
	if !ok || o.ok {
		t.Fatalf("outcome not recorded as failure: %+v (ok=%v)", o, ok)
	}
	if !strings.Contains(o.msg, "no authCode/refreshToken") {
		t.Fatalf("unexpected outcome message: %q", o.msg)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no credential file expected on the fail-fast path, found %d entries", len(entries))
	}
	t.Cleanup(func() {
		outcomeCache.Lock()
		delete(outcomeCache.m, state)
		outcomeCache.Unlock()
	})
}

// Live login consumed by the host poll DURING the grace window: the
// self-completion goroutine must lose the LoadAndDelete claim, do nothing,
// and leave the poll's outcome untouched.
func TestLiveLoginGraceYieldsToHostPoll(t *testing.T) {
	prevGrace := loginSelfCompleteGrace
	loginSelfCompleteGrace = 250 * time.Millisecond
	t.Cleanup(func() { loginSelfCompleteGrace = prevGrace })

	state := "grace-yield-cn-1"
	lc := &loginCtx{
		variant: variantCN, state: state, loginTraceID: state,
		expires: time.Now().Add(time.Minute),
	}
	loginStates.Store(state, lc)

	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(realFormRedirectURL(state)) + `}`),
	})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("paste rejected: %s", body)
	}
	// Host poll drains the state before the grace elapses.
	recordLoginOutcome(state, true, "")
	clearPendingLogin("")
	loginStates.Delete(state)
	time.Sleep(loginSelfCompleteGrace + 200*time.Millisecond)

	o, ok := lookupLoginOutcome(state)
	if !ok || !o.ok {
		t.Fatalf("poll outcome overwritten by self-completion: %+v (ok=%v)", o, ok)
	}
	if _, live := loginStates.Load(state); live {
		t.Fatalf("state resurrected after poll completion")
	}
	t.Cleanup(func() {
		outcomeCache.Lock()
		delete(outcomeCache.m, state)
		outcomeCache.Unlock()
	})
}

// Intl mirror of the live grace test.
func TestLiveLoginSelfCompletesAfterGraceIntl(t *testing.T) {
	prevGrace := loginSelfCompleteGrace
	loginSelfCompleteGrace = 80 * time.Millisecond
	t.Cleanup(func() { loginSelfCompleteGrace = prevGrace })

	state := "grace-live-intl-1"
	lc := &intlloginCtx{
		state: state, loginTraceID: state,
		expires: time.Now().Add(time.Minute),
	}
	intlloginStates.Store(state, lc)
	spawnSelfCompleteIntl(lc)

	waitForGraceCompletion(t, func() bool {
		_, live := intlloginStates.Load(state)
		return !live
	})
	o, ok := lookupLoginOutcome(state)
	if !ok || o.ok || !strings.Contains(o.msg, "no authCode") {
		t.Fatalf("intl outcome wrong: %+v (ok=%v)", o, ok)
	}
	t.Cleanup(func() {
		outcomeCache.Lock()
		delete(outcomeCache.m, state)
		outcomeCache.Unlock()
	})
}
