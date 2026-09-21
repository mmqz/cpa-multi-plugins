// signing_test.go pins the Client Request Signing V4 crypto chain and the
// fail-open decision helpers. The wire-format constants (newline-joined
// messages, HKDF salt/info, base64 encoding, PoW seed shape) are load-bearing:
// the official client rejects space-joined variants, so every template here
// is byte-exact.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// hkdfExtract/hkdfExpand are textbook HKDF-Extract/Expand (SHA-256) used to
// independently cross-check hkdfBytes.
func hkdfExtract(secret, salt []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	return mac.Sum(nil)
}

func hkdfExpand(prk []byte, info string, length int) []byte {
	mac := hmac.New(sha256.New, prk)
	out := make([]byte, 0, length)
	var t []byte
	for i := byte(1); len(out) < length; i++ {
		mac.Reset()
		mac.Write(t)
		mac.Write([]byte(info))
		mac.Write([]byte{i})
		t = mac.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// TestHKDFBytesVector pins the HKDF derivation against an independently
// computed value (RFC 5869-style self-check with the plugin's salt/info).
func TestHKDFBytesVector(t *testing.T) {
	secret := []byte("test-secret-material")
	bits, err := hkdfBytes(secret, signKDFInfoHMAC)
	if err != nil {
		t.Fatalf("hkdfBytes: %v", err)
	}
	// Recompute via Extract/Expand explicitly.
	prk := hkdfExtract(secret, []byte(signKDFSalt))
	out := hkdfExpand(prk, signKDFInfoHMAC, 32)
	if hex.EncodeToString(bits) != hex.EncodeToString(out) {
		t.Fatalf("hkdf mismatch: got %x want %x", bits, out)
	}
	// Distinct info must produce distinct keys.
	other, err := hkdfBytes(secret, signKDFInfoEd25519)
	if err != nil {
		t.Fatalf("hkdfBytes(2): %v", err)
	}
	if hex.EncodeToString(bits) == hex.EncodeToString(other) {
		t.Fatalf("hkdf info separation broken")
	}
}

// TestHandshakeSignatureShape pins the newline-joined handshake message.
func TestHandshakeSignatureShape(t *testing.T) {
	secret, id, ts, nonce := "shhh", "key-1", "1700000000000", "aabbccddeeff00112233445566778899"
	sig, err := handshakeSignature(secret, id, ts, nonce)
	if err != nil {
		t.Fatalf("handshakeSignature: %v", err)
	}
	// Recompute manually: HMAC over "get_sign_key\n{id}\n{ts}\n{nonce}" with the
	// HKDF-derived key.
	bits, _ := hkdfBytes([]byte(secret), signKDFInfoHMAC)
	mac := hmac.New(sha256.New, bits)
	mac.Write([]byte("get_sign_key\n" + id + "\n" + ts + "\n" + nonce))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Fatalf("handshake sig mismatch: got %s want %s", sig, want)
	}
	// Space-joined variant must NOT match (the bundle rejects it upstream).
	mac2 := hmac.New(sha256.New, bits)
	mac2.Write([]byte("get_sign_key " + id + " " + ts + " " + nonce))
	if sig == base64.StdEncoding.EncodeToString(mac2.Sum(nil)) {
		t.Fatalf("space-joined signature unexpectedly matches")
	}
}

// TestSigningKeyRoundTrip proves decryptSigningPrivateKey recovers a key
// encrypted exactly the way the server encrypts privateCipher:
// AES-256-GCM over PKCS8, iv = first 12 bytes, AAD = apiKeyId.
func TestSigningKeyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	secret := "round-trip-secret"
	apiKeyID := "id-123"
	aesBits, _ := hkdfBytes([]byte(secret), signKDFInfoEd25519)
	block, _ := aes.NewCipher(aesBits)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	cipherBlob := gcm.Seal(nil, iv, pkcs8, []byte(apiKeyID))
	privateCipher := base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), cipherBlob...))

	got, err := decryptSigningPrivateKey(apiKeyID, secret, privateCipher)
	if err != nil {
		t.Fatalf("decryptSigningPrivateKey: %v", err)
	}
	if !pub.Equal(got.Public()) {
		t.Fatalf("recovered key mismatch")
	}
	// Signing with the recovered key must verify against the original pub.
	msg := []byte("business\nmessage")
	if !ed25519.Verify(pub, msg, ed25519.Sign(got, msg)) {
		t.Fatalf("signature from recovered key does not verify")
	}
}

// TestBusinessSignMessage pins the newline-joined business message.
func TestBusinessSignMessage(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := signBusinessMessage(priv, "id", "1700000000000", "3.14.0", "sess", "nonce01")
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("sig not base64: %v", err)
	}
	if len(raw) != ed25519.SignatureSize {
		t.Fatalf("sig size %d", len(raw))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key type")
	}
	if !ed25519.Verify(pub, []byte("id\n1700000000000\n3.14.0\nsess\nnonce01"), raw) {
		t.Fatalf("business signature does not verify over newline-joined message")
	}
}

