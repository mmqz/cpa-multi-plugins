package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// The fixtures below mirror the LIVE cockpit-tools export of the 9074-affected
// TRAE SOLO CN account (upload/unknown_2026-09-06.json, captured 2026-09-06):
// exchangeResponse top level + Result, and the GetUserInfo profile Result.
const parityExchangeFixture = `{"AIRegion":"CN","ResponseMetadata":{"Action":"","Region":"","TraceID":"00000000000000000000000000000000"},"Result":{"BoundDeviceID":"e4w2k6llxjrky2","ClientID":"en1oxy7wnw8j9n","DeviceBindStatus":"BOUND","RefreshExpireAt":1804206273052,"RefreshToken":"jbGpBmlK.18d29395ab406f66","Token":"eyJ.abc","TokenExpireAt":1789863873052,"UserJwt":"eyJ.abc"},"authClientId":"en1oxy7wnw8j9n","host":"https://api.trae.cn","loginHost":"https://api.trae.cn","loginRegion":"cn","storeRegion":"CN"}`

const parityProfileFixture = `{"ResponseMetadata":{"Service":""},"Result":{"AIRegion":"CN","AvatarUrl":"https://p3-passport.example/avatar.png","Description":"","Gender":"0","LastLoginTime":"2026-09-06T08:11:08+08:00","LastLoginType":"sms","NonPlainTextMobile":"130******63","Region":"CN","RegisterTime":"2026-07-30T18:13:05.648+08:00","ScreenName":"用户37396015402","TenantID":"7o2d894p7dr0o4","UserID":"1237380756941412"}}`

// TestCredentialParityFieldsSoloCN locks the full parity field set for a
// TRAE SOLO CN login against the cockpit-tools export values.
func TestCredentialParityFieldsSoloCN(t *testing.T) {
	authExtras, accountExtras := credentialParityFields("solo", "https://www.trae.cn", []byte(parityExchangeFixture), []byte(parityProfileFixture))

	if authExtras["platformId"] != "trae_solo_cn" {
		t.Errorf("platformId=%v want trae_solo_cn", authExtras["platformId"])
	}
	if authExtras["platformName"] != "TRAE SOLO CN" {
		t.Errorf("platformName=%v want TRAE SOLO CN", authExtras["platformName"])
	}
	if authExtras["authClientId"] != "en1oxy7wnw8j9n" {
		t.Errorf("authClientId=%v want en1oxy7wnw8j9n (echo wins over default)", authExtras["authClientId"])
	}
	if authExtras["boundDeviceId"] != "e4w2k6llxjrky2" {
		t.Errorf("boundDeviceId=%v want e4w2k6llxjrky2", authExtras["boundDeviceId"])
	}
	if authExtras["deviceBindStatus"] != "BOUND" {
		t.Errorf("deviceBindStatus=%v want BOUND", authExtras["deviceBindStatus"])
	}
	if authExtras["refreshExpiredAt"] != int64(1804206273) {
		t.Errorf("refreshExpiredAt=%v want 1804206273 (ms→s)", authExtras["refreshExpiredAt"])
	}
	// Echo overrides for region/host.
	if authExtras["host"] != "https://api.trae.cn" || authExtras["loginHost"] != "https://api.trae.cn" {
		t.Errorf("host/loginHost echo wrong: %v / %v", authExtras["host"], authExtras["loginHost"])
	}
	if authExtras["loginRegion"] != "cn" || authExtras["storeRegion"] != "CN" || authExtras["aiRegion"] != "CN" {
		t.Errorf("region echo wrong: %v %v %v", authExtras["loginRegion"], authExtras["storeRegion"], authExtras["aiRegion"])
	}
	if authExtras["authDomain"] != "www.trae.cn" {
		t.Errorf("authDomain=%v want www.trae.cn (cockpit TRAE_CN_AUTH_DOMAIN)", authExtras["authDomain"])
	}
	// exchangeResponse byte-preserving round trip.
	raw, ok := authExtras["exchangeResponse"].(json.RawMessage)
	if !ok || !strings.Contains(string(raw), "e4w2k6llxjrky2") || !strings.Contains(string(raw), "RefreshExpireAt") {
		t.Errorf("exchangeResponse raw not preserved: %v", authExtras["exchangeResponse"])
	}
	// userRegion cockpit shape.
	ur, ok := authExtras["userRegion"].(map[string]string)
	if !ok || ur["region"] != "CN" || ur["_aiRegion"] != "CN" {
		t.Errorf("userRegion=%v want {region:CN,_aiRegion:CN}", authExtras["userRegion"])
	}
	// Account-side rich profile.
	for k, want := range map[string]string{
		"avatar": "https://p3-passport.example/avatar.png", "region": "CN", "aiRegion": "CN",
		"tenantId": "7o2d894p7dr0o4", "mobile": "130******63", "registerTime": "2026-07-30T18:13:05.648+08:00",
	} {
		if got, _ := accountExtras[k].(string); got != want {
			t.Errorf("accountExtras[%q]=%v want %q", k, accountExtras[k], want)
		}
	}
}

