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
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestClientSignVector(t *testing.T) {
	// Known-answer vector: sha1("nonce=1234567890&abcdefgh") → base64 →
	// url-escape. Independent of the implementation's own primitives.
	if got := clientSign("1234567890", "abcdefgh"); got != "02i8YjagkChj1jgJHHyz0OSzPJs%3D" {
		t.Fatalf("clientSign = %q", got)
	}
}

func TestRegionSIDAndCandidates(t *testing.T) {
	if regionSID("SGP") != "mimosgp" || regionSID("cn") != "mimopc" {
		t.Fatalf("measured sids wrong")
	}
	if regionSID("ru") != "" || regionSID("in") != "" || regionSID("") != "" {
		t.Fatalf("unmeasured regions must not guess a sid")
	}
	if got := regionCandidates("sgp", ""); len(got) != 1 || got[0] != "sgp" {
		t.Fatalf("pinned candidates = %v", got)
	}
	auto := regionCandidates("auto", "")
	if len(auto) != 2 || auto[0] != "sgp" || auto[1] != "cn" {
		t.Fatalf("auto candidates = %v (sgp first: measured working cluster)", auto)
	}
	skip := regionCandidates("auto", "sgp")
	if len(skip) != 1 || skip[0] != "cn" {
		t.Fatalf("auto must skip the already-bound region: %v", skip)
	}
}

func TestParseServiceLogin(t *testing.T) {
	// Quoted-string nonce (historical fixture shape; the suite used to cover
	// ONLY this form while the real wire is the bare number below).
	body := "&&&START&&&" + `{"code":0,"ssecurity":"sECret==","nonce":"3862976506","location":"https://sts.example/api/sts?sign=abc"}` + "&&&END&&&"
	res, err := parseServiceLogin([]byte(body))
	if err != nil || res.Security != "sECret==" || res.Nonce != "3862976506" || !strings.HasPrefix(res.Location, "https://sts.example") {
		t.Fatalf("parse: %+v err=%v", res, err)
	}

	// Bare-number nonce = the measured real-machine wire shape (2026-09-24).
	// Known-answer: the parsed digits must be exactly what clientSign hashes —
	// same vector as TestClientSignVector.
	bare := "&&&START&&&" + `{"code":0,"ssecurity":"abcdefgh","nonce":1234567890,"location":"https://sts.example/api/sts?sign=abc"}` + "&&&END&&&"
	res2, err := parseServiceLogin([]byte(bare))
	if err != nil || res2.Nonce != "1234567890" {
		t.Fatalf("bare-number nonce: %+v err=%v", res2, err)
	}
	if got := clientSign(string(res2.Nonce), res2.Security); got != "02i8YjagkChj1jgJHHyz0OSzPJs%3D" {
		t.Fatalf("clientSign from parsed bare nonce = %q", got)
	}

	// Measured magnitude (19 digits): any float64-backed decoding would round
	// this past the 53-bit mantissa and silently corrupt the signature.
	huge := "&&&START&&&" + `{"code":0,"ssecurity":"s","nonce":4341996316119746560,"location":"https://sts.example/l"}` + "&&&END&&&"
	res3, err := parseServiceLogin([]byte(huge))
	if err != nil || res3.Nonce != "4341996316119746560" {
		t.Fatalf("19-digit nonce must survive verbatim: %+v err=%v", res3, err)
	}

	// Empty / null nonce are shape failures too.
	if _, err := parseServiceLogin([]byte(`{"code":0,"ssecurity":"s","nonce":"","location":"l"}`)); err == nil {
		t.Fatalf("empty nonce must fail")
	}
	if _, err := parseServiceLogin([]byte(`{"code":0,"ssecurity":"s","nonce":null,"location":"l"}`)); err == nil {
		t.Fatalf("null nonce must fail")
	}

	if _, err := parseServiceLogin([]byte(`{"code":1010,"message":"expired"}`)); err == nil {
		t.Fatalf("code!=0 must fail")
	}
	if _, err := parseServiceLogin([]byte(`{"code":0}`)); err == nil {
		t.Fatalf("missing ssecurity/nonce/location must fail")
	}
	if _, err := parseServiceLogin([]byte("&&&START&&&not json&&&END&&&")); err == nil {
		t.Fatalf("garbage must fail")
	}
}

