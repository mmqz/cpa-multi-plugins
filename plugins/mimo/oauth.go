// oauth.go implements the sk lane's auth flow — a faithful port of the
// official CLI's browser OAuth (MiMo-Code packages/opencode/src/plugin/
// mimo.ts, verified byte-for-byte in docs/MIMO_AUTH.md §5):
//
//  1. Generate an X25519 key pair; publish the public key (SPKI DER →
//     base64url) as the `pk` authorize parameter.
//  2. Start a loopback callback server on a random port (the CLI does the
//     same in-process, 5-minute TTL).
//  3. Open {platform}/authorize?pk=…&redirect_uri=http://localhost:<port>/
//     &kn=mimocode&key_name=mimo-code-cli-key-<8hex>.
//  4. The platform redirects back with ?u=<blob>:
//     base64url(ephemeralPub(32B) ‖ nonce(12B) ‖ ciphertext ‖ tag(16B)),
//     AES-256-GCM with key = SHA256(ECDH(x25519)) → {sk, uid, url}.
//  5. The sk is a permanent API credential (the CLI stores
//     {type:"api", key:sk, metadata:{uid, base_url:url}} in auth.json and
//     never refreshes) — so AuthRefresh is identity-only here too.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// spkiX25519Prefix is the DER SPKI header for an X25519 public key; the
// platform's blob carries the raw 32-byte point which the CLI rebuilds into
// SPKI form exactly like this (mimo.ts decrypt()).
const spkiX25519Prefix = "302a300506032b656e032100"

// decryptOAuthBlobFn is an indirection point so tests can exercise the login
// flow without real key material.
var decryptOAuthBlobFn = decryptOAuthBlob

// decodeBase64URL tolerates padded and unpadded base64url (Node's
// Buffer.from(x, "base64url") accepts both).
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// generateX25519 returns (spkiBase64url, rawPrivate) for the authorize URL.
func generateX25519() (string, []byte, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	pub := priv.PublicKey().Bytes() // raw 32-byte point
	der := make([]byte, 0, len(spkiX25519Prefix)/2+len(pub))
	for i := 0; i < len(spkiX25519Prefix); i += 2 {
		b, err := hex.DecodeString(spkiX25519Prefix[i : i+2])
		if err != nil {
			return "", nil, err
		}
		der = append(der, b...)
	}
	der = append(der, pub...)
	return base64.RawURLEncoding.EncodeToString(der), priv.Bytes(), nil
}

// decryptOAuthBlob decrypts the platform's `u` callback parameter.
func decryptOAuthBlob(privateKeyRaw []byte, encryptedBase64 string) (oauthResult, error) {
	encrypted, err := decodeBase64URL(encryptedBase64)
	if err != nil {
		return oauthResult{}, fmt.Errorf("callback blob decode: %w", err)
	}
	// Format: ephemeralPublicKey(32) + nonce(12) + ciphertext + tag(16).
	if len(encrypted) < 32+12+16+1 {
		return oauthResult{}, fmt.Errorf("callback blob too short (%d bytes)", len(encrypted))
	}
	ephemeralPub := encrypted[:32]
	nonce := encrypted[32:44]
	ciphertextAndTag := encrypted[44:]
	tag := ciphertextAndTag[len(ciphertextAndTag)-16:]
	ciphertext := ciphertextAndTag[:len(ciphertextAndTag)-16]

	priv, err := ecdh.X25519().NewPrivateKey(privateKeyRaw)
	if err != nil {
		return oauthResult{}, fmt.Errorf("private key: %w", err)
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(ephemeralPub)
	if err != nil {
		return oauthResult{}, fmt.Errorf("ephemeral public key: %w", err)
	}
	shared, err := priv.ECDH(ephemeral)
	if err != nil {
		return oauthResult{}, fmt.Errorf("ecdh: %w", err)
	}
	key := sha256.Sum256(shared)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return oauthResult{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return oauthResult{}, err
	}
	plain, err := gcm.Open(nil, nonce, append(append([]byte{}, ciphertext...), tag...), nil)
	if err != nil {
		return oauthResult{}, fmt.Errorf("decrypt: %w", err)
	}
	var result oauthResult
	if err := json.Unmarshal(plain, &result); err != nil {
		return oauthResult{}, fmt.Errorf("payload parse: %w", err)
	}
	if strings.TrimSpace(result.SK) == "" {
		return oauthResult{}, fmt.Errorf("payload missing sk")
	}
	return result, nil
}

// newKeyName mirrors the CLI's key naming (mimo.ts getKeyName): a stable
// per-install name persisted across logins.
func newKeyName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "mimo-code-cli-key-" + hex.EncodeToString(b)
}

// -----------------------------------------------------------------------------
// Loopback callback server
// -----------------------------------------------------------------------------

type loopbackServer struct {
	server   *http.Server
	listener net.Listener
	port     int
	mu       sync.Mutex
	closed   bool
}

