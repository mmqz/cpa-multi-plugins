// cookies.go — Chromium cookie-store plumbing shared by every platform:
// SQLite reading (via a temp copy so a running desktop's lock never blocks
// us), version-prefix dispatch, the GCM/CBC primitives, and WebKit epoch
// conversion. Platform-specific key material lives in cookies_windows.go /
// cookies_linux.go / cookies_darwin.go.
//
// The adopted lane reproduces the desktop's own cookie lane (the Electron
// session.fromPartition("persist:xiaomi-account") jar): only *.xiaomimimo.com
// cookies are collected — .xiaomi.com rows (passToken/userId on the account
// domain) are login-side state the upstream hosts never see and must NOT be
// replayed to xiaomimimo.com (docs/MIMO_AUTH.md §3.3).
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// userDataRootFor walks a Cookies-store path back to the Electron user-data
// dir (the Local State lookup root). Chromium 96+ / Electron 15+ lay the
// store out as <user-data>/Partitions/<name>/Network/Cookies; older builds
// used <user-data>/Partitions/<name>/Cookies. Paths outside a Partitions
// tree (custom cookie_paths config) fall back to the two-level parent, where
// a wrong guess only surfaces as a loud Local State error.
func userDataRootFor(dbPath string) string {
	norm := strings.ReplaceAll(filepath.ToSlash(dbPath), "\\", "/")
	parts := strings.Split(norm, "/")
	for i, p := range parts {
		if i > 0 && p == "Partitions" {
			return filepath.FromSlash(strings.Join(parts[:i], "/"))
		}
	}
	return filepath.Dir(filepath.Dir(dbPath))
}

// readCookiesFromDB copies the Chromium Cookies store (plus its WAL/journal
// sidecars) to a temp file and returns every decryptable *.xiaomimimo.com
// cookie row. The copy sidesteps the running desktop's exclusive lock and
// hot-journal replay.
func readCookiesFromDB(dbPath string) ([]mimoCookie, string, error) {
	userDataDir := userDataRootFor(dbPath)
	tmp, err := os.MkdirTemp("", "mimo-adopt-")
	if err != nil {
		return nil, "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	tmpDB := filepath.Join(tmp, "Cookies")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, "", fmt.Errorf("copy cookies db: %w", err)
	}
	for _, side := range []string{"Cookies-wal", "Cookies-journal"} {
		if _, err := os.Stat(dbPath + side[len("Cookies"):]); err == nil {
			_ = copyFile(dbPath+side[len("Cookies"):], tmpDB+side[len("Cookies"):])
		}
	}
	rows, err := queryCookieRows(tmpDB)
	if err != nil {
		return nil, "", fmt.Errorf("read cookies: %w", err)
	}
	out := make([]mimoCookie, 0, len(rows))
	seen := map[string]struct{}{}
	for _, r := range rows {
		if !isMimoUpstreamHost(r.HostKey) {
			continue
		}
		value := r.Value
		if value == "" && len(r.Encrypted) > 0 {
			plain, derr := decryptChromiumCookie(r.Encrypted, userDataDir)
			if derr != nil {
				// Unreadable rows are expected on platforms we don't support
				// (macOS keychain) or app-bound entries; skip, don't fail.
				continue
			}
			value = string(plain)
		}
		if value == "" {
			continue
		}
		key := r.Name + "|" + r.HostKey + "|" + r.Path
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, mimoCookie{
			Name:     r.Name,
			Value:    value,
			Domain:   r.HostKey,
			Path:     r.Path,
			Secure:   r.Secure,
			HTTPOnly: r.HTTPOnly,
			Expires:  webkitToUnix(r.ExpiresUTC),
		})
	}
	if len(out) == 0 {
		return nil, userDataDir, fmt.Errorf("no usable *.xiaomimimo.com cookies in %s", filepath.Base(dbPath))
	}
	return out, userDataDir, nil
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o600)
}

// cookieRow is one raw `cookies` table row.
type cookieRow struct {
	Name       string
	Value      string
	Encrypted  []byte
	HostKey    string
	Path       string
	Secure     bool
	HTTPOnly   bool
	ExpiresUTC int64
}

