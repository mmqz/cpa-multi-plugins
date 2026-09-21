// signing.go implements Client Request Signing V4 — mirrors the ZCode 3.12.3
// ClientRequestSigningV4Signer (TriDefender/zcode-api src/proxy/client-signing.ts).
//
// All four signed message templates join their fields with literal NEWLINES
// (verified byte-exact by the ZCode Proxy project; space-joined variants are
// rejected upstream):
//
//	handshake HMAC   "get_sign_key\n{apiKeyId}\n{ts}\n{nonce}"
//	business Ed25519 "{apiKeyId}\n{ts}\n{clientVersion}\n{sessionId}\n{nonce}"
//	PoW challenge    sha256("{apiKeyId}\nzcode\n{sessionId}\n{ts}") hex[:32]
//	PoW answer       sha256("{challenge}\n{answer}")
//
// Signatures are base64, never hex. Keys derive via HKDF-SHA256:
//
//	hmac key     = HKDF(secret, salt="WD_CLIENT_SIGN_KDF_SALT", info="getSignKey_hmac")
//	aes key      = HKDF(secret, salt="WD_CLIENT_SIGN_KDF_SALT", info="ed25519_priv")
//
// The handshake POSTs {origin}/api/paas/c1f3a7e2/v2/client with
// `Authorization: {apiKeyId}.{apiKeySecret}` and body
// {apiKey, nonce, sig, ts}; the response `data.privateCipher` is an AES-GCM
// blob (iv = first 12 bytes, AAD = apiKeyId, tag 128) encrypting a PKCS8
// Ed25519 private key.
//
// Everything here is fail-open, matching the client: gate disabled/unreachable
// → unsigned; handshake failure → unsigned; two consecutive 401 VERIFY_*
// rejections → permanent bypass for that (origin, credential) pair. The
// zcode-plan / off-peak paths are never signed. Credentials without the
// `{apiKeyId}.{apiKeySecret}` separator are silently skipped.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf" //nolint:staticcheck // Go 1.26 stdlib hkdf
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	signGatePath      = "/api/v1/agent/configs"
	signHandshakePath = "/api/paas/c1f3a7e2/v2/client"
	signAppID         = "zcode"
	signPowBits       = 8
	signNonceBytes    = 16
	signPowNonceBytes = 12
	signKDFSalt       = "WD_CLIENT_SIGN_KDF_SALT"
)

const (
	signKDFInfoHMAC    = "getSignKey_hmac"
	signKDFInfoEd25519 = "ed25519_priv"
	signHandshakeMeth  = "get_sign_key"
	signGateTTL        = 1 * time.Hour
	// Negative-cache cooldowns after a failed/unavailable gate probe.
	signGateFailureCooldown     = 60 * time.Second
	signGateUnavailableCooldown = 30 * time.Second
	signGateTimeout             = 15 * time.Second
	signHandshakeTimeout        = 10 * time.Second

	verifySignatureInvalid = "VERIFY_SIGNATURE_INVALID"
	verifyAPIKeyExpired    = "VERIFY_APIKEY_EXPIRED"
)

// signingHeaderNames are the proxy-generated V4 headers — inbound copies must
// never reach the upstream (spoofed values would either fail verification or
// silently disable plugin signing via the existing-header guard).
var signingHeaderNames = map[string]bool{
	"x-client-ts": true, "x-client-version": true, "x-client-sig": true,
	"x-client-nonce": true, "x-app-id": true, "x-client-pow": true,
	"x-client-sign-verified": true,
}

// signerState is the per-(origin, credential) signing state.
type signerState struct {
	gateEnabled   bool
	gateExpiresAt time.Time
	gateNegUntil  time.Time
	privKey       ed25519.PrivateKey
	bypass        bool
}

// signingManager owns the gate + handshake caches across requests.
type signingManager struct {
	origin string
	nowFn  func() time.Time

	mu     sync.Mutex
	states map[string]*signerState
	noted  map[string]bool
}

var signer = newSigningManager("https://zcode.z.ai")

func newSigningManager(origin string) *signingManager {
	return &signingManager{
		origin: strings.TrimRight(origin, "/"),
		nowFn:  time.Now,
		states: map[string]*signerState{},
		noted:  map[string]bool{},
	}
}

func (m *signingManager) stateFor(key string) *signerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[key]
	if !ok {
		st = &signerState{}
		m.states[key] = st
	}
	return st
}

func (m *signingManager) noteOnce(key, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.noted[key] {
		return
	}
	m.noted[key] = true
}