// startLoopbackServer binds 127.0.0.1 on a random port and serves the OAuth
// redirect target until shutdown. On ?u= it decrypts and forwards the result
// to the flow's channel, then responds with the platform's own interstitial
// redirect (status=success/error — CLI parity).
func startLoopbackServer(privKey []byte, result chan oauthResult, platform string) (*loopbackServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listen: %w", err)
	}
	ls := &loopbackServer{listener: listener, port: listener.Addr().(*net.TCPAddr).Port}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("u")
		if u == "" {
			http.Redirect(w, r, platform+"/authorize/callback?status=error&message=missing_data", http.StatusFound)
			return
		}
		result0, derr := decryptOAuthBlobFn(privKey, u)
		if derr != nil {
			http.Redirect(w, r, platform+"/authorize/callback?status=error&message=decrypt_failed", http.StatusFound)
			return
		}
		// Deliver before redirecting so the poll never races the browser page.
		select {
		case result <- result0:
		default:
		}
		http.Redirect(w, r, platform+"/authorize/callback?status=success", http.StatusFound)
	})
	ls.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = ls.server.Serve(listener)
	}()
	return ls, nil
}

// shutdown stops the loopback server exactly once.
func (ls *loopbackServer) shutdown() {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	ls.closed = true
	ls.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ls.server.Shutdown(ctx)
}

func (ls *loopbackServer) redirectURI() string {
	return fmt.Sprintf("http://localhost:%d/", ls.port)
}

// shutdown stops the flow's loopback server (nil-safe for flows that never
// got a server).
func (lc *loginCtx) shutdown() {
	if lc == nil {
		return
	}
	lc.callback.shutdown()
}

// buildAuthorizeURL mirrors the CLI's buildAuthorizeUrl (URLSearchParams →
// %XX escaping, same param set and order).
func buildAuthorizeURL(platform, publicKeyB64, redirectURI, keyName string) string {
	q := make([]string, 0, 4)
	for _, kv := range [][2]string{
		{"pk", publicKeyB64},
		{"redirect_uri", redirectURI},
		{"kn", "mimocode"},
		{"key_name", keyName},
	} {
		q = append(q, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	return platform + "/authorize?" + strings.Join(q, "&")
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

// handleStartLogin implements AuthProvider.StartLogin: generate the key pair,
// start the loopback callback server, and hand the authorize URL to the host.
func handleStartLogin(raw []byte) ([]byte, error) {
	_ = raw // AuthLoginStartRequest carries provider/host; nothing needed here.
	platform := loadedPlatformURL()
	pkB64, privRaw, err := generateX25519()
	if err != nil {
		return nil, fmt.Errorf("x25519 generate: %w", err)
	}
	keyName := newKeyName()
	result := make(chan oauthResult, 1)
	ls, err := startLoopbackServer(privRaw, result, platform)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	state := fmt.Sprintf("mimo-%d", now.UnixNano())
	loginStates.Store(state, &loginCtx{
		keyName:   keyName,
		privKey:   privRaw,
		callback:  ls,
		result:    result,
		expires:   now.Add(loginTTL),
		startedAt: now.UnixNano(),
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       buildAuthorizeURL(platform, pkB64, ls.redirectURI(), keyName),
		State:     state,
		ExpiresAt: now.Add(loginTTL).UTC(),
		Metadata: map[string]any{
			// v0.2.5: the prompt now carries the paste-to-complete
			// fallback — remote deployments cannot receive the
			// localhost redirect at all, and without this hint the
			// login just pends until TTL (issue report 2026-09-26).
			"prompt": "在打开的页面中登录小米账号并授权 MiMo Code 密钥（回调直达本机，完成后自动继续）。授权记录名为 " + keyName + "。远程部署浏览器跳不回本机时：复制失败页地址栏的完整链接（http://localhost:…/?u=…），打开 <宿主地址>/v0/resource/plugins/mimo/oauth_submit 粘贴提交即可完成登录。",
		},
	})
}

// handlePollLogin implements AuthProvider.PollLogin: one round per call — the
// host drives the cadence.
func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		lc.shutdown()
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}
	select {
	case res := <-lc.result:
		lc.shutdown()
		loginStates.Delete(state)
		sa := buildStoredAuthFromOAuth(res)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status: pluginapi.AuthLoginStatusSuccess,
			Auth:   toAuthData(sa, false),
		})
	default:
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待浏览器完成授权",
	})
}

// buildStoredAuthFromOAuth maps a decrypted {sk, uid, url} onto storedAuth.
// The uid doubles as the account id; when absent (platform quirk) a stable
// pseudo-id is derived from the key material — the same trick the CLI's
// metadata relies on, keyed by sk hash.
func buildStoredAuthFromOAuth(res oauthResult) *storedAuth {
	uid := strings.TrimSpace(res.UID)
	if uid == "" {
		uid = "u-" + sha256hex8(res.SK)
	}
	return &storedAuth{
		Auth: mimoTokens{
			Lane:    laneKey,
			SK:      strings.TrimSpace(res.SK),
			BaseURL: strings.TrimRight(strings.TrimSpace(res.URL), "/"),
			UID:     uid,
		},
		Account: mimoAccount{UID: uid},
	}
}

// handleRefreshAuth implements AuthProvider.Refresh. The platform-issued sk
// is a permanent credential (the CLI never refreshes it; a 401 from the
// gateway means re-login). Return the stored credential unchanged so the
// host refreshes its metadata without altering tokens.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: toAuthData(sa, parseDisabledFromAuthJSON(req.StorageJSON)),
	})
}