// isMimoUpstreamHost reports whether the cookie would be attached by a real
// jar to a *.xiaomimimo.com request (domain cookies carry a leading dot).
func isMimoUpstreamHost(hostKey string) bool {
	h := hostKey
	if len(h) > 0 && h[0] == '.' {
		h = h[1:]
	}
	return h == "xiaomimimo.com" || len(h) > len(".xiaomimimo.com") && h[len(h)-len(".xiaomimimo.com"):] == ".xiaomimimo.com"
}

// webkitToUnix converts Chromium's WebKit epoch (µs since 1601-01-01) to
// unix seconds. 0 (session cookies) passes through.
func webkitToUnix(webkit int64) int64 {
	if webkit <= 0 {
		return 0
	}
	const webkitEpochDiff = 11644473600 // seconds between 1601-01-01 and 1970-01-01
	return webkit/1_000_000 - webkitEpochDiff
}

// -----------------------------------------------------------------------------
// Version dispatch + primitives
// -----------------------------------------------------------------------------

// decryptChromiumCookie dispatches on Chromium's os_crypt version prefix.
// The encrypted_value layout is: "v10"/"v11"/"v20" magic + platform payload.
func decryptChromiumCookie(enc []byte, userDataDir string) ([]byte, error) {
	if len(enc) >= 3 && enc[0] == 'v' && enc[1] == '1' && (enc[2] == '0' || enc[2] == '1') {
		version := string(enc[:3])
		return decryptPlatformV10(enc[3:], userDataDir, version)
	}
	if len(enc) >= 3 && enc[0] == 'v' && enc[1] == '2' && enc[2] == '0' {
		// v20 = app-bound encryption (Chrome 127+ style IElevator COM); the
		// desktop's Electron build ships v10 — a v20 row means a foreign
		// store, not ours.
		return nil, fmt.Errorf("v20 app-bound cookies are not supported")
	}
	// No prefix: ancient plaintext-AES builds — treat as Linux CBC fallback.
	return decryptPlatformV10(enc, userDataDir, "")
}

// aesGCMDecrypt decrypts the Windows v10 payload: nonce(12) ‖ ct ‖ tag(16)
// under the 32-byte key from DPAPI(Local State).
func aesGCMDecrypt(key, payload []byte) ([]byte, error) {
	if len(payload) < 12+16 {
		return nil, fmt.Errorf("gcm payload too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, payload[:12], payload[12:], nil)
}

// aesCBCDecrypt decrypts the Linux/mac v10/v11 payload: AES-128-CBC with a
// 16-byte key and IV = 16 space bytes, PKCS#5/7 padding.
func aesCBCDecrypt(key, payload []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("cbc key must be 16 bytes")
	}
	if len(payload) == 0 || len(payload)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("cbc payload not block-aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := bytes.Repeat([]byte(" "), aes.BlockSize)
	plain := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, payload)
	return stripPKCS7(plain)
}

func stripPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty plaintext")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(b) {
		return nil, fmt.Errorf("invalid pkcs7 padding")
	}
	for _, v := range b[len(b)-pad:] {
		if int(v) != pad {
			return nil, fmt.Errorf("corrupt pkcs7 padding")
		}
	}
	return b[:len(b)-pad], nil
}

// localStateCryptKey reads os_crypt.encrypted_key from the Electron user-data
// dir's Local State and unwraps it with the platform mechanism
// (DPAPI on Windows; base64 JSON plumbing shared here, the actual unprotect
// is platform code).
func localStateCryptKey(userDataDir string, unprotect func([]byte) ([]byte, error)) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(userDataDir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("read Local State: %w", err)
	}
	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("parse Local State: %w", err)
	}
	if ls.OsCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("Local State has no os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decode encrypted_key: %w", err)
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, fmt.Errorf("encrypted_key missing DPAPI prefix")
	}
	key, err := unprotect(blob[5:])
	if err != nil {
		return nil, fmt.Errorf("unprotect encrypted_key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("unexpected os_crypt key length %d", len(key))
	}
	return key, nil
}

// sha256Sum is the pseudo-UID helper (first 8 hex of sha256).
func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:4])
}
