// plugin_test.go covers the mimo plugin's wire-critical pieces: the OAuth
// crypto round-trip (byte-format parity with the CLI), the desktop-fingerprint
// headers, the cookie jar matcher, model alias resolution, privacy stripping,
// the me-probe parser, the 401 renew ladder, and the SSE frame guards.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// encryptOAuthBlob emulates the platform's callback blob exactly as the CLI
// documents it (mimo.ts decrypt): ephemeralPub(32) ‖ nonce(12) ‖ ct ‖ tag(16),
// AES-256-GCM keyed by SHA256(ECDH(ephemeralPriv, staticPub)).
func encryptOAuthBlob(t *testing.T, staticPubRaw []byte, payload oauthResult) string {
	t.Helper()
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ephemeral key: %v", err)
	}
	staticPub, err := ecdh.X25519().NewPublicKey(staticPubRaw)
	if err != nil {
		t.Fatalf("static pub: %v", err)
	}
	shared, err := eph.ECDH(staticPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	key := sha256.Sum256(shared)
	plain, _ := json.Marshal(payload)
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	sealed := gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, 32+12+len(sealed))
	out = append(out, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.RawURLEncoding.EncodeToString(out)
}

func TestDecryptOAuthBlobRoundTrip(t *testing.T) {
	pkB64, privRaw, err := generateX25519()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The published pk must decode to SPKI-DER-shape base64url of the raw
	// point (the platform reconstructs the key from it).
	der, err := decodeBase64URL(pkB64)
	if err != nil {
		t.Fatalf("pk decode: %v", err)
	}
	if want := "302a300506032b656e032100"; fmt.Sprintf("%x", der[:12]) != want {
		t.Fatalf("pk not SPKI-prefixed: got %x", der[:12])
	}
	if !bytes.Equal(der[12:], mustPub(privRaw)) {
		t.Fatalf("pk payload != raw public key")
	}
	want := oauthResult{SK: "sk-test-1234567890", UID: "2026", URL: "https://api.xiaomimimo.com/v1"}
	// Padded and unpadded base64url must both decrypt (Node Buffer parity).
	for _, enc := range encodings(encryptOAuthBlob(t, mustPub(privRaw), want)) {
		got, err := decryptOAuthBlob(privRaw, enc)
		if err != nil {
			t.Fatalf("decrypt %q form: %v", enc[:16], err)
		}
		if got.SK != want.SK || got.UID != want.UID || got.URL != want.URL {
			t.Fatalf("roundtrip mismatch: %+v", got)
		}
	}
	// Corrupt tag → hard failure.
	bad := encryptOAuthBlob(t, mustPub(privRaw), want)
	rawBad, _ := decodeBase64URL(bad)
	rawBad[len(rawBad)-1] ^= 0xFF
	if _, err := decryptOAuthBlob(privRaw, base64.RawURLEncoding.EncodeToString(rawBad)); err == nil {
		t.Fatalf("corrupt blob decrypted")
	}
}

func encodings(b64 string) []string {
	return []string{b64, b64 + strings.Repeat("=", (4-len(b64)%4)%4)}
}

func mustPub(privRaw []byte) []byte {
	priv, err := ecdh.X25519().NewPrivateKey(privRaw)
	if err != nil {
		panic(err)
	}
	return priv.PublicKey().Bytes()
}