// parseSigningCredential splits `{apiKeyId}.{apiKeySecret}` (exactly one dot).
func parseSigningCredential(credential string) (apiKeyID, apiKeySecret string, ok bool) {
	dot := strings.Index(credential, ".")
	if dot <= 0 || dot != strings.LastIndex(credential, ".") {
		return "", "", false
	}
	apiKeyID = credential[:dot]
	apiKeySecret = credential[dot+1:]
	if strings.TrimSpace(apiKeyID) == "" || strings.TrimSpace(apiKeySecret) == "" {
		return "", "", false
	}
	return apiKeyID, apiKeySecret, true
}

// hkdfBytes derives 32 bytes via HKDF-SHA256 with the client's salt/info.
func hkdfBytes(secret []byte, info string) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, []byte(signKDFSalt), info, 32)
}

// handshakeSignature computes base64(HMAC-SHA256(HKDF(secret, getSignKey_hmac),
// "get_sign_key\n{id}\n{ts}\n{nonce}")).
func handshakeSignature(secret, apiKeyID, ts, nonce string) (string, error) {
	bits, err := hkdfBytes([]byte(secret), signKDFInfoHMAC)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, bits)
	message := "get_sign_key\n" + apiKeyID + "\n" + ts + "\n" + nonce
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// decryptSigningPrivateKey decrypts `privateCipher` into an Ed25519 key.
func decryptSigningPrivateKey(apiKeyID, secret, privateCipher string) (ed25519.PrivateKey, error) {
	cipherBlob, err := base64.StdEncoding.DecodeString(privateCipher)
	if err != nil {
		return nil, fmt.Errorf("privateCipher base64: %w", err)
	}
	if len(cipherBlob) <= 12+16 {
		return nil, errors.New("privateCipher is too short")
	}
	aesBits, err := hkdfBytes([]byte(secret), signKDFInfoEd25519)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aesBits)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, cipherBlob[:12], cipherBlob[12:], []byte(apiKeyID))
	if err != nil {
		return nil, fmt.Errorf("privateCipher decrypt: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, fmt.Errorf("privateCipher pkcs8: %w", err)
	}
	edKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("privateCipher: key is not Ed25519")
	}
	return edKey, nil
}

// performHandshake exchanges the credential for the signing private key.
func (m *signingManager) performHandshake(apiKeyID, apiKeySecret string) (ed25519.PrivateKey, error) {
	ts := strconv.FormatInt(m.nowFn().UnixMilli(), 10)
	nonce := hex.EncodeToString(cryptoRand(make([]byte, signNonceBytes)))
	sig, err := handshakeSignature(apiKeySecret, apiKeyID, ts, nonce)
	if err != nil {
		return nil, err
	}
	credential := apiKeyID + "." + apiKeySecret
	body, _ := json.Marshal(map[string]string{
		"apiKey": credential,
		"nonce":  nonce,
		"sig":    sig,
		"ts":     ts,
	})
	req, err := http.NewRequest(http.MethodPost, m.origin+signHandshakePath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", credential)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("handshake_http_%d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("handshake parse: %w", err)
	}
	if env.Code == 500 {
		return nil, errors.New("handshake_server_500")
	}
	if env.Code != 200 {
		return nil, fmt.Errorf("handshake_rejected: %s", env.Msg)
	}
	if env.Data.PrivateCipher == "" {
		return nil, errors.New("handshake_omitted_privateCipher")
	}
	return decryptSigningPrivateKey(apiKeyID, apiKeySecret, env.Data.PrivateCipher)
}

// cryptoRand fills buf with cryptographically secure random bytes (crypto/rand).
func cryptoRand(buf []byte) []byte {
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is fatal for signing material; a zeroed nonce
		// would be a fingerprint - panic is the honest failure mode.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return buf
}

// createProofOfWork solves the 8-leading-zero-bits challenge:
// seed = sha256("{id}\nzcode\n{sessionId}\n{ts}") hex[:32]; candidate =
// 12-byte-hex nonce + 8-hex counter with sha256("{seed}\n{candidate}") having
// signPowBits leading zero bits.
func createProofOfWork(apiKeyID, sessionID, ts string) (string, error) {
	seedDigest := sha256.Sum256([]byte(apiKeyID + "\n" + signAppID + "\n" + sessionID + "\n" + ts))
	seed := hex.EncodeToString(seedDigest[:])[:32]
	nonce := hex.EncodeToString(cryptoRand(make([]byte, signPowNonceBytes)))
	for counter := 0; counter <= 0xFFFFFFFF; counter++ {
		candidate := nonce + fmt.Sprintf("%08x", counter)
		digest := sha256.Sum256([]byte(seed + "\n" + candidate))
		if hasLeadingZeroBits(digest[:], signPowBits) {
			return candidate, nil
		}
	}
	return "", errors.New("unable to solve client request proof of work")
}

