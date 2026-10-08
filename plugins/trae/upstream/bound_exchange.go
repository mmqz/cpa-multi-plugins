// bound_exchange.go — device-bound token renewal (v0.12.73, the trae 9074
// root fix).
//
// The legacy refresh path (EpExchange = /cloudide/api/v3/trae/oauth/
// ExchangeToken, ClientID+RefreshToken only) yields a JWT with NO device
// binding. Trae CN's growth gate answers that JWT's check-in claim with
// 9074 「当前参与用户太多，请稍后再试」 EVERY time while the read-only
// status stays code:0 — the official client's own (device-bound) token
// claims code:0 at the same moment on the same account (magpie#808:
// mintonight's same-account side-by-side 2026-10-05; MiFaZhan's
// out-of-plugin isolation 2026-10-05/06; qilimixingkong/trae-checkin
// TECH_DESIGN §2.8). This file renews the way the official clients do:
//
//	POST /trae/api/v3/oauth/ExchangeToken
//	{
//	  "ClientID":     <lineage client>,
//	  "ClientSecret": "",
//	  "RefreshToken": <rt>,
//	  "DeviceInfo":   { DeviceID, MachineID, PlatformCode, DeviceType "PC",
//	                    ClientVersion, DevicePublicKey <SPKI PEM>, ... },
//	  "DeviceProof":  { Signature <ECDSA-SHA256 DER b64 over
//	                     "POST\n{path}\n{ClientID}\n{RefreshToken}\n{ts}\n{nonce}">,
//	                    Timestamp, Nonce },
//	  "IDEVersion":   <per-identity version>
//	}
//
// Cross-verified against TWO independent working implementations:
// magpie-community/plugins packages/trae index.mjs (boundExchange/proofOf)
// and qilimixingkong/trae-checkin trae_checkin.py (_exchange_once). The
// refreshToken does not rotate on this path; a returned one is honored.
// Failure falls back to the legacy path — never worse than before.
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// epExchangeBound is the clients' own renewal path, which binds the JWT to
// the device named in DeviceInfo.
const epExchangeBound = "/trae/api/v3/oauth/ExchangeToken"

// exchangeIdentity is one renewal identity candidate: the client the refresh
// token is exchanged as, and the version that identity names.
type exchangeIdentity struct {
	ClientID     string
	PlatformCode string // SOLO_PC | IDE_PC
	Version      string
}

// exchangeIdentitiesFor returns the renewal identity candidates for a
// variant, OWN lineage first, then the cross-lineage one. Evidence
// (magpie#808 MiFaZhan 2026-10-06): a TRAE-spectrum refresh token 400s
// SOLO_PC with 10101 "refresh token is not matched to the client" and 200s
// IDE_PC — each token only matches its own lineage, so own-first, and the
// cross candidate is the self-healing fallback when the variant tag is
// wrong or legacy.
func exchangeIdentitiesFor(variant string) []exchangeIdentity {
	own := exchangeIdentity{
		ClientID:     ClientIDFor(variant),
		PlatformCode: PlatformCodeFor(variant),
		Version:      IdeVersion,
	}
	var cross exchangeIdentity
	if IsSoloVariant(variant) {
		cross = exchangeIdentity{ClientID: ClientIDFor("cn"), PlatformCode: PlatformCodeFor("cn"), Version: ideVersionIdePC}
	} else {
		cross = exchangeIdentity{ClientID: ClientIDFor("solo"), PlatformCode: PlatformCodeFor("solo"), Version: IdeVersion}
	}
	if cross.ClientID == own.ClientID {
		return []exchangeIdentity{own}
	}
	return []exchangeIdentity{own, cross}
}

// ideVersionIdePC is the Trae CN IDE version the IDE_PC renewal names
// (magpie deviceExchange as[1].version — client 3.3.104 era).
const ideVersionIdePC = "3.3.99"

// PlatformCodeFor maps a variant to the ExchangeToken PlatformCode
// (SOLO_PC for the SOLO/TraeWork lineage, IDE_PC for Trae Code CN).
func PlatformCodeFor(variant string) string {
	if IsSoloVariant(variant) {
		return "SOLO_PC"
	}
	return "IDE_PC"
}

// deviceProofPayload is the exact string the device key signs:
// method, path, client, refresh token, unix seconds, nonce — one per line.
func deviceProofPayload(path, clientID, refreshToken string, ts int64, nonce string) string {
	return strings.Join([]string{"POST", path, clientID, refreshToken, fmt.Sprintf("%d", ts), nonce}, "\n")
}

