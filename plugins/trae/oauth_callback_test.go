// oauth_callback_test.go: v0.12.4 regression tests — host-aware OAuth
// callback plumbing, wire-format handling, and variant-aware auth records.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseLoginHostContext(t *testing.T) {
	req := []byte(`{"Provider":"trae","BaseURL":"http://127.0.0.1:8317/v0/management/oauth-callback","Host":{"AuthDir":"/data/auths"}}`)
	ctx := parseLoginHostContext(req)
	if ctx.BaseURL != "http://127.0.0.1:8317/v0/management/oauth-callback" {
		t.Fatalf("BaseURL = %q", ctx.BaseURL)
	}
	if ctx.AuthDir != "/data/auths" {
		t.Fatalf("AuthDir = %q", ctx.AuthDir)
	}
	if ctx := parseLoginHostContext(nil); ctx.BaseURL != "" || ctx.AuthDir != "" {
		t.Fatalf("nil payload: %+v", ctx)
	}
}

func TestResourceCallbackURL(t *testing.T) {
	got := resourceCallbackURL("http://127.0.0.1:8317/v0/management/oauth-callback")
	want := "http://127.0.0.1:8317" + resourceCallbackPath
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, bad := range []string{"", "   ", "::::", "/just/a/path"} {
		if got := resourceCallbackURL(bad); got != "" {
			t.Fatalf("resourceCallbackURL(%q) = %q, want empty", bad, got)
		}
	}
	if !strings.HasPrefix(resourceCallbackPath, "/v0/resource/plugins/trae/") {
		t.Fatalf("resourceCallbackPath = %q", resourceCallbackPath)
	}
}

func TestSniffVariantIntlNestedMarkers(t *testing.T) {
	// The merged plugin's own intl login writes nested markers WITHOUT an
	// explicit variant — it must sniff as intl, not cn.
	nested := []byte(`{"type":"trae","provider":"trae","auth":{"accessToken":"a","tenant":"marscode","scope":"marscode-us"},"account":{"uid":"1"}}`)
	if got := sniffVariantFromJSON(nested); got != variantIntl {
		t.Fatalf("nested tenant/scope sniffed as %q, want intl", got)
	}
	nestedWebID := []byte(`{"auth":{"webId":"w1","userIdentity":"Free"}}`)
	if got := sniffVariantFromJSON(nestedWebID); got != variantIntl {
		t.Fatalf("nested webId sniffed as %q, want intl", got)
	}
	// Explicit variant always wins.
	explicitSolo := []byte(`{"auth":{"variant":"solo","tenant":"marscode"}}`)
	if got := sniffVariantFromJSON(explicitSolo); got != variantSolo {
		t.Fatalf("explicit solo sniffed as %q, want solo", got)
	}
	// Plain CN file stays CN.
	cn := []byte(`{"auth":{"accessToken":"a","machineId":"m","deviceId":"d"}}`)
	if got := sniffVariantFromJSON(cn); got != variantCN {
		t.Fatalf("cn sniffed as %q, want cn", got)
	}
	// Legacy top-level markers (old trae-intl files) still recognized.
	legacy := []byte(`{"webId":"w","tenant":"t","userIdentity":"u"}`)
	if got := sniffVariantFromJSON(legacy); got != variantIntl {
		t.Fatalf("legacy intl sniffed as %q, want intl", got)
	}
}

func TestRequestVariantIsIntlWireFormats(t *testing.T) {
	intlJSON := []byte(`{"auth":{"tenant":"marscode","scope":"marscode-us"},"account":{"uid":"1"}}`)
	b64 := base64.StdEncoding.EncodeToString(intlJSON)

	wire := []byte(`{"Plugin":{"Name":"trae"},"AuthID":"x","AuthProvider":"trae","StorageJSON":"` + b64 + `"}`)
	if !requestVariantIsIntl(wire) {
		t.Fatalf("model.for_auth base64 wire not recognized as intl")
	}
	parseWire := []byte(`{"Provider":"trae","FileName":"f.json","RawJSON":"` + b64 + `"}`)
	if !requestVariantIsIntl(parseWire) {
		t.Fatalf("auth.parse base64 wire not recognized as intl")
	}
	cnJSON := []byte(`{"auth":{"accessToken":"a","machineId":"m"}}`)
	cnB64 := base64.StdEncoding.EncodeToString(cnJSON)
	cnWire := []byte(`{"StorageJSON":"` + cnB64 + `"}`)
	if requestVariantIsIntl(cnWire) {
		t.Fatalf("cn wire misdetected as intl")
	}
	if !requestVariantIsIntl([]byte(`{"storage_json":` + string(intlJSON) + `}`)) {
		t.Fatalf("raw storage_json envelope not recognized")
	}
	if requestVariantIsIntl([]byte("garbage")) {
		t.Fatalf("garbage detected as intl")
	}
}