// TestCredentialParityFieldsNoEcho covers the refresh-token login path (no
// ExchangeToken raw): only variant-derivable fields land, no echo keys.
func TestCredentialParityFieldsNoEcho(t *testing.T) {
	authExtras, _ := credentialParityFields("solo", "", nil, nil)
	if authExtras["platformId"] != "trae_solo_cn" || authExtras["authClientId"] != "en1oxy7wnw8j9n" {
		t.Errorf("variant-derivable fields missing: %v", authExtras)
	}
	for _, absent := range []string{"exchangeResponse", "boundDeviceId", "deviceBindStatus", "refreshExpiredAt", "host"} {
		if _, exists := authExtras[absent]; exists {
			t.Errorf("%s must be absent without an exchange echo", absent)
		}
	}
	// CN defaults still apply.
	if authExtras["loginRegion"] != "cn" || authExtras["storeRegion"] != "CN" {
		t.Errorf("CN defaults missing: %v", authExtras)
	}
}

// TestCredentialParityFieldsIntlNoGuess: the intl realm gets NO invented
// region values (the plugin's intl hosts are marscode.com; cockpit's
// www.trae.ai constant is not assumed) — region fields only via echo.
func TestCredentialParityFieldsIntlNoGuess(t *testing.T) {
	authExtras, _ := credentialParityFields("intl", "", nil, nil)
	if authExtras["platformId"] != "trae" {
		t.Errorf("intl platformId=%v want trae", authExtras["platformId"])
	}
	if authExtras["authClientId"] != "ono9krqynydwx5" {
		t.Errorf("intl authClientId=%v want ono9krqynydwx5", authExtras["authClientId"])
	}
	for _, guessed := range []string{"loginRegion", "storeRegion", "aiRegion", "authDomain"} {
		if _, exists := authExtras[guessed]; exists {
			t.Errorf("intl must not guess %s", guessed)
		}
	}
}

// TestMergeAuthStoragePreservesExtras locks the v0.12.44 refresh fix: the
// previous rebuild dropped devicePublicKey/devicePrivateKey (the v0.12.24
// device-binding key pair!) and every parity extra on each refresh.
func TestMergeAuthStoragePreservesExtras(t *testing.T) {
	existing := `{
  "type": "trae",
  "provider": "trae",
  "auth": {
    "accessToken": "old-at",
    "refreshToken": "old-rt",
    "expiresAt": 1000,
    "domain": "trae.cn",
    "apiHost": "https://api.trae.cn",
    "machineId": "mid-1",
    "deviceId": "d-16",
    "variant": "solo",
    "devicePublicKey": "PUB-PEM",
    "devicePrivateKey": "PRIV-PEM",
    "platformId": "trae_solo_cn",
    "exchangeResponse": {"Result": {"BoundDeviceID": "e4w2k6llxjrky2"}}
  },
  "account": {"uid": "u-1", "nickname": "用户X"},
  "disabled": false
}`
	a := fixtureAuth("new-at", "new-rt", 2000)
	merged := mergeAuthStorage([]byte(existing), a)

	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatalf("merged not json: %v", err)
	}
	am := m["auth"].(map[string]any)
	if am["accessToken"] != "new-at" || am["refreshToken"] != "new-rt" {
		t.Errorf("token fields not updated: %v", am)
	}
	if am["expiresAt"] != float64(2000) {
		t.Errorf("expiresAt not updated: %v", am)
	}
	// The whole point: custom fields survive the refresh.
	if am["devicePrivateKey"] != "PRIV-PEM" || am["devicePublicKey"] != "PUB-PEM" {
		t.Errorf("device key pair LOST on refresh: %v", am)
	}
	if am["platformId"] != "trae_solo_cn" {
		t.Errorf("platformId lost: %v", am)
	}
	if _, ok := am["exchangeResponse"].(map[string]any); !ok {
		t.Errorf("exchangeResponse lost: %v", am)
	}
	if m["account"].(map[string]any)["uid"] != "u-1" {
		t.Errorf("account.uid lost")
	}
	if m["type"] != "trae" || m["provider"] != "trae" {
		t.Errorf("routing fields lost: %v %v", m["type"], m["provider"])
	}

	// Unparseable existing → legacy rebuild fallback (never lose tokens).
	fallback := mergeAuthStorage([]byte(`not-json`), a)
	var fb map[string]any
	if err := json.Unmarshal(fallback, &fb); err != nil || fb["auth"] == nil {
		t.Fatalf("fallback broken: %v %v", err, fb)
	}
}

// fixtureAuth builds a minimal *auth.Auth for merge tests.
func fixtureAuth(at, rt string, exp int64) *auth.Auth {
	return &auth.Auth{
		AccessToken: at, RefreshToken: rt, ExpiresAt: exp,
		Domain: "trae.cn", APIHost: "https://api.trae.cn",
		MachineID: "mid-1", DeviceID: "d-16", Variant: "solo",
		UID: "u-1", Nickname: "用户X",
	}
}
