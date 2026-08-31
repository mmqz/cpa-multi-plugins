// helpers.go: crypto helpers (separated to avoid pulling crypto/rand import
// conflicts with the cgo main file).
package main

import (
        "crypto/rand"
        "crypto/sha256"
        "encoding/base64"
        "encoding/hex"
        "fmt"
        "net/url"
)

func readRand(b []byte) (int, error) {
        return rand.Read(b)
}

func hexEncode(b []byte) string {
        return hex.EncodeToString(b)
}

// urlEncode URL-encodes a string for use in a query parameter value.
// Mirrors trae-solo-cn/helpers.go.
func urlEncode(s string) string {
        return url.QueryEscape(s)
}

// generatePKCEPair returns a PKCE (code_verifier, code_challenge) pair using
// the S256 method. Mirrors trae-solo-cn/helpers.go (cockpit-tools
// generate_pkce_pair, trae_oauth.rs:187-196):
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

// newLoginTraceID generates a UUID v4 for OAuth login trace ID.
func newLoginTraceID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newDeviceID generates a random 16-byte hex device ID.
func newDeviceID() string {
	return randomHex(16)
}

// newMachineID generates a random 16-byte hex machine ID.
func newMachineID() string {
	return randomHex(16)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