// hasLeadingZeroBits reports whether the first `bits` bits of b are zero.
func hasLeadingZeroBits(b []byte, bits int) bool {
	fullBytes := bits / 8
	for i := 0; i < fullBytes; i++ {
		if b[i] != 0 {
			return false
		}
	}
	remainder := bits % 8
	if remainder == 0 {
		return true
	}
	mask := byte(255<<(8-remainder)) & 255
	return (b[fullBytes] & mask) == 0
}

// signBusinessMessage computes base64(Ed25519(priv,
// "{id}\n{ts}\n{ver}\n{sessionId}\n{nonce}")).
func signBusinessMessage(priv ed25519.PrivateKey, apiKeyID, ts, appVersion, sessionID, nonce string) string {
	message := apiKeyID + "\n" + ts + "\n" + appVersion + "\n" + sessionID + "\n" + nonce
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(message)))
}

// gateEnabled probes GET {origin}/api/v1/agent/configs (with the LLM identity
// header set + x-api-key) for data.codingPlanSignature.enable. Success caches
// for signGateTTL; network failure / unavailable responses get short negative
// caches so the worst-case per-request latency stays one probe per window.
func (m *signingManager) gateEnabled(st *signerState, credential string) bool {
	now := m.nowFn()
	if now.Before(st.gateExpiresAt) {
		return st.gateEnabled
	}
	if now.Before(st.gateNegUntil) {
		return false
	}
	enabled, unavailable := m.probeGate(credential)
	st.gateEnabled = enabled
	if unavailable {
		st.gateNegUntil = now.Add(signGateUnavailableCooldown)
	} else if enabled {
		st.gateExpiresAt = now.Add(signGateTTL)
		st.gateNegUntil = time.Time{}
	} else {
		st.gateExpiresAt = now.Add(signGateTTL)
		st.gateNegUntil = time.Time{}
	}
	return st.gateEnabled
}

