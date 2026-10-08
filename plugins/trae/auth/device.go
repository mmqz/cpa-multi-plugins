// device.go: EC P-256 device key pair + DeviceProof signing for the bound
// token renewal, shared by the login path (package main wraps the generator)
// and the refresh path (upstream.boundExchangeLocked).
//
// Why (v0.12.73, trae 9074 root cause): a JWT renewed via
// /cloudide/api/v3/trae/oauth/ExchangeToken (ClientID+RefreshToken only)
// carries NO device binding, and Trae CN answers its check-in claim with
// 9074 「当前参与用户太多」 every time while status stays code:0 — the
// official client's own (device-bound) token claims fine at the same moment
// on the same account (magpie#808 mintonight / MiFaZhan side-by-sides,
// 2026-10-05/06; qilimixingkong/trae-checkin TECH_DESIGN §2.8). The bound
// renewal signs with THIS key pair, whose public half the server binds.
package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// GenerateDeviceKeyPair creates a fresh P-256 key pair and returns the public
// key as a standard SPKI PEM ("-----BEGIN PUBLIC KEY-----") plus the private
// key as a PKCS#8 PEM — byte-compatible with the official client's uploaded
// DeviceInfo.DevicePublicKey (cockpit-tools pem_wrap shapes).
func GenerateDeviceKeyPair() (publicKeyPEM, privateKeyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate P-256 key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal PKCS#8 private key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal SPKI public key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return string(pubPEM), string(privPEM), nil
}

// SignDeviceProof signs payload with the PKCS#8 PEM device private key,
// ECDSA-SHA256, ASN.1 DER, base64 — the official client's DeviceProof
// signature shape (qilimixingkong/trae-checkin _ec_sign cross-verified
// against magpie-community/plugins proofOf).
func SignDeviceProof(privateKeyPEM, payload string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil || block.Type != "PRIVATE KEY" {
		return "", fmt.Errorf("device private key: bad PEM block %q", blockType(block))
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("device private key parse: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("device private key: %T is not an ECDSA key", key)
	}
	sum := sha256.Sum256([]byte(payload))
	sig, err := ecdsa.SignASN1(rand.Reader, ec, sum[:])
	if err != nil {
		return "", fmt.Errorf("device proof sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func blockType(b *pem.Block) string {
	if b == nil {
		return "<nil>"
	}
	return b.Type
}
