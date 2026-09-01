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
	if !env.OK || !env.Result.Handled || env.Result.Auth.ID != "u-1" {
		t.Fatalf("parse result: %+v", env)
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
	body := string(handleOAuthCallbackResource(pluginapi.ManagementRequest{Method: "GET"}))
	if !strings.Contains(body, "Login failed") || !strings.Contains(body, "missing state") {
		t.Fatalf("missing-state body = %s", body)
	}
	q := url.Values{}
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

func TestReadHostCallbackFile(t *testing.T) {
	if _, _, ok := readHostCallbackFile("", "s"); ok {
		t.Fatalf("empty authDir returned ok")
	}
	if _, _, ok := readHostCallbackFile(t.TempDir(), "state-x"); ok {
		t.Fatalf("missing file returned ok")
	}
}