func (m *signingManager) probeGate(credential string) (enabled, unavailable bool) {
	req, err := http.NewRequest(http.MethodGet, m.origin+signGatePath, nil)
	if err != nil {
		return false, true
	}
	for k, v := range buildLlmIdentityHeaders(zcodeIdentity()) {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-api-key", credential)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return false, true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, true
	}
	var parsed struct {
		Code int `json:"code"`
		Data struct {
			CodingPlanSignature *struct {
				Enable bool `json:"enable"`
			} `json:"codingPlanSignature"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil || parsed.Code != 0 {
		return false, true
	}
	if parsed.Data.CodingPlanSignature == nil {
		return false, false // feature absent = disabled
	}
	return parsed.Data.CodingPlanSignature.Enable, false
}

// isVerifyFailure reports whether the 401 body is a signing rejection the
// client retries on.
func isVerifyFailure(status int, body []byte) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	verdict := func(v any) bool {
		s, _ := v.(string)
		return s == verifySignatureInvalid || s == verifyAPIKeyExpired
	}
	if verdict(parsed["msg"]) || verdict(parsed["reason"]) {
		return true
	}
	if data, ok := parsed["data"].(map[string]any); ok && verdict(data["reason"]) {
		return true
	}
	if errObj, ok := parsed["error"].(map[string]any); ok {
		return verdict(errObj["reason"]) || verdict(errObj["message"])
	}
	return false
}

// signRequest decides whether this request signs, and if so appends the V4
// header set. Never throws: any ineligibility returns the input headers.
func (m *signingManager) signRequest(req *http.Request, sa *storedAuth) {
	if req.URL == nil || req.URL.Scheme != "https" {
		return
	}
	if isUnsignedChatPath(req.URL.Path) {
		return
	}
	if req.Header.Get("x-client-sig") != "" {
		return
	}
	stateKey := req.URL.Host + "\n" + sa.Auth.AccessToken
	st := m.stateFor(stateKey)
	if st.bypass {
		return
	}
	apiKeyID, apiKeySecret, ok := parseSigningCredential(sa.Auth.AccessToken)
	if !ok {
		return
	}
	sessionID := sessionIdFromRequest(req)
	if sessionID == "" {
		return
	}
	if !m.gateEnabled(st, sa.Auth.AccessToken) {
		return
	}
	priv, err := m.performHandshake(apiKeyID, apiKeySecret)
	if err != nil {
		return // fail-open: send unsigned
	}
	ts := strconv.FormatInt(m.nowFn().UnixMilli(), 10)
	nonce := hex.EncodeToString(cryptoRand(make([]byte, signNonceBytes)))
	pow, err := createProofOfWork(apiKeyID, sessionID, ts)
	if err != nil {
		return
	}
	sig := signBusinessMessage(priv, apiKeyID, ts, zcodeIdentity().appVersion, sessionID, nonce)
	req.Header.Set("X-Client-Ts", ts)
	req.Header.Set("X-Client-Version", zcodeIdentity().appVersion)
	req.Header.Set("X-Client-Sig", sig)
	req.Header.Set("X-Client-Nonce", nonce)
	req.Header.Set("X-App-Id", signAppID)
	req.Header.Set("X-Client-Pow", pow)
}

// noteVerifyRejection reacts to a 401 VERIFY_* response after a signed send:
// invalidate the cached key and re-sign once; a second VERIFY rejection makes
// the bypass permanent for this (origin, credential) pair. Returns the
// action the caller took ("resign" / "bypass" / "").
func (m *signingManager) noteVerifyRejection(req *http.Request, sa *storedAuth, second bool) string {
	if req.URL == nil {
		return ""
	}
	stateKey := req.URL.Host + "\n" + sa.Auth.AccessToken
	st := m.stateFor(stateKey)
	if second {
		st.bypass = true
		return "bypass"
	}
	st.privKey = nil
	return "resign"
}

// isUnsignedChatPath reports paths the client never signs (start-plan /
// off-peak gateways). Trailing slashes are normalized.
func isUnsignedChatPath(path string) bool {
	p := strings.TrimRight(path, "/")
	switch p {
	case "/api/v1/zcode-plan/anthropic/v1/messages",
		"/api/v1/zcode-plan/chat/completions",
		"/api/v1/off-peak/anthropic/v1/messages":
		return true
	}
	return false
}

// sendChatWithSigning is the client's full signing retry ladder applied to
// one coding-plan chat call:
//
//	signed → on 401 VERIFY: re-handshake + re-sign → on second 401 VERIFY:
//	permanent bypass + one unsigned send.
//
// buildReq must return a FRESH request each call (the previous attempt's
// body has been consumed by the bridge). A 401 response body is always
// drained and rewrapped in-memory so the caller's ReadAll contract holds.
// When signing is not applicable the first send is as-is — identical to the
// disabled-manager case.
func sendChatWithSigning(sa *storedAuth, buildReq func() (*http.Request, error)) (*hostHTTPStream, int, http.Header, error) {
	req, err := buildReq()
	if err != nil {
		return nil, 0, nil, err
	}
	signer.signRequest(req, sa)
	stream, status, hdrs, err := hostHTTPDoStream(req)
	if err != nil {
		return stream, status, hdrs, err
	}
	if status != http.StatusUnauthorized {
		return stream, status, hdrs, err
	}
	// Drain the 401 body (small JSON) so we can inspect it and rewrap.
	payload, _ := io.ReadAll(newHostStreamReader(stream))
	stream.Close()
	rewrap := func() *hostHTTPStream {
		return &hostHTTPStream{direct: payload}
	}
	if !isVerifyFailure(status, payload) {
		return rewrap(), status, hdrs, nil
	}
	// First VERIFY rejection: invalidate the cached signing key, re-sign, resend.
	signer.noteVerifyRejection(req, sa, false)
	req2, err := buildReq()
	if err != nil {
		return rewrap(), status, hdrs, nil
	}
	signer.signRequest(req2, sa)
	stream2, status2, hdrs2, err2 := hostHTTPDoStream(req2)
	if err2 != nil || status2 != http.StatusUnauthorized {
		return stream2, status2, hdrs2, err2
	}
	payload2, _ := io.ReadAll(newHostStreamReader(stream2))
	stream2.Close()
	rewrap2 := func() *hostHTTPStream {
		return &hostHTTPStream{direct: payload2}
	}
	if !isVerifyFailure(status2, payload2) {
		return rewrap2(), status2, hdrs2, nil
	}
	// Second VERIFY rejection: bypass signing permanently for this
	// (origin, credential) pair and send one unsigned attempt.
	signer.noteVerifyRejection(req2, sa, true)
	req3, err := buildReq()
	if err != nil {
		return rewrap2(), status2, hdrs2, nil
	}
	return hostHTTPDoStream(req3)
}