func TestBuildCookieHeaderOrder(t *testing.T) {
	cookies := []mimoCookie{
		{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
		{Name: "userId", Value: "acct", Domain: ".account.xiaomi.com"},
		{Name: "cUserId", Value: "cu", Domain: ".account.xiaomi.com"},
		{Name: "uLocale", Value: "zh", Domain: ".xiaomi.com"},
		{Name: "serviceToken", Value: "st", Domain: ".xiaomimimo.com"},
		{Name: "userId", Value: "minted", Domain: ".xiaomimimo.com"},
		{Name: "mimosgp_ph", Value: "ph", Domain: ".xiaomimimo.com"},
		{Name: "mimosgp_slh", Value: "slh", Domain: ".xiaomimimo.com"},
		{Name: "mimopc_ph", Value: "wrongsid", Domain: ".xiaomimimo.com"},
	}
	got := buildCookieHeader(cookies, "mimosgp")
	want := "userId=minted; serviceToken=st; cUserId=cu; mimosgp_ph=ph"
	if got != want {
		t.Fatalf("buildCookieHeader = %q, want %q", got, want)
	}
	// Login-side rows and other-sid rows must never ride the header.
	for _, banned := range []string{"passToken=pt", "uLocale=zh", "slh", "mimopc_ph"} {
		if strings.Contains(got, banned) {
			t.Fatalf("leaked %q in %q", banned, got)
		}
	}
	// Missing rows drop silently; unknown sid skips the _ph slot.
	if got := buildCookieHeader(cookies[:1], ""); got != "" {
		t.Fatalf("bootstrap-only header must be empty, got %q", got)
	}
	if got := buildCookieHeader(cookies, ""); strings.Contains(got, "_ph") {
		t.Fatalf("no sid → no _ph slot: %q", got)
	}
}

func TestPickBootstrapPrefersAccountHost(t *testing.T) {
	cookies := []mimoCookie{
		{Name: "cUserId", Value: "mirror", Domain: ".xiaomi.com"},
		{Name: "cUserId", Value: "account", Domain: ".account.xiaomi.com"},
		{Name: "serviceToken", Value: "st", Domain: ".xiaomimimo.com"},
		{Name: "foreign", Value: "x", Domain: ".example.com"},
	}
	boot := pickBootstrap(cookies)
	if len(boot) != 1 || boot[0].Name != "cUserId" || boot[0].Value != "account" {
		t.Fatalf("pickBootstrap = %+v", boot)
	}
}

func TestRenderCookieHeaderShapes(t *testing.T) {
	// Minted jar → assembled M2 header.
	minted := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Region: "sgp", Cookies: []mimoCookie{
		{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
		{Name: "userId", Value: "u9", Domain: ".account.xiaomi.com"},
		{Name: "serviceToken", Value: "st", Domain: ".xiaomimimo.com"},
		{Name: "mimosgp_ph", Value: "ph", Domain: ".xiaomimimo.com"},
	}}}
	if got := renderCookieHeader(minted, "https://mimo-server-sgp.xiaomimimo.com/api/route/chat/completions"); got != "userId=u9; serviceToken=st; mimosgp_ph=ph" {
		t.Fatalf("minted render = %q", got)
	}
	// Legacy jar (no mint, upstream rows exist) → M1 whole-jar render.
	legacy := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
		{Name: "legacy", Value: "l", Domain: ".xiaomimimo.com", Path: "/"},
	}}}
	if got := renderCookieHeader(legacy, "https://mimo-server-sgp.xiaomimimo.com/api/route/chat/completions"); got != "legacy=l" {
		t.Fatalf("legacy render = %q", got)
	}
	// Bootstrap-only → empty (the ladder re-mints; never leak account rows).
	boot := &storedAuth{Auth: mimoTokens{Lane: laneCookie, Cookies: legacy.Auth.Cookies[:0]}}
	boot.Auth.Cookies = []mimoCookie{{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"}}
	if got := renderCookieHeader(boot, "https://mimo-server-sgp.xiaomimimo.com/api/route/chat/completions"); got != "" {
		t.Fatalf("bootstrap-only render must be empty, got %q", got)
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

// TestRedactSecretsCredentialKV pins the deep-audit P2 #2 fix (2026-09-25):
// an upstream error body echoing bare credential key=value pairs — the xiaomi
// ticket set (serviceToken/passToken/cUserId), the sk lane's key, the
// region-scoped cookie rows (<sid>_ph/<sid>_slh), URL-encoded values — must
// redact, while unrelated kv shapes (task=/mask=/risk=) must survive intact.
// The passToken vectors carry the real wire shape `V1:<base64>` — the value
// class must include `:` or the run dies at the colon and nothing matches
// (reviewer re-check find; bytes below are synthetic, only the shape is real
// — never bake a live credential fragment into the repo).
func TestRedactSecretsCredentialKV(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string // must not survive
		keep   string // non-secret context that must survive
	}{
		{"bare serviceToken kv", `invalid ticket: serviceToken=TQXRlGs1234567890abc in cookie`, "TQXRlGs1234567890abc", `invalid ticket: `},
		{"json serviceToken kv", `{"msg":"bad credentials","serviceToken":"TQXRlGs1234567890abc"}`, "TQXRlGs1234567890abc", `"msg":"bad credentials",`},
		{"snake_case service_token", `{"service_token":"srvTkn1234567890ab"}`, "srvTkn1234567890ab", ""},
		{"passToken V1: bare kv (real wire shape)", `login failed passToken=V1:zKq8mP2vXw9Qr5Tn3YbC6JdH1sL4FgV0Ne7UjIkMhAo`, "zKq8mP2vXw9Qr5Tn3YbC6JdH1sL4FgV0Ne7UjIkMhAo", `login failed `},
		{"passToken V1: json kv", `{"error":"stale","passToken":"V1:zKq8mP2vXw9Qr5Tn3YbC6JdH1sL4FgV0Ne7UjIkMhAo"}`, "zKq8mP2vXw9Qr5Tn3YbC6JdH1sL4FgV0Ne7UjIkMhAo", `"error":"stale",`},
		{"cUserId kv", `cUserId=603318735706639872 rejected`, "603318735706639872", ` rejected`},
		{"sk kv json", `{"sk":"skval1234567890abcd","detail":"bad key"}`, "skval1234567890abcd", `"detail":"bad key"`},
		{"sk kv bare", `bad key: sk=skval1234567890abcd`, "skval1234567890abcd", `bad key: `},
		{"region slh row", `mimosgp_slh=YWJjZGVmZ2hpamtsbW5vcA stale`, "YWJjZGVmZ2hpamtsbW5vcA", ` stale`},
		{"region ph row", `mimocn_ph=Zm9vYmFyYmF6cXV1eA== missing`, "Zm9vYmFyYmF6cXV1eA", ` missing`},
		{"url-encoded value", `serviceToken=TQXRlGs%2Babc%2Fdef%3D rejected`, "TQXRlGs%2Babc", ` rejected`},
	}
	for _, tc := range cases {
		out := redactSecrets(tc.in)
		if strings.Contains(out, tc.secret) {
			t.Errorf("%s: secret survived: %q", tc.name, out)
		}
		if tc.keep != "" && !strings.Contains(out, tc.keep) {
			t.Errorf("%s: lost non-secret context: %q", tc.name, out)
		}
	}
	// The short `sk` name is boundary-guarded and names must be complete:
	// unrelated kv shapes — including colon-carrying values — stay intact.
	in := `task=1234567890123456 mask=abcdefghijklmn risk=9999999999999999 deadline=12:34:56:78:90:12 access_token_expiry=09:00:00:00`
	if out := redactSecrets(in); out != in {
		t.Fatalf("false positive on unrelated kv shapes: %q", out)
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
		Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
			// Minted ticket (rendered by buildCookieHeader).
			{Name: "serviceToken", Value: "tok", Domain: ".127.0.0.1", Path: "/"},
			// Account-domain bootstrap rows (consumed by the exchange only).
			{Name: "passToken", Value: "pt-1", Domain: ".account.xiaomi.com", Path: "/"},
			{Name: "userId", Value: "u-test", Domain: ".account.xiaomi.com", Path: "/"},
			{Name: "cUserId", Value: "cu-1", Domain: ".account.xiaomi.com", Path: "/"},
		}, Region: ""},
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
	// M2 assembled order: userId → serviceToken → cUserId (Region "" → no _ph).
	if got := sawRenewHeader.Get("Cookie"); got != "userId=u-test; serviceToken=tok; cUserId=cu-1" {
		t.Fatalf("retry lost the assembled ticket: %q", got)
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

// TestSendChatWithCookieRetryClosesFaultStream pins the deep-audit P2 #1 fix
// (2026-09-25): past the fault gate every exit must Close the first (drained)
// stream — Read's EOF does not release the host-side record, Close is the only
// MethodHostHTTPStreamClose path, and a long-lived host would accumulate one
// dead stream per faulted call. Direct-mode fixtures make "closed" observable
// (Close nils the buffered body); the returned retry stream and the non-fault
// passthrough must stay open — the caller owns those Closes.
func TestSendChatWithCookieRetryClosesFaultStream(t *testing.T) {
	origSend, origRenew := sendChatFn, renewCookieSessionFn
	defer func() { sendChatFn, renewCookieSessionFn = origSend, origRenew }()

	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://mimo.test/chat", nil)
	}
	route := chatRoute{lane: laneCookie}
	sa := &storedAuth{}
	newStream := func(body string) *hostHTTPStream { return &hostHTTPStream{direct: []byte(body)} }
	pong := `{"id":"1","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	t.Run("re-mint success: fault stream closed, retry stream stays open", func(t *testing.T) {
		attempts := 0
		fault, retry := newStream(`{"error":{"message":"session invalid"}}`), newStream(pong)
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			attempts++
			if attempts == 1 {
				return fault, http.StatusUnauthorized, nil, nil
			}
			return retry, http.StatusOK, nil, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { return true }
		got, _, _, err := sendChatWithCookieRetry(sa, route, buildReq)
		if err != nil || got != retry {
			t.Fatalf("retry must return the second stream (err=%v)", err)
		}
		if fault.direct != nil {
			t.Fatalf("first stream leaked on the re-mint path")
		}
		if retry.direct == nil {
			t.Fatalf("returned stream must stay open — the caller owns its Close")
		}
	})

	t.Run("re-mint failure: fault stream closed", func(t *testing.T) {
		fault := newStream(`{"error":{"message":"session invalid"}}`)
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			return fault, http.StatusUnauthorized, nil, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { return false }
		if _, _, _, err := sendChatWithCookieRetry(sa, route, buildReq); err == nil {
			t.Fatalf("re-mint failure must surface")
		}
		if fault.direct != nil {
			t.Fatalf("first stream leaked on the re-mint-failed exit")
		}
	})

	t.Run("model-range 401: fault stream closed, no renew, no retry", func(t *testing.T) {
		fault := newStream(`{"error":{"message":"该模型不在当前Key可用模型范围内"}}`)
		attempts, renewed := 0, false
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			attempts++
			return fault, http.StatusUnauthorized, nil, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { renewed = true; return true }
		_, sc, _, err := sendChatWithCookieRetry(sa, route, buildReq)
		if err == nil || sc != http.StatusUnauthorized || attempts != 1 || renewed {
			t.Fatalf("model-range 401 must fail fast (sc=%d attempts=%d renewed=%v err=%v)", sc, attempts, renewed, err)
		}
		if fault.direct != nil {
			t.Fatalf("first stream leaked on the allowlist exit")
		}
	})

	t.Run("200 login page: fault stream closed", func(t *testing.T) {
		fault := newStream(`<!DOCTYPE html><html><head><title>小米帐号 - 登录</title></head><body></body></html>`)
		hdrs := http.Header{"Content-Type": {"text/html; charset=utf-8"}}
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			return fault, http.StatusOK, hdrs, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { return false }
		_, sc, _, err := sendChatWithCookieRetry(sa, route, buildReq)
		if err == nil || sc != http.StatusOK {
			t.Fatalf("login-page 200 must surface as a session fault (sc=%d err=%v)", sc, err)
		}
		if fault.direct != nil {
			t.Fatalf("first stream leaked on the html-200 exit")
		}
	})

	t.Run("non-fault passthrough: stream stays open, no renew", func(t *testing.T) {
		okStream := newStream(pong)
		renewed := false
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			return okStream, http.StatusOK, nil, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { renewed = true; return true }
		got, _, _, err := sendChatWithCookieRetry(sa, route, buildReq)
		if err != nil || got != okStream {
			t.Fatalf("passthrough must return the same stream (err=%v)", err)
		}
		if okStream.direct == nil {
			t.Fatalf("passthrough stream must stay open — the caller owns its Close")
		}
		if renewed {
			t.Fatalf("non-fault status must not re-mint")
		}
	})

	t.Run("sk lane fault: passthrough untouched, stream stays open", func(t *testing.T) {
		fault := newStream(`{"error":{"message":"invalid api key"}}`)
		renewed := false
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			return fault, http.StatusUnauthorized, nil, nil
		}
		renewCookieSessionFn = func(*storedAuth) bool { renewed = true; return true }
		got, sc, _, err := sendChatWithCookieRetry(sa, chatRoute{lane: laneKey}, buildReq)
		if err != nil || got != fault || sc != http.StatusUnauthorized {
			t.Fatalf("sk lane must pass through untouched (sc=%d err=%v)", sc, err)
		}
		if fault.direct == nil {
			t.Fatalf("sk-lane stream must stay open — the caller owns its Close")
		}
		if renewed {
			t.Fatalf("sk lane must not re-mint the cookie session")
		}
	})

	t.Run("transport error: passes through with nil stream", func(t *testing.T) {
		sendChatFn = func(func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
			return nil, 0, nil, fmt.Errorf("dial upstream: refused")
		}
		if _, _, _, err := sendChatWithCookieRetry(sa, route, buildReq); err == nil {
			t.Fatalf("transport error must surface")
		}
	})
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

// -----------------------------------------------------------------------------
// M2 exchange end-to-end (httptest passport + STS)
// -----------------------------------------------------------------------------

// startPassportStub spins one httptest server playing both the P1
// serviceLogin endpoint and the P2 STS ticket mint, wired so P1's location
// points at its own /api/sts. It asserts the measured wire shape: P1 query
// (sid/_json), P1 cookie = bootstrap rows, P2 cookieless with a clientSign
// that verifies against the same formula the client used.
func startPassportStub(t *testing.T) *httptest.Server {
	t.Helper()
	// P2 first (no self-reference): the STS ticket mint.
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The wire param is url-escaped; r.URL.Query() already decoded it.
		// Recompute the expectation from primitives, not from clientSign().
		sum := sha1.Sum([]byte("nonce=3862976506&sECret=="))
		if got := r.URL.Query().Get("clientSign"); got != base64.StdEncoding.EncodeToString(sum[:]) {
			t.Errorf("P2 clientSign mismatch: %q", got)
		}
		if ck := r.Header.Get("Cookie"); ck != "" {
			t.Errorf("P2 must be cookieless, got %q", ck)
		}
		http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "st-new", Domain: "xiaomimimo.com", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "u9", Domain: "xiaomimimo.com", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "mimosgp_ph", Value: "ph9", Domain: "xiaomimimo.com", Path: "/"})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { sts.Close() })
	// P1: serviceLogin(_json), location pointing at the P2 server.
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sid") != "mimosgp" || r.URL.Query().Get("_json") != "true" {
			t.Errorf("P1 query wrong: %q", r.URL.RawQuery)
		}
		ck := r.Header.Get("Cookie")
		for _, want := range []string{"passToken=pt", "userId=", "cUserId=cu"} {
			if !strings.Contains(ck, want) {
				t.Errorf("P1 missing bootstrap row %q in %q", want, ck)
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		// nonce rides as a bare number — the measured wire shape (2026-09-24).
		fmt.Fprintf(w, "&&&START&&&%s&&&END&&&", `{"code":0,"ssecurity":"sECret==","nonce":3862976506,"location":"`+sts.URL+`/api/sts?sign=abc&followup=x"}`)
	}))
}

// withPasspointStub points serviceLoginBase at a stub for the test's lifetime.
func withPassportStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := startPassportStub(t)
	t.Cleanup(func() { srv.Close() })
	return srv
}

// swapServiceLoginBase redirects the exchange at the stub and restores it.
func swapServiceLoginBase(t *testing.T, url string) {
	t.Helper()
	orig := serviceLoginBase
	serviceLoginBase = url
	t.Cleanup(func() { serviceLoginBase = orig })
}

func TestExchangeServiceTokenRoundTrip(t *testing.T) {
	srv := withPassportStub(t)
	swapServiceLoginBase(t, srv.URL)
	bootstrap := []mimoCookie{
		{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
		{Name: "userId", Value: "u9", Domain: ".account.xiaomi.com"},
		{Name: "cUserId", Value: "cu", Domain: ".account.xiaomi.com"},
	}
	res, err := exchangeServiceToken(bootstrap, "mimosgp")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.SID != "mimosgp" || cookieValue(res.Cookies, "serviceToken") != "st-new" {
		t.Fatalf("mint wrong: %+v", res)
	}
	for _, name := range []string{"serviceToken", "userId", "mimosgp_ph"} {
		if cookieValue(res.Cookies, name) == "" {
			t.Fatalf("minted row %s missing", name)
		}
	}
	// Minted rows must be scoped to the upstream domain.
	for _, c := range res.Cookies {
		if !isMimoUpstreamHost(c.Domain) {
			t.Fatalf("minted row %s has non-upstream domain %q", c.Name, c.Domain)
		}
	}
}

func TestExchangeForCredentialMergesAndStamps(t *testing.T) {
	srv := withPassportStub(t)
	swapServiceLoginBase(t, srv.URL)
	sa := &storedAuth{
		Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
			{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
			{Name: "userId", Value: "stale-acct", Domain: ".account.xiaomi.com"},
			{Name: "cUserId", Value: "cu", Domain: ".account.xiaomi.com"},
			{Name: "serviceToken", Value: "st-old", Domain: ".xiaomimimo.com"},
			{Name: "mimosgp_ph", Value: "ph-old", Domain: ".xiaomimimo.com"},
		}},
		Account: mimoAccount{UID: "u9"},
	}
	if !exchangeForCredential(sa) {
		t.Fatalf("exchange must succeed")
	}
	if sa.Auth.Region != "sgp" || sa.Auth.SID != "mimosgp" || sa.Auth.ExchangedAt == 0 {
		t.Fatalf("stamping wrong: region=%q sid=%q at=%d", sa.Auth.Region, sa.Auth.SID, sa.Auth.ExchangedAt)
	}
	if got := buildCookieHeader(sa.Auth.Cookies, sa.Auth.SID); got != "userId=u9; serviceToken=st-new; cUserId=cu; mimosgp_ph=ph9" {
		t.Fatalf("merged header = %q", got)
	}
	if strings.Contains(buildCookieHeader(sa.Auth.Cookies, sa.Auth.SID), "st-old") {
		t.Fatalf("stale service rows must be replaced")
	}
	// Bootstrap rows survive the merge.
	if cookieValue(sa.Auth.Cookies, "passToken") != "pt" {
		t.Fatalf("bootstrap rows must survive the merge")
	}
}

func TestRenewCookieSessionExchangePersists(t *testing.T) {
	srv := withPassportStub(t)
	swapServiceLoginBase(t, srv.URL)
	var persistedName string
	origPersist := hostAuthPersistFn
	hostAuthPersistFn = func(name string, raw []byte) error { persistedName = name; return nil }
	defer func() { hostAuthPersistFn = origPersist }()

	sa := &storedAuth{
		Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
			{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
			{Name: "userId", Value: "u9", Domain: ".account.xiaomi.com"},
			{Name: "cUserId", Value: "cu", Domain: ".account.xiaomi.com"},
		}},
		Account: mimoAccount{UID: "u9"},
	}
	if !renewCookieSession(sa) {
		t.Fatalf("renew must mint successfully")
	}
	if persistedName != authFileNameFor(sa) {
		t.Fatalf("renew must persist under the canonical name, got %q", persistedName)
	}
	if cookieValue(sa.Auth.Cookies, "serviceToken") == "" {
		t.Fatalf("renew must leave a usable ticket")
	}
}

func TestHandleExecExecuteCookieLane302RetryWithMint(t *testing.T) {
	// Full ladder over real HTTP: chat 302 (stale ticket) → real
	// serviceLogin→STS mint against the passport stub → retry on the freshly
	// routed sgp endpoint with the assembled ticket header.
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if upstreamCalls == 1 {
			w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin?sid=mimosgp")
			w.WriteHeader(http.StatusFound)
			return
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "serviceToken=st-new") {
			t.Errorf("retry must carry the freshly minted ticket, got %q", got)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("retry must not carry Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"2","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	passport := withPassportStub(t)
	swapServiceLoginBase(t, passport.URL)
	origSgp, origCn := regionBases["sgp"], regionBases["cn"]
	regionBases["sgp"] = upstream.URL + "/api"
	regionBases["cn"] = upstream.URL + "/api" // first attempt (pinned cn) hits the stub too
	defer func() { regionBases["sgp"], regionBases["cn"] = origSgp, origCn }()
	origPersist := hostAuthPersistFn
	hostAuthPersistFn = func(name string, raw []byte) error { return nil }
	defer func() { hostAuthPersistFn = origPersist }()

	// Stale ticket + fresh bootstrap rows, region pinned cn so the first
	// attempt misses the (sgp-routed) test upstream.
	sa := &storedAuth{
		Auth: mimoTokens{Lane: laneCookie, Region: "cn", Cookies: []mimoCookie{
			{Name: "serviceToken", Value: "st-old", Domain: ".xiaomimimo.com", Path: "/"},
			{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com", Path: "/"},
			{Name: "userId", Value: "u9", Domain: ".account.xiaomi.com", Path: "/"},
			{Name: "cUserId", Value: "cu", Domain: ".account.xiaomi.com", Path: "/"},
		}},
		Account: mimoAccount{UID: "u9"},
	}
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
	if upstreamCalls != 2 {
		t.Fatalf("expected 302 → mint → retry (2 upstream calls), got %d", upstreamCalls)
	}
}

func TestExchangePassTokenExpiredShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "&&&START&&&"+`{"code":1010,"message":"session expired"}`+"&&&END&&&")
	}))
	defer srv.Close()
	swapServiceLoginBase(t, srv.URL)
	sa := &storedAuth{
		Auth: mimoTokens{Lane: laneCookie, Cookies: []mimoCookie{
			{Name: "passToken", Value: "pt", Domain: ".account.xiaomi.com"},
			{Name: "userId", Value: "u9", Domain: ".account.xiaomi.com"},
		}},
		Account: mimoAccount{UID: "u9"},
	}
	if exchangeForCredential(sa) {
		t.Fatalf("dead passToken must fail the exchange")
	}
	if sa.Auth.SID != "" || sa.Auth.ExchangedAt != 0 {
		t.Fatalf("failed exchange must not stamp the credential")
	}
}

// TestVersionMatchesVERSIONFile locks the version surfaces together: CI
// releases build WITHOUT -X injection, so main.go's var must track the
// VERSION file (trae v0.12.86 shipped self-reporting 0.12.56 — same drift
// class, repo lesson 2026-09-23).
func TestVersionMatchesVERSIONFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Skipf("VERSION file unavailable: %v", err)
	}
	if v := strings.TrimSpace(string(raw)); v != version {
		t.Fatalf("main.go var version %q drifts from the VERSION file %q — keep them in lockstep", version, v)
	}
}

// The stream paths must ride the same renew ladder as execute: a stale
// service ticket answers 302 → re-mint → retry (deep-audit 2026-09-23: the
// stream paths previously bypassed sendChatWithCookieRetry entirely).
func TestHandleExecStreamCookie302RetryWithMint(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, `data: [DONE]`+"\n\n")
	}))
	defer upstream.Close()
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()
	origRenew := renewCookieSessionFn
	renewCookieSessionFn = func(*storedAuth) bool { return true }
	defer func() { renewCookieSessionFn = origRenew }()

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
		t.Fatalf("stream after 302 re-mint: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 302 → renew → retry (2 calls), got %d", calls)
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", string(resp))
	}
	var sr streamResponse
	if err := json.Unmarshal(env.Result, &sr); err != nil {
		t.Fatalf("stream response: %v", err)
	}
	if len(sr.Chunks) != 1 || !strings.Contains(string(sr.Chunks[0].Payload), "ok") {
		t.Fatalf("retry chunks wrong: %s", string(env.Result))
	}
}

// A fault that survives the ladder's one retry must surface the self-heal
// guidance instead of an "empty stream" riddle (the 302 body is an empty
// login redirect the SSE parser cannot make sense of).
func TestCollectUpstreamStreamFaultAfterRetrySurfacesGuidance(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	origBase := regionBases[regionCN]
	regionBases[regionCN] = upstream.URL + "/api"
	defer func() { regionBases[regionCN] = origBase }()
	origRenew := renewCookieSessionFn
	renewCookieSessionFn = func(*storedAuth) bool { return true }
	defer func() { renewCookieSessionFn = origRenew }()

	sa := cookieCred(t)
	_, _, err := collectUpstreamStream(sa, routeFor(sa), `{"model":"mimo-flash","messages":[]}`, false)
	if err == nil || !strings.Contains(err.Error(), "re-mint attempted") {
		t.Fatalf("want self-heal guidance error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly one retry (2 calls), got %d", calls)
	}
}