func TestBuildAuthorizeURL(t *testing.T) {
	got := buildAuthorizeURL("https://platform.xiaomimimo.com", "PUBKEY", "http://localhost:51000/", "mimo-code-cli-key-ab12cd34")
	want := "https://platform.xiaomimimo.com/authorize?pk=PUBKEY&redirect_uri=http%3A%2F%2Flocalhost%3A51000%2F&kn=mimocode&key_name=mimo-code-cli-key-ab12cd34"
	if got != want {
		t.Fatalf("authorize url mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestResolveAutoModel(t *testing.T) {
	cases := map[string]string{
		"mimo-auto":          "mimo-pro",
		"agent/mimo-auto":    "agent/mimo-pro",
		"mimo-flash":         "mimo-flash",
		"mimo-pro":           "mimo-pro",
		"mimo-x-pro-preview": "mimo-x-pro-preview", // passthrough: no desktop-verified rewrite
	}
	for in, want := range cases {
		if got := resolveAutoModel(in); got != want {
			t.Fatalf("resolveAutoModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildChatBodyPrivacyStrip(t *testing.T) {
	payload := []byte(`{"model":"mimo-auto","user":"uid-123","metadata":{"x":1},"service_tier":"default","logprobs":true,"top_logprobs":5,"logit_bias":{},"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	body, err := buildChatBody(payload, "mimo-auto", true, laneCookie)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, field := range privacyStripFields {
		if _, present := obj[field]; present {
			t.Fatalf("field %q survived the privacy strip", field)
		}
	}
	if obj["model"] != "mimo-pro" {
		t.Fatalf("cookie lane should resolve mimo-auto to mimo-pro, got %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Fatalf("stream should be pinned to true, got %v", obj["stream"])
	}
	// Key lane: no model rewrite, same strip.
	body2, err := buildChatBody(payload, "mimo-auto", false, laneKey)
	if err != nil {
		t.Fatalf("build key lane: %v", err)
	}
	var obj2 map[string]any
	_ = json.Unmarshal([]byte(body2), &obj2)
	if obj2["model"] != "mimo-auto" {
		t.Fatalf("key lane must not rewrite the model, got %v", obj2["model"])
	}
	if obj2["stream"] != false {
		t.Fatalf("stream should be pinned to false, got %v", obj2["stream"])
	}
}

func TestParseMeResponse(t *testing.T) {
	ok := parseMeResponse(200, []byte(`{"code":0,"data":{"userId":"2026001","region":"cn","country":"CN","userMark":"m1"}}`))
	if !ok.LoggedIn || ok.UserID != "2026001" || ok.Region != "cn" {
		t.Fatalf("me parse ok: %+v", ok)
	}
	numeric := parseMeResponse(200, []byte(`{"code":0,"data":{"userId":2026001}}`))
	if !numeric.LoggedIn || numeric.UserID != "2026001" {
		t.Fatalf("me parse numeric id: %+v", numeric)
	}
	rejected := parseMeResponse(401, []byte(`{"code":401,"message":"expired"}`))
	if rejected.LoggedIn || !rejected.Rejected {
		t.Fatalf("me parse rejected: %+v", rejected)
	}
	other := parseMeResponse(403, []byte(`{"code":100,"message":"x"}`))
	if other.LoggedIn || other.Rejected {
		t.Fatalf("non-rejection code must stay inconclusive: %+v", other)
	}
}

func TestCookieJarHeader(t *testing.T) {
	cookies := []mimoCookie{
		{Name: "serviceToken", Value: "tok", Domain: ".xiaomimimo.com", Path: "/"},
		{Name: "hostonly", Value: "h", Domain: "mimo-server-cn.xiaomimimo.com", Path: "/"},
		{Name: "scoped", Value: "s", Domain: ".xiaomimimo.com", Path: "/route"},
		{Name: "foreign", Value: "f", Domain: ".xiaomi.com", Path: "/"},
		{Name: "stale", Value: "x", Domain: ".xiaomimimo.com", Path: "/", Expires: 1},
	}
	target := "https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions"
	hdr := buildCookieJarHeader(cookies, target)
	for _, want := range []string{"serviceToken=tok", "hostonly=h"} {
		if !strings.Contains(hdr, want) {
			t.Fatalf("missing %q in %q", want, hdr)
		}
	}
	for _, banned := range []string{"scoped=s", "foreign=f", "stale=x"} {
		if strings.Contains(hdr, banned) {
			t.Fatalf("unwanted %q in %q", banned, hdr)
		}
	}
	// Path-scoped cookie attaches only under its path.
	if got := buildCookieJarHeader(cookies, "https://mimo-server-cn.xiaomimimo.com/api/user/xiaomi/me"); strings.Contains(got, "scoped=s") {
		t.Fatalf("path-scoped cookie leaked to /user path: %q", got)
	}
}

func TestApplyCookieLaneHeaders(t *testing.T) {
	sa := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{{Name: "serviceToken", Value: "tok", Domain: ".xiaomimimo.com", Path: "/"}}}}
	req, _ := http.NewRequest(http.MethodPost, "https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer leftover")
	applyCookieLaneHeaders(req, sa, "")
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("cookie lane must not carry Authorization, got %q", got)
	}
	if got := req.Header.Get("X-Mimo-Source"); got != sourceCookieLane {
		t.Fatalf("X-Mimo-Source = %q, want %q", got, sourceCookieLane)
	}
	if got := req.Header.Get("X-Client-Version"); got != desktopAppVersion {
		t.Fatalf("X-Client-Version = %q, want %q", got, desktopAppVersion)
	}
	if got := req.Header.Get("Cookie"); got != "serviceToken=tok" {
		t.Fatalf("Cookie = %q", got)
	}
}

func TestApplyKeyLaneHeaders(t *testing.T) {
	sa := &storedAuth{Auth: mimoTokens{Lane: laneKey, SK: "sk-abc"}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.xiaomimimo.com/v1/chat/completions", nil)
	applyKeyLaneHeaders(req, sa, "")
	if got := req.Header.Get("Authorization"); got != "Bearer sk-abc" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-Mimo-Source"); got != sourceKeyLane {
		t.Fatalf("X-Mimo-Source = %q, want %q", got, sourceKeyLane)
	}
}

func TestRouteFor(t *testing.T) {
	sa := &storedAuth{Auth: mimoTokens{Lane: laneKey, SK: "sk", BaseURL: "https://issued.example.com/v1/"}}
	if got := routeFor(sa).endpoint; got != "https://issued.example.com/v1/chat/completions" {
		t.Fatalf("key lane endpoint = %q", got)
	}
	sa2 := &storedAuth{Auth: mimoTokens{Lane: laneKey, SK: "sk"}}
	if got := routeFor(sa2).endpoint; got != "https://api.xiaomimimo.com/v1/chat/completions" {
		t.Fatalf("key lane default endpoint = %q", got)
	}
	sa3 := &storedAuth{Auth: mimoTokens{Lane: laneCookie}}
	if got := routeFor(sa3).endpoint; got != "https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions" {
		t.Fatalf("cookie lane endpoint = %q", got)
	}
	sa4 := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Region: "sgp"}}
	if got := routeFor(sa4).endpoint; got != "https://mimo-server-sgp.xiaomimimo.com/api/route/chat/completions" {
		t.Fatalf("cookie lane region endpoint = %q", got)
	}
}

func TestParseStored(t *testing.T) {
	nested := []byte(`{"type":"mimo","auth":{"lane":"key","sk":"sk-1","uid":"u1"},"account":{"uid":"u1"}}`)
	sa, err := parseStored(nested)
	if err != nil || sa.Auth.SK != "sk-1" || sa.Account.UID != "u1" {
		t.Fatalf("nested parse: %+v err=%v", sa, err)
	}
	flat := []byte(`{"lane":"cookie","cookies":[{"name":"serviceToken","value":"tok","domain":".xiaomimimo.com","path":"/"}],"uid":"u2"}`)
	sa2, err := parseStored(flat)
	if err != nil || authLaneFor(sa2) != laneCookie || len(sa2.Auth.Cookies) != 1 {
		t.Fatalf("flat parse: %+v err=%v", sa2, err)
	}
	if _, err := parseStored([]byte(`{"lane":"key"}`)); err == nil {
		t.Fatalf("key lane without sk must fail")
	}
	if _, err := parseStored([]byte(`{"lane":"cookie"}`)); err == nil {
		t.Fatalf("cookie lane without cookies must fail")
	}
}

func TestMimoUnwrapFrameAndClean(t *testing.T) {
	body, meaningful, err := mimoUnwrapFrame(`data: {"id":"x","choices":[{"delta":{"content":"a"}}]}`)
	if err != nil || !meaningful || body == "" {
		t.Fatalf("normal frame: %q %v %v", body, meaningful, err)
	}
	body, meaningful, _ = mimoUnwrapFrame(`data: [DONE]`)
	if body != "[DONE]" || meaningful {
		t.Fatalf("done frame: %q %v", body, meaningful)
	}
	if _, _, err := mimoUnwrapFrame(`data: {"error":{"code":1,"message":"boom"}}`); err == nil {
		t.Fatalf("error-in-200 must surface")
	}
	if _, meaningful, err := mimoUnwrapFrame(": keepalive comment"); err != nil || meaningful {
		t.Fatalf("comment line: %v %v", meaningful, err)
	}
	// Empty tool-call shell is stripped; role-only delta survives.
	cleaned := cleanChunkJSON(`{"choices":[{"delta":{"tool_calls":[],"refusal":""}}]}`)
	if cleaned != "" {
		t.Fatalf("empty delta should drop, got %q", cleaned)
	}
	roleKept := cleanChunkJSON(`{"choices":[{"delta":{"role":"assistant"},"finish_reason":""}]}`)
	if !strings.Contains(roleKept, `"role":"assistant"`) {
		t.Fatalf("role-only delta must survive: %q", roleKept)
	}
}

func TestIsModelAllowlistError(t *testing.T) {
	if !isModelAllowlistError([]byte(`{"error":{"message":"该模型不在当前Key可用模型范围内"}}`)) {
		t.Fatalf("CN shape must match")
	}
	if !isModelAllowlistError([]byte(`{"error":{"message":"model is not in the api-key allowlist for this key"}}`)) {
		t.Fatalf("EN shape must match")
	}
	if isModelAllowlistError([]byte(`{"error":{"message":"session expired"}}`)) {
		t.Fatalf("unrelated 401 must not match")
	}
}

func TestRedactSecrets(t *testing.T) {
	in := `Bearer sk-very-secret-123456 and {"access_token":"abcdef123456789"} Set-Cookie: serviceToken=abc123def456; Path=/`
	out := redactSecrets(in)
	if strings.Contains(out, "sk-very-secret") || strings.Contains(out, "abcdef123456789") || strings.Contains(out, "abc123def456") {
		t.Fatalf("secrets survived: %q", out)
	}
}

func TestMimoModelsCatalog(t *testing.T) {
	models := mimoModels()
	if len(models) != 3 {
		t.Fatalf("catalog size = %d", len(models))
	}
	want := map[string]bool{"mimo-auto": true, "mimo-flash": true, "mimo-pro": true}
	for _, m := range models {
		if !want[m.ID] {
			t.Fatalf("unexpected model id %q", m.ID)
		}
		if m.ContextLength != 1000000 || m.MaxCompletionTokens != 128000 {
			t.Fatalf("%s limits wrong: %+v", m.ID, m)
		}
	}
}

func TestRegionBaseFor(t *testing.T) {
	if got := regionBaseFor(nil); got != regionBases["cn"] {
		t.Fatalf("default region = %q", got)
	}
	sa := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Region: "RU"}}
	if got := regionBaseFor(sa); got != regionBases["ru"] {
		t.Fatalf("cred region = %q", got)
	}
}

// -----------------------------------------------------------------------------
// Upstream-facing tests via the direct-mode bridge (httptest upstreams)
// -----------------------------------------------------------------------------

func cookieCred(t *testing.T) *storedAuth {
	t.Helper()
	return &storedAuth{
		Auth:    mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{{Name: "serviceToken", Value: "tok", Domain: ".127.0.0.1", Path: "/"}}, Region: ""},
		Account: mimoAccount{UID: "u-test"},
	}
}

func TestHandleExecExecuteCookieLaneRenewRetry(t *testing.T) {
	calls := 0
	var sawRenewHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "" {
			t.Errorf("cookie lane carried Authorization: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Mimo-Source") != sourceCookieLane {
			t.Errorf("X-Mimo-Source = %q", r.Header.Get("X-Mimo-Source"))
		}
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"session invalid"}}`))
			return
		}
		sawRenewHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	// Point the route at the test server via the credential region... the
	// route is built from regionBaseFor; override through a fake region map
	// entry is not possible, so swap the sa's cookies domain and monkey-patch
	// the base via config region. Simplest: point regionBases cn at upstream.
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()

	origRenew := renewCookieSessionFn
	renewCookieSessionFn = func(sa *storedAuth) bool { return true }
	defer func() { renewCookieSessionFn = origRenew }()

	sa := cookieCred(t)
	storage, _ := json.Marshal(sa)
	req, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID:      "a1",
		Model:       "mimo/mimo-pro",
		Payload:     []byte(`{"model":"mimo-pro","messages":[{"role":"user","content":"ping"}]}`),
		StorageJSON: storage,
	})
	resp, err := handleExecExecute(req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", string(resp))
	}
	if calls != 2 {
		t.Fatalf("expected 401 → renew → retry (2 calls), got %d", calls)
	}
	if sawRenewHeader.Get("Cookie") != "serviceToken=tok" {
		t.Fatalf("retry lost the jar: %q", sawRenewHeader.Get("Cookie"))
	}
}

func TestHandleExecExecuteModelAllowlistNoRetry(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"该模型不在当前Key可用模型范围内"}}`))
	}))
	defer upstream.Close()
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()
	origRenew := renewCookieSessionFn
	renewCookieSessionFn = func(sa *storedAuth) bool { return true }
	defer func() { renewCookieSessionFn = origRenew }()

	sa := cookieCred(t)
	storage, _ := json.Marshal(sa)
	req, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID:      "a1",
		Model:       "mimo/mimo-pro",
		Payload:     []byte(`{"model":"mimo-pro","messages":[]}`),
		StorageJSON: storage,
	})
	_, err := handleExecExecute(req)
	if err == nil || calls != 1 {
		t.Fatalf("model-range 401 must fail fast without retry (calls=%d err=%v)", calls, err)
	}
}

func TestHandleExecStreamCollect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hel","tool_calls":[]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"lo"}}]}`+"\n\n")
		fmt.Fprint(w, `data: [DONE]`+"\n\n")
	}))
	defer upstream.Close()
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()

	sa := cookieCred(t)
	storage, _ := json.Marshal(sa)
	req, _ := json.Marshal(executorStreamRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:      "a1",
			Model:       "mimo/mimo-flash",
			Payload:     []byte(`{"model":"mimo-flash","messages":[],"stream":true}`),
			StorageJSON: storage,
			Metadata:    map[string]any{"request_path": "/v1/chat/completions"},
		},
		StreamID: "", // synchronous collect path
	})
	resp, err := handleExecStream(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", string(resp))
	}
	var sr streamResponse
	if err := json.Unmarshal(env.Result, &sr); err != nil {
		t.Fatalf("stream response: %v", err)
	}
	if len(sr.Chunks) != 3 {
		t.Fatalf("chunk count = %d (%s)", len(sr.Chunks), string(env.Result))
	}
	joined := ""
	for _, c := range sr.Chunks {
		joined += string(c.Payload)
	}
	if strings.Contains(joined, "data: ") {
		t.Fatalf("chat-completions path must receive unframed payloads")
	}
	if strings.Contains(joined, "tool_calls") {
		t.Fatalf("empty tool_calls shell must be cleaned")
	}
}