// TestCreateProofOfWork solves a real PoW and validates the leading-zero
// contract plus the candidate shape (12-byte hex nonce + 8-hex counter).
func TestCreateProofOfWork(t *testing.T) {
	pow, err := createProofOfWork("key-1", "session-1", "1700000000000")
	if err != nil {
		t.Fatalf("createProofOfWork: %v", err)
	}
	if len(pow) != 24+8 {
		t.Fatalf("candidate shape: %s", pow)
	}
	seedDigest := sha256.Sum256([]byte("key-1\n" + signAppID + "\nsession-1\n1700000000000"))
	seed := hex.EncodeToString(seedDigest[:])[:32]
	digest := sha256.Sum256([]byte(seed + "\n" + pow))
	if !hasLeadingZeroBits(digest[:], signPowBits) {
		t.Fatalf("solved candidate lacks %d leading zero bits", signPowBits)
	}
}

func TestHasLeadingZeroBits(t *testing.T) {
	// At-least-N semantics: 0x00_01 carries 15 leading zero bits.
	if !hasLeadingZeroBits([]byte{0x00, 0x01}, 8) {
		t.Fatal("0x0001 has 15 leading zero bits, so 8 must hold")
	}
	if hasLeadingZeroBits([]byte{0x00, 0x01}, 16) {
		t.Fatal("0x0001 does not have 16 leading zero bits")
	}
	if !hasLeadingZeroBits([]byte{0x00, 0xFF}, 8) {
		t.Fatal("0x00FF has exactly 8 leading zero bits")
	}
	if hasLeadingZeroBits([]byte{0x01, 0xFF}, 8) {
		t.Fatal("0x01FF has only 7 leading zero bits")
	}
	if !hasLeadingZeroBits([]byte{0x00}, 8) {
		t.Fatal("0x00 has 8 leading zero bits")
	}
	if hasLeadingZeroBits([]byte{0x40}, 2) {
		t.Fatal("0x40 (01000000) has only 1 leading zero bit")
	}
	if !hasLeadingZeroBits([]byte{0x40}, 1) {
		t.Fatal("0x40 has 1 leading zero bit")
	}
}

func TestParseSigningCredential(t *testing.T) {
	if id, sec, ok := parseSigningCredential("abc.def"); !ok || id != "abc" || sec != "def" {
		t.Fatalf("two-part parse: %v %v %v", id, sec, ok)
	}
	if _, _, ok := parseSigningCredential("single"); ok {
		t.Fatal("single-part key must not sign")
	}
	if _, _, ok := parseSigningCredential("a.b.c"); ok {
		t.Fatal("three-part key must not sign")
	}
	if _, _, ok := parseSigningCredential(".secret"); ok {
		t.Fatal("empty id must not sign")
	}
}

func TestIsVerifyFailure(t *testing.T) {
	mk := func(body string) *http.Response {
		return &http.Response{StatusCode: 401, Body: http.NoBody}
	}
	_ = mk
	if !isVerifyFailure(401, []byte(`{"code":401,"msg":"VERIFY_SIGNATURE_INVALID"}`)) {
		t.Fatal("msg-form VERIFY_SIGNATURE_INVALID must match")
	}
	if !isVerifyFailure(401, []byte(`{"error":{"reason":"VERIFY_APIKEY_EXPIRED"}}`)) {
		t.Fatal("error.reason form must match")
	}
	if !isVerifyFailure(401, []byte(`{"data":{"reason":"VERIFY_SIGNATURE_INVALID"}}`)) {
		t.Fatal("data.reason form must match")
	}
	if isVerifyFailure(401, []byte(`{"msg":"something else"}`)) {
		t.Fatal("non-VERIFY 401 must not match")
	}
	if isVerifyFailure(403, []byte(`{"msg":"VERIFY_SIGNATURE_INVALID"}`)) {
		t.Fatal("non-401 must not match")
	}
}

func TestIsUnsignedChatPath(t *testing.T) {
	yes := []string{
		"/api/v1/zcode-plan/anthropic/v1/messages",
		"/api/v1/zcode-plan/chat/completions/",
		"/api/v1/off-peak/anthropic/v1/messages",
	}
	no := []string{"/chat/completions", "/api/anthropic/v1/messages"}
	for _, p := range yes {
		if !isUnsignedChatPath(p) {
			t.Fatalf("%s must be unsigned", p)
		}
	}
	for _, p := range no {
		if isUnsignedChatPath(p) {
			t.Fatalf("%s must be signable", p)
		}
	}
	if !strings.Contains("", "") {
		t.Fatal("strings sanity")
	}
}
