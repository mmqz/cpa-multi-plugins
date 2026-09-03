// helpers.go: net aliases, random device identifiers, PKCE pair, URL encoding,
// and login trace ID generation — all aligned with cockpit-tools trae_oauth.rs.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"
)

// Aliases so main.go can reference them as netListen / netTCPAddr etc.
type (
	netListener    = net.Listener
	netTCPAddr     = net.TCPAddr
	netTCPListener = net.TCPListener
	netConn        = net.Conn
)

func netListen(network, addr string) (netListener, error) {
	return net.Listen(network, addr)
}

// randomHex returns n random bytes as a hex string (length 2*n).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomDigits returns a random numeric string of n digits. Trae device ids
// are numeric strings of 8-24 digits (cockpit-tools normalize_device_id →
// is_numeric_id(8, 24); extract_device_id_from_logs matches [0-9]{8,24} and
// TRAE_DEFAULT_DEVICE_ID is "0"). The previous randomHex(16) produced a
// 32-char hex string containing letters — out of spec upstream.
func randomDigits(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b)
}

// newUUIDv4 returns a random RFC 4122 UUID v4 string. cockpit-tools uses
// Uuid::new_v4() as the machine_id fallback (generate_service_machine_id,
// trae_oauth.rs:183-185) when the real IDE telemetry.machineId is absent.
func newUUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// urlEncode URL-encodes a string for use in a query parameter value.
// Equivalent to Rust's urlencoding::encode (RFC 3986 unreserved set).
// Go's url.QueryEscape produces RFC 3986-compatible output (space → +,
// which is valid in application/x-www-form-urlencoded contexts).
func urlEncode(s string) string {
	return url.QueryEscape(s)
}

// generatePKCEPair returns a PKCE (code_verifier, code_challenge) pair using
// the S256 method. Mirrors cockpit-tools generate_pkce_pair (trae_oauth.rs:187-196):
//   - 48 random bytes → base64url no-pad → code_verifier (~64 chars)
//   - SHA256(code_verifier) → base64url no-pad → code_challenge (~43 chars)
func generatePKCEPair() (codeVerifier, codeChallenge string) {
	var b [48]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read should never fail on Linux/Windows; if it does, panic
		// (we cannot proceed without a verifier).
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	codeVerifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return codeVerifier, codeChallenge
}

// newLoginTraceID returns a fresh UUID v4 (lowercase, with dashes).
// Used as both the OAuth state and the login_trace_id query parameter.
func newLoginTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	// RFC 4122 §4.4: set version (4) and variant (10xx) bits.
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sweepExpiredLoginStates deletes expired login states from one sync.Map and
// closes their callback listeners. Shared by the CN/SOLO (loginStates) and
// Intl (intlloginStates) janitors (v0.12.3: the Intl map was previously never
// swept, leaking one listener per abandoned login until process restart).
func sweepExpiredLoginStates(now time.Time, m *sync.Map, project func(any) (time.Time, netListener, bool)) {
	m.Range(func(key, value any) bool {
		expires, listener, ok := project(value)
		if ok && now.After(expires) {
			m.Delete(key)
			if listener != nil {
				listener.Close()
			}
		}
		return true
	})
}
