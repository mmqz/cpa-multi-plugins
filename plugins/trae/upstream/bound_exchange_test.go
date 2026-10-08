// bound_exchange_test.go pins the v0.12.73 device-bound token renewal (the
// trae 9074 root fix): the proof payload shape, the ECDSA signature round
// trip, bound-first ordering in refreshLocked, and the legacy-path fallback
// when the bound renewal is refused.
package upstream

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

func TestDeviceProofPayloadShape(t *testing.T) {
	// method, path, client, refresh token, unix seconds, nonce — one per line
	// (magpie proofOf / trae-checkin _exchange_once cross-verified).
	got := deviceProofPayload("/trae/api/v3/oauth/ExchangeToken", "cid", "rt", 1750000000, "abc123")
	want := "POST\n/trae/api/v3/oauth/ExchangeToken\ncid\nrt\n1750000000\nabc123"
	if got != want {
		t.Errorf("deviceProofPayload = %q, want %q", got, want)
	}
}

func TestSignDeviceProofVerifiesWithPublicKey(t *testing.T) {
	pubPEM, privPEM, err := auth.GenerateDeviceKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	payload := "POST\n/trae/api/v3/oauth/ExchangeToken\ncid\nrt\n1750000000\nnonce"
	sig, err := auth.SignDeviceProof(privPEM, payload)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatal("public key PEM undecodable")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T", pubAny)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(payload))
	if !ecdsa.VerifyASN1(pub, sum[:], raw) {
		t.Error("signature does not verify against the generated public key")
	}
	// a tampered payload must not verify
	sum2 := sha256.Sum256([]byte(payload + "x"))
	if ecdsa.VerifyASN1(pub, sum2[:], raw) {
		t.Error("signature verified over a different payload")
	}
}

func TestRefreshLockedBoundExchangeFirst(t *testing.T) {
	var boundBody map[string]any
	var boundSeen bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == epExchangeBound {
			boundSeen = true
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &boundBody); err != nil {
				return nil, err
			}
			return jsonResp(200, `{"Result":{"Token":"bound-token","TokenExpireAt":1790000000000}}`), nil
		}
		return nil, errors.New("unexpected path " + r.URL.Path)
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, APIHost: "https://oauth.example", Variant: "cn", DeviceID: "1111222233334444", MachineID: "machine-1"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !boundSeen {
		t.Fatal("bound exchange path never hit")
	}
	if a.AccessToken != "bound-token" {
		t.Errorf("AccessToken = %q, want bound-token", a.AccessToken)
	}
	if exp := a.ExpiresAt; exp != 1790000000 {
		t.Errorf("ExpiresAt = %d, want ms-normalized 1790000000", exp)
	}
	// the account had no device keys → generated, attached, ready to persist
	if a.DevicePublicKey == "" || a.DevicePrivateKey == "" {
		t.Error("device key pair not generated/attached")
	}
	// request shape: lineage client, device info with the public key, proof
	if boundBody["ClientID"] != ClientIDFor("cn") {
		t.Errorf("ClientID = %v, want the cn lineage id", boundBody["ClientID"])
	}
	if boundBody["RefreshToken"] != "oldrt" {
		t.Errorf("RefreshToken = %v", boundBody["RefreshToken"])
	}
	di, ok := boundBody["DeviceInfo"].(map[string]any)
	if !ok {
		t.Fatal("DeviceInfo missing")
	}
	if di["DeviceID"] != "1111222233334444" || di["DevicePublicKey"] == "" || di["PlatformCode"] != "IDE_PC" {
		t.Errorf("DeviceInfo = %v, want device identity with bound public key", di)
	}
	proof, ok := boundBody["DeviceProof"].(map[string]any)
	if !ok {
		t.Fatal("DeviceProof missing")
	}
	sig, _ := proof["Signature"].(string)
	ts, _ := proof["Timestamp"].(float64)
	nonce, _ := proof["Nonce"].(string)
	if sig == "" || ts == 0 || nonce == "" {
		t.Errorf("DeviceProof incomplete: %v", proof)
	}
}

func TestRefreshLockedBoundFallsBackToLegacy(t *testing.T) {
	// bound refused (400, e.g. cross-lineage mismatch on both identities) →
	// legacy /cloudide path still renews; never worse than before.
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == epExchangeBound {
			return jsonResp(400, `{"code":10101,"message":"refresh token is not matched to the client"}`), nil
		}
		if strings.HasSuffix(r.URL.Path, EpExchange) {
			return jsonResp(200, `{"Result":{"Token":"legacy-token","RefreshToken":"legacy-rt"}}`), nil
		}
		return nil, errors.New("unexpected path " + r.URL.Path)
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, APIHost: "https://oauth.example", Variant: "cn", DeviceID: "1111222233334444"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "legacy-token" || a.RefreshToken != "legacy-rt" {
		t.Errorf("fallback tokens wrong: %q / %q", a.AccessToken, a.RefreshToken)
	}
	// the generated key pair stays attached for the next attempt (same key the
	// server will eventually bind), ready for SaveAtomic persistence
	if a.DevicePublicKey == "" || a.DevicePrivateKey == "" {
		t.Error("generated device keys were not kept after fallback")
	}
}

func TestExchangeIdentitiesOwnLineageFirst(t *testing.T) {
	solo := exchangeIdentitiesFor("solo")
	if solo[0].ClientID != ClientIDFor("solo") || solo[0].PlatformCode != "SOLO_PC" {
		t.Errorf("solo own identity = %+v", solo[0])
	}
	if len(solo) < 2 || solo[1].ClientID != ClientIDFor("cn") {
		t.Errorf("solo cross identity = %+v", solo)
	}
	cn := exchangeIdentitiesFor("cn")
	if cn[0].ClientID != ClientIDFor("cn") || cn[0].PlatformCode != "IDE_PC" {
		t.Errorf("cn own identity = %+v", cn[0])
	}
	if len(cn) < 2 || cn[1].PlatformCode != "SOLO_PC" {
		t.Errorf("cn cross identity = %+v", cn)
	}
}