func TestCollectUpstreamStreamErrorIn200(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"error":{"code":500,"message":"boom"}}`+"\n\n")
	}))
	defer upstream.Close()
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()
	sa := cookieCred(t)
	route := routeFor(sa)
	if _, _, err := collectUpstreamStream(sa, route, `{"model":"mimo-pro","messages":[]}`, false); err == nil {
		t.Fatalf("error-in-200 must fail the collect")
	}
}

func TestHandleParseAuthOwnership(t *testing.T) {
	// Foreign declared type → not handled.
	foreign, _ := json.Marshal(pluginapi.AuthParseRequest{FileName: "other.json", RawJSON: []byte(`{"type":"other"}`)})
	respRaw, err := handleParseAuth(foreign)
	if err != nil {
		t.Fatalf("foreign parse: %v", err)
	}
	var foreignEnv envelope
	_ = json.Unmarshal(respRaw, &foreignEnv)
	var foreignResp pluginapi.AuthParseResponse
	_ = json.Unmarshal(foreignEnv.Result, &foreignResp)
	if foreignResp.Handled {
		t.Fatalf("foreign type must not be handled")
	}

	sa := &storedAuth{Auth: mimoTokens{Lane: laneKey, SK: "sk-1", UID: "u1"}, Account: mimoAccount{UID: "u1"}}
	raw, _ := json.Marshal(sa)
	req, _ := json.Marshal(pluginapi.AuthParseRequest{FileName: "mimo-key-u1.json", RawJSON: raw})
	respRaw2, err := handleParseAuth(req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(respRaw2, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", string(respRaw2))
	}
	var pr pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &pr); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if !pr.Handled || len(pr.Auth.StorageJSON) == 0 {
		t.Fatalf("mimo file must be handled: %+v", pr)
	}
	if pr.Auth.ID != "" {
		t.Fatalf("ID must stay empty (host derives it), got %q", pr.Auth.ID)
	}
}

func TestCookiePathMatching(t *testing.T) {
	cases := []struct {
		cookiePath, reqPath string
		want                bool
	}{
		{"/", "/anything", true},
		{"/api", "/api/route/x", true},
		{"/api", "/api2/x", false},
		{"/api/", "/api/x", false},
		{"", "/x", true},
	}
	for _, c := range cases {
		if got := cookieMatchesPath(c.cookiePath, c.reqPath); got != c.want {
			t.Fatalf("cookieMatchesPath(%q,%q)=%v want %v", c.cookiePath, c.reqPath, got, c.want)
		}
	}
}

func TestHostOnlyCookieMatching(t *testing.T) {
	if !cookieMatchesHost(".xiaomimimo.com", "mimo-server-cn.xiaomimimo.com") {
		t.Fatalf("domain cookie must match subdomain")
	}
	if cookieMatchesHost("mimo-server-sgp.xiaomimimo.com", "mimo-server-cn.xiaomimimo.com") {
		t.Fatalf("host-only cookie must not cross hosts")
	}
	if _, err := url.Parse("https://x.example"); err != nil {
		t.Fatalf("sanity url parse: %v", err)
	}
}

func TestUserDataRootFor(t *testing.T) {
	// Expectations go through FromSlash so the table is separator-agnostic.
	winBase := filepath.FromSlash("C:/Users/u/AppData/Roaming/Xiaomi MiMo AI")
	uxBase := filepath.FromSlash("/home/u/.config/Xiaomi MiMo AI")
	cases := []struct {
		dbPath string
		want   string
	}{
		// Chromium 96+ / Electron 15+ layout (this desktop: Electron 41).
		{winBase + "/Partitions/xiaomi-account/Network/Cookies", winBase},
		// Legacy pre-96 layout still probed for old installs.
		{winBase + "/Partitions/xiaomi-account/Cookies", winBase},
		// POSIX variants.
		{uxBase + "/Partitions/xiaomi-account/Network/Cookies", uxBase},
		{uxBase + "/Partitions/xiaomi-account/Cookies", uxBase},
		// Windows separator style must resolve identically.
		{filepath.FromSlash(winBase) + string(filepath.Separator) + filepath.Join("Partitions", "xiaomi-account", "Network", "Cookies"), winBase},
		// Custom cookie_paths outside a Partitions tree: fallback keeps the
		// old two-level-parent behavior (Local State lookup fails loudly).
		{filepath.Join("/opt", "custom", "Cookies"), filepath.FromSlash("/opt")},
	}
	for _, c := range cases {
		if got := userDataRootFor(c.dbPath); got != c.want {
			t.Fatalf("userDataRootFor(%q)=%q want %q", c.dbPath, got, c.want)
		}
	}
}