// newProofNonce mints a 128-bit hex nonce per attempt (the clients mint one
// per renewal; trae-checkin "%032x" / magpie randomBytes(16).toString("hex")).
func newProofNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// boundDeviceInfo mirrors the login DeviceInfo (main's deviceInfo struct)
// with the persisted device identity this renewal binds.
func boundDeviceInfo(a *auth.Auth, platformCode, clientVersion string) map[string]any {
	deviceType := "windows"
	osVersion := OSVersion
	return map[string]any{
		"DeviceID":        a.DeviceID,
		"MachineID":       a.MachineID,
		"PlatformCode":    platformCode,
		"DeviceType":      "PC",
		"DeviceName":      "",
		"DeviceModel":     DeviceBrand,
		"ClientVersion":   clientVersion,
		"DevicePublicKey": a.DevicePublicKey,
		"DeviceBrand":     DeviceBrand,
		"DeviceCPU":       "",
		"OSInfo":          deviceType,
		"OSVersion":       osVersion,
	}
}

// ensureDeviceKeyLocked returns the account's device key pair, generating and
// attaching one when the account predates the bound renewal (older logins).
// The generated pair persists with the next SaveAtomic, so every later
// renewal signs with the SAME key the server bound — magpie keeps this
// contract too ("a device key made for the account goes with either").
// Caller must hold a's write lock.
func ensureDeviceKeyLocked(a *auth.Auth) {
	if strings.TrimSpace(a.DevicePublicKey) != "" && strings.TrimSpace(a.DevicePrivateKey) != "" {
		return
	}
	pubPEM, privPEM, err := auth.GenerateDeviceKeyPair()
	if err != nil {
		return // bound exchange will fail without keys; legacy path still renews
	}
	a.DevicePublicKey = pubPEM
	a.DevicePrivateKey = privPEM
}

// boundExchangeLocked renews the JWT the official-client way so it is bound
// to the account's device. Every failure path leaves a untouched (the
// refreshLocked atomicity contract). Caller must hold a's write lock.
func (c *Client) boundExchangeLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	ensureDeviceKeyLocked(a)
	if a.DevicePublicKey == "" || a.DevicePrivateKey == "" {
		return fmt.Errorf("no device key pair")
	}
	rt := a.RefreshToken // held write lock
	hosts := exchangeHosts(a.APIHost, c.OAuthHost)
	var lastErr error
	for _, id := range exchangeIdentitiesFor(a.Variant) {
		for _, host := range hosts {
			ts := time.Now().Unix()
			nonce := newProofNonce()
			proof, err := auth.SignDeviceProof(a.DevicePrivateKey, deviceProofPayload(epExchangeBound, id.ClientID, rt, ts, nonce))
			if err != nil {
				return err // key material problem: no point trying other hosts
			}
			body := map[string]any{
				"ClientID":     id.ClientID,
				"ClientSecret": "",
				"RefreshToken": rt,
				"DeviceInfo":   boundDeviceInfo(a, id.PlatformCode, id.Version),
				"DeviceProof": map[string]any{
					"Signature": proof,
					"Timestamp": ts,
					"Nonce":     nonce,
				},
				"IDEVersion": id.Version,
			}
			raw, _ := json.Marshal(body)
			req, err := http.NewRequest(http.MethodPost, host+epExchangeBound, bytes.NewReader(raw))
			if err != nil {
				return err
			}
			OAuthHeaders(req)
			data, err := c.doJSON(req)
			if err != nil {
				lastErr = err
				continue
			}
			token, refreshToken, expiresAt, ok := parseBoundExchange(data)
			if !ok {
				lastErr = fmt.Errorf("bound exchange: no token in response")
				continue
			}
			a.AccessToken = token
			if refreshToken != "" {
				a.RefreshToken = refreshToken
			}
			if expiresAt > 0 {
				a.ExpiresAt = expiresAt
			}
			log.Printf("exchange refresh (bound): uid=%s variant=%q client=%s platform=%s", a.UID, a.Variant, id.ClientID, id.PlatformCode)
			return nil
		}
	}
	return lastErr
}

// parseBoundExchange reads the ExchangeToken envelope (Result.Token /
// TokenExpireAt, refreshToken optional) shared by both renewal paths.
func parseBoundExchange(data []byte) (token, refreshToken string, expiresAt int64, ok bool) {
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", 0, false
	}
	if resp.Result.Token == "" {
		return "", "", 0, false
	}
	exp := int64(0)
	if resp.Result.TokenExpireAt > 0 {
		exp = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		exp = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return resp.Result.Token, resp.Result.RefreshToken, exp, true
}
