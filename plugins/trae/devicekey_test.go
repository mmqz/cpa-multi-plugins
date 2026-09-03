package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
)

// v0.12.24: the ExchangeToken DeviceInfo must carry a fresh EC P-256 public
// key (cockpit-tools generate_device_key_pair parity) — an empty value
// surfaces as HTTP 401 / business code 20405 (live-verified 2026-09-04).
func TestGenerateDeviceKeyPair(t *testing.T) {
	pubPEM, privPEM, err := generateDeviceKeyPair()
	if err != nil {
		t.Fatalf("generateDeviceKeyPair: %v", err)
	}
	if !strings.HasPrefix(pubPEM, "-----BEGIN PUBLIC KEY-----") {
		t.Errorf("public key PEM header = %q", pubPEM[:30])
	}
	if !strings.HasPrefix(privPEM, "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("private key PEM header = %q", privPEM[:34])
	}
	pubBlock, _ := pem.Decode([]byte(pubPEM))
	if pubBlock == nil {
		t.Fatal("public PEM does not decode")
	}
	pubAny, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("ParsePKIXPublicKey: %v", err)
	}
	if _, ok := pubAny.(*ecdsa.PublicKey); !ok {
		t.Fatalf("public key is %T, want *ecdsa.PublicKey", pubAny)
	}
	// Two logins must produce independent key pairs (fresh per login).
	pub2, _, err := generateDeviceKeyPair()
	if err != nil {
		t.Fatalf("second generateDeviceKeyPair: %v", err)
	}
	if pub2 == pubPEM {
		t.Fatal("two logins produced the same public key")
	}
}

func TestDeviceBrandForContext(t *testing.T) {
	cases := map[string]string{
		"windows": "Microsoft",
		"mac":     "Apple",
		"linux":   "Linux",
		"freebsd": "freebsd",
	}
	for in, want := range cases {
		if got := deviceBrandForContext(in); got != want {
			t.Errorf("deviceBrandForContext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIntlbuildOfficialDeviceInfoCarriesPublicKey(t *testing.T) {
	di := intlbuildOfficialDeviceInfo("dev1", "mach1", "IDE_PC", "DESKTOP-X",
		"83DG", "3.5.66", "windows", "Windows 11 Pro", "-----BEGIN PUBLIC KEY-----X")
	if di["DevicePublicKey"] != "-----BEGIN PUBLIC KEY-----X" {
		t.Errorf("DevicePublicKey = %v", di["DevicePublicKey"])
	}
	if di["DeviceBrand"] != "Microsoft" {
		t.Errorf("DeviceBrand = %v, want Microsoft (windows vendor map)", di["DeviceBrand"])
	}
	if di["DeviceModel"] != "83DG" {
		t.Errorf("DeviceModel = %v, want the raw x_device_brand", di["DeviceModel"])
	}
}

// cockpit-tools candidate_account_api_origins: the official client exchanges
// the auth code EXCLUSIVELY against the account-API origins — sg first for a
// plain i18n row user (test auth_code_account_urls_use_official_sg_for_i18n_row_user),
// usttp first for US-direct accounts — and never against the callback
// loginHost (api-sg-central.trae.ai answers an auth code with HTTP 401 code
// 20405).
func TestIntlAuthCodeExchangeURLsSGFirst(t *testing.T) {
	urls := intlauthCodeExchangeURLs("api-sg-central.trae.ai", "")
	if len(urls) == 0 {
		t.Fatal("no urls")
	}
	if urls[0] != "https://growsg-normal.trae.ai/trae/api/v3/oauth/ExchangeToken" {
		t.Errorf("first url = %s, want growsg-normal (SG default)", urls[0])
	}
	for i, u := range urls[:3] {
		if strings.Contains(u, "api-sg-central") {
			t.Errorf("loginHost-derived url at position %d (must come after all official origins): %s", i, u)
		}
	}
	found := -1
	for i, u := range urls {
		if strings.Contains(u, "api-sg-central") {
			found = i
			break
		}
	}
	if found < 3 {
		t.Errorf("loginHost-derived url at index %d, want index >= 3 (after the official origins)", found)
	}
}

func TestIntlAuthCodeExchangeURLsUsttpFirst(t *testing.T) {
	for _, tag := range []string{"usttp", "USTTP", "us_ttp", "us-ttp"} {
		urls := intlauthCodeExchangeURLs("api-sg-central.trae.ai", tag)
		if urls[0] != "https://grow-normal.traeapi.us/trae/api/v3/oauth/ExchangeToken" {
			t.Errorf("tag %q: first url = %s, want the US origin", tag, urls[0])
		}
	}
}

func TestAuthCodeExchangeURLsCNFirst(t *testing.T) {
	urls := authCodeExchangeURLsCN("www.trae.cn")
	if urls[0] != "https://api.trae.cn/trae/api/v3/oauth/ExchangeToken" {
		t.Errorf("first url = %s, want api.trae.cn", urls[0])
	}
	if urls[1] != "https://api.trae.com.cn/trae/api/v3/oauth/ExchangeToken" {
		t.Errorf("second url = %s, want api.trae.com.cn", urls[1])
	}
}

func TestParseCallbackParamsUserTag(t *testing.T) {
	q := url.Values{}
	q.Set("userTag", "USTTP")
	q.Set("authCode", "ac1")
	p := parseCallbackParams(q)
	if p.userTag != "USTTP" {
		t.Errorf("userTag = %q, want USTTP", p.userTag)
	}
	if p.authCode != "ac1" {
		t.Errorf("authCode = %q, want ac1", p.authCode)
	}
}
