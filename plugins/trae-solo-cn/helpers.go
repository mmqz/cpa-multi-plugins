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
)

// Aliases so main.go can reference them as netListen / netTCPAddr etc.
type (
	netListener    = net.Listener
	netTCPAddr     = net.TCPAddr
	netTCPListener = net.TCPListener
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