func TestHandleParseAuthWireBase64(t *testing.T) {
	authJSON := []byte(`{"type":"trae","provider":"trae","auth":{"accessToken":"tk","refreshToken":"rk","expiresAt":9999999999,"domain":"trae.cn","variant":"solo"},"account":{"uid":"u-1","nickname":"Nick"}}`)
	wire := []byte(`{"Provider":"trae","Path":"/x/trae-u-1.json","FileName":"trae-u-1.json","RawJSON":"` + base64.StdEncoding.EncodeToString(authJSON) + `"}`)
	resp, err := handleParseAuth(wire)
	if err != nil {
		t.Fatalf("parse wire: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"handled"`
			Auth    struct {
				ID          string `json:"id"`
				Label       string `json:"label"`
				StorageJSON string `json:"StorageJSON"`
			} `json:"auth"`
		} `json:"result"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Error != nil {
		t.Fatalf("parse error envelope: %+v", env.Error)
	}
	if !env.OK || !env.Result.Handled {
		t.Fatalf("parse result: %+v", env)
	}
	// v0.12.8: parse must leave ID empty — the host derives the record ID
	// from the file path so import and watcher registrations share one key.
	if env.Result.Auth.ID != "" {
		t.Fatalf("parse Auth.ID = %q, want empty (host derives from path)", env.Result.Auth.ID)
	}
	if env.Result.Auth.Label != "Nick" {
		t.Fatalf("label = %q, want nickname", env.Result.Auth.Label)
	}
	// AuthData.StorageJSON is []byte: the wire base64-encodes it once, and
	// the host decodes it back into the REAL auth JSON. Decoding here must
	// yield the auth object (the broken version double-encoded — the wire
	// form was base64 of a base64 STRING).
	decodedStorage, errD := base64.StdEncoding.DecodeString(env.Result.Auth.StorageJSON)
	if errD != nil {
		t.Fatalf("storage_json is not valid base64: %v", errD)
	}
	if !strings.Contains(string(decodedStorage), "variant") || !strings.HasPrefix(string(decodedStorage), "{") {
		t.Fatalf("storage_json not the auth object: %q", decodedStorage)
	}
}

func TestHandleOAuthCallbackResourceCNSolo(t *testing.T) {
	state := "test-state-cn-1234"
	lc := &loginCtx{variant: variantSolo, state: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)

	q := url.Values{}
	q.Set("state", state)
	q.Set("authCode", "ac-xyz")
	q.Set("loginHost", "www.trae.cn")
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("callback body = %s", body)
	}
	if lc.authCode != "ac-xyz" || lc.loginHost != "www.trae.cn" {
		t.Fatalf("loginCtx not completed: %+v", lc)
	}
	select {
	case <-lc.done:
	default:
		t.Fatalf("lc.done not closed after callback")
	}
}

func TestHandleOAuthCallbackResourceIntl(t *testing.T) {
	state := "test-state-intl-5678"
	lc := &intlloginCtx{state: state, expires: time.Now().Add(time.Minute)}
	intlloginStates.Store(state, lc)
	defer intlloginStates.Delete(state)

	q := url.Values{}
	q.Set("state", state)
	q.Set("authCode", "ac-intl")
	q.Set("loginHost", "www.trae.ai")
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("callback body = %s", body)
	}
	if lc.authCode != "ac-intl" || lc.loginHost != "www.trae.ai" {
		t.Fatalf("intl ctx fields: %+v", lc)
	}
	// The intl poll handler is auth-code only: a refreshToken-only callback
	// must surface as a failure, not a hang.
	state2 := "test-state-intl-rt"
	lc2 := &intlloginCtx{state: state2, expires: time.Now().Add(time.Minute)}
	intlloginStates.Store(state2, lc2)
	defer intlloginStates.Delete(state2)
	q2 := url.Values{}
	q2.Set("state", state2)
	q2.Set("refreshToken", "rt-xyz")
	body2 := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q2}))
	if !strings.Contains(body2, "Login failed") || !strings.Contains(body2, "no authCode") {
		t.Fatalf("intl refresh-only body = %s", body2)
	}
	select {
	case <-lc.done:
	default:
		t.Fatalf("lc.done not closed after callback")
	}
}

func TestHandleOAuthCallbackResourceErrors(t *testing.T) {
	// v0.12.12: a bare hit with NO query at all answers pending and keeps
	// waiting (cockpit-tools keeps waiting on an empty query) — it must
	// NOT fail the flow with "missing state" anymore.
	body := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET"}))
	if !strings.Contains(body, "Waiting for authorization") {
		t.Fatalf("empty-query body = %s", body)
	}
	// Params present but no state / trace id and zero live logins →
	// still a missing-state failure.
	q := url.Values{}
	q.Set("authCode", "ac-orphan")
	body = string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q}))
	if !strings.Contains(body, "Login failed") || !strings.Contains(body, "missing state") {
		t.Fatalf("param-only body = %s", body)
	}
	q = url.Values{}
	q.Set("state", "no-such-state")
	body = string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q}))
	if !strings.Contains(body, "unknown or expired") {
		t.Fatalf("unknown-state body = %s", body)
	}
	state := "test-state-err-9999"
	lc := &loginCtx{state: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)
	q = url.Values{}
	q.Set("state", state)
	q.Set("error", "access_denied")
	body = string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q}))
	if !strings.Contains(body, "Login failed") || !strings.Contains(body, "access_denied") {
		t.Fatalf("error-param body = %s", body)
	}
	if lc.err == nil || !strings.Contains(lc.err.Error(), "access_denied") {
		t.Fatalf("lc.err = %v", lc.err)
	}
}

func TestCompleteLoginIdempotent(t *testing.T) {
	lc := &loginCtx{}
	completeLogin(lc)
	first := lc.done
	completeLogin(lc)
	if lc.done != first {
		t.Fatalf("completeLogin re-created done channel")
	}
	select {
	case <-lc.done:
	default:
		t.Fatalf("done not closed")
	}
	closeListener(nil)
}

func TestResolveCallbackStatePriority(t *testing.T) {
	// 1. state wins over any trace id.
	q := url.Values{}
	q.Set("state", "s-1")
	q.Set("login_trace_id", "t-1")
	if got := resolveCallbackState(q); got != "s-1" {
		t.Fatalf("state priority: got %q", got)
	}
	// 2. trace id variants resolve.
	for _, k := range []string{"loginTraceID", "loginTraceId", "login_trace_id", "trace_id"} {
		q := url.Values{}
		q.Set(k, "trace-x")
		if got := resolveCallbackState(q); got != "trace-x" {
			t.Fatalf("trace variant %s: got %q", k, got)
		}
	}
	// 3. nothing to go on and no live logins → empty.
	if got := resolveCallbackState(url.Values{}); got != "" {
		t.Fatalf("no hints: got %q, want empty", got)
	}
}

func TestHandleOAuthCallbackResourceLoginTraceIDEcho(t *testing.T) {
	// Real Trae redirect shape (v0.12.12): NO "state" param — the plugin
	// never sends one. The authorization page echoes login_trace_id plus
	// its own params (isRedirect=true, authCode, loginHost).
	state := newLoginTraceID()
	lc := &loginCtx{variant: variantCN, state: state, loginHost: "www.trae.cn", expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)

	q := url.Values{}
	q.Set("login_trace_id", state)
	q.Set("isRedirect", "true")
	q.Set("authCode", "ac-real")
	q.Set("loginHost", "api.trae.cn")
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("real-redirect callback body = %s", body)
	}
	if lc.authCode != "ac-real" {
		t.Fatalf("authCode = %q", lc.authCode)
	}
	if lc.loginHost != "api.trae.cn" {
		t.Fatalf("loginHost = %q, want callback value", lc.loginHost)
	}
	select {
	case <-lc.done:
	default:
		t.Fatalf("lc.done not closed after real-redirect callback")
	}
}

func TestHandleOAuthCallbackResourceKeepsFallbackLoginHost(t *testing.T) {
	// Callback WITHOUT loginHost must keep the start-login host
	// (cockpit-tools fallback_login_host), not blank it out.
	state := newLoginTraceID()
	lc := &loginCtx{variant: variantCN, state: state, loginHost: "www.trae.cn", expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)
	q := url.Values{}
	q.Set("login_trace_id", state)
	q.Set("authCode", "ac-nohost")
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("body = %s", body)
	}
	if lc.loginHost != "www.trae.cn" {
		t.Fatalf("loginHost overwritten to %q, want start-login value kept", lc.loginHost)
	}
}

func TestHandleOAuthCallbackResourceSingleInflightFallback(t *testing.T) {
	// No state, no trace id — exactly one live login resolves it
	// (cockpit-tools port-scoped uniqueness semantics).
	state := newLoginTraceID()
	lc := &intlloginCtx{state: state, loginHost: "www.trae.ai", expires: time.Now().Add(time.Minute)}
	intlloginStates.Store(state, lc)
	defer intlloginStates.Delete(state)
	q := url.Values{}
	q.Set("authCode", "ac-single")
	q.Set("loginHost", "www.trae.ai")
	body := handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("single-inflight body = %s", body)
	}
	if lc.authCode != "ac-single" {
		t.Fatalf("intl authCode = %q", lc.authCode)
	}
}

func TestHandleOAuthCallbackResourceAmbiguousInflight(t *testing.T) {
	// Two live logins and no identifying param → error page; NEITHER
	// flow may complete.
	s1, s2 := newLoginTraceID(), newLoginTraceID()
	lc1 := &loginCtx{state: s1, expires: time.Now().Add(time.Minute)}
	lc2 := &intlloginCtx{state: s2, expires: time.Now().Add(time.Minute)}
	loginStates.Store(s1, lc1)
	intlloginStates.Store(s2, lc2)
	defer loginStates.Delete(s1)
	defer intlloginStates.Delete(s2)
	q := url.Values{}
	q.Set("authCode", "ac-ambiguous")
	body := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q}))
	if !strings.Contains(body, "Login failed") {
		t.Fatalf("ambiguous body = %s", body)
	}
	select {
	case <-lc1.done:
		t.Fatalf("cn flow completed on ambiguous callback")
	default:
	}
	select {
	case <-lc2.done:
		t.Fatalf("intl flow completed on ambiguous callback")
	default:
	}
}

func TestHandleOAuthCallbackResourceEmptyQueryPending(t *testing.T) {
	// Bare callback hit (no query at all — browser prefetch / manual
	// paste): pending answer, in-flight login NOT failed.
	state := newLoginTraceID()
	lc := &loginCtx{state: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)
	body := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: nil}))
	if !strings.Contains(body, "Waiting for authorization") {
		t.Fatalf("empty-query body = %s", body)
	}
	if lc.err != nil {
		t.Fatalf("empty query must not fail the login: %v", lc.err)
	}
	select {
	case <-lc.done:
		t.Fatalf("empty query must not complete the login")
	default:
	}
}

func TestHandleOAuthCallbackResourceExpiredStateExcluded(t *testing.T) {
	// An EXPIRED login must not satisfy the single-inflight fallback.
	state := newLoginTraceID()
	lc := &loginCtx{state: state, expires: time.Now().Add(-time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)
	q := url.Values{}
	q.Set("authCode", "ac-expired")
	body := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET", Query: q}))
	if !strings.Contains(body, "Login failed") || !strings.Contains(body, "missing state") {
		t.Fatalf("expired-only fallback body = %s", body)
	}
}

func TestNewDeviceIDShape(t *testing.T) {
	// Upstream validates device ids as numeric strings of 8-24 digits
	// (cockpit-tools is_numeric_id(8, 24)). The old randomHex(16) was
	// 32 hex chars with letters — out of spec.
	for i := 0; i < 200; i++ {
		d := newDeviceID()
		if n := len(d); n < 8 || n > 24 {
			t.Fatalf("device id length %d out of 8..24: %q", n, d)
		}
		for _, r := range d {
			if r < '0' || r > '9' {
				t.Fatalf("device id contains non-digit %q: %q", r, d)
			}
		}
	}
}

func TestNewMachineIDUUIDv4(t *testing.T) {
	m := newMachineID()
	if len(m) != 36 || m[8] != '-' || m[13] != '-' || m[18] != '-' || m[23] != '-' {
		t.Fatalf("machine id not UUID-shaped: %q", m)
	}
	if m[14] != '4' {
		t.Fatalf("machine id not version 4: %q", m)
	}
	// RFC 4122 variant: high bits of the 4th group are 10xx.
	v := m[19]
	if v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("machine id variant bits invalid: %q", m)
	}
	if m == newMachineID() {
		t.Fatalf("machine id not random")
	}
}

func TestNewDeviceIDUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		d := newDeviceID()
		if seen[d] {
			t.Fatalf("device id collision: %q", d)
		}
		seen[d] = true
	}
}

func TestReadHostCallbackFile(t *testing.T) {
	if _, _, ok := readHostCallbackFile("", "s"); ok {
		t.Fatalf("empty authDir returned ok")
	}
	if _, _, ok := readHostCallbackFile(t.TempDir(), "state-x"); ok {
		t.Fatalf("missing file returned ok")
	}
}
