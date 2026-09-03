// oauth_fallback.go: multi-endpoint OAuth fallback helpers, ported from
// cockpit-tools trae_oauth.rs to fix "404 page not found" during OAuth login.
//
// Fixes applied (all mirror cockpit-tools behavior):
//   - intlensureHTTPSScheme: cockpit-tools ensure_https_url (trae_oauth.rs:1117-1126).
//     GetLoginGuidance returns LoginHost as a BARE domain ("www.trae.ai"); the
//     login URL must get an https:// prefix or the panel/browser resolves it as
//     a RELATIVE URL against the CPA server itself → gin default 404 body
//     "404 page not found".
//   - intlrequestLoginGuidance: cockpit-tools request_login_guidance
//     (trae_oauth.rs:1762-1848) — tries every GetLoginGuidance endpoint in the
//     platform list before failing. Intl candidates: api.marscode.com →
//     api.trae.ai → www.trae.ai, so a single-host outage/404 no longer blocks
//     the whole login flow.
//   - intlcandidateAPIOorigins + intlbuildAPIURLs: cockpit-tools candidate_api_origins /
//     candidate_account_api_origins (trae_oauth.rs:1994-2049, 673-689) —
//     derives candidate API origins from the callback loginHost (rewriting
//     www. → api.), then appends the Intl account-API defaults
//     (grow-normal.trae.ai / growsg-normal.trae.ai). ExchangeToken/GetUserInfo
//     try each URL in turn until one returns an access token.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Platform GetLoginGuidance endpoint lists — cockpit-tools
// TRAE_LOGIN_GUIDANCE_URLS (trae_oauth.rs:36-39).
var intlLoginGuidanceURLs = []string{
	"https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
	"https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
	"https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
}

// Intl default API origins — cockpit-tools default_account_api_config
// (trae_oauth.rs:683-688) + candidate_api_origins Intl branch (2015-2022):
// account API normal/sg first, then the classic api hosts.
// grow-normal.traeapi.us = cockpit-tools TRAE_ACCOUNT_API_ORIGIN_USTTP (the
// upstream's newer US direct origin; live-verified 2026-09-03 that the
// classic api.* hosts 404 /trae/api/v3/oauth/ExchangeToken at the TLB edge,
// so every extra real origin widens the fallback without any downside).
var intlDefaultOrigins = []string{
	"https://grow-normal.trae.ai",
	"https://growsg-normal.trae.ai",
	"https://grow-normal.traeapi.us",
	"https://api.marscode.com",
	"https://api.trae.ai",
}

// intlensureHTTPSScheme mirrors cockpit-tools ensure_https_url: a bare domain such
// as "www.trae.ai" gets an https:// prefix. Without this the verification URI
// returned to the panel is scheme-less and the browser treats it as a relative
// path on the CPA server → "404 page not found".
func intlensureHTTPSScheme(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	if strings.HasPrefix(normalized, "http://") || strings.HasPrefix(normalized, "https://") {
		return normalized
	}
	return "https://" + strings.TrimLeft(normalized, "/")
}

// intlcandidateAPIOorigins mirrors cockpit-tools candidate_api_origins:
//  1. origin derived from the callback loginHost (scheme + host);
//  2. if that host starts with "www.", also the api. variant;
//  3. platform default origins.
//
// Results are de-duplicated preserving order (dedup_keep_order).
func intlcandidateAPIOorigins(loginHost string, cn bool) []string {
	var origins []string
	if u := intlensureHTTPSScheme(loginHost); u != "" {
		if host := intlhostOnly(u); host != "" {
			origins = append(origins, intlschemeOf(u)+"://"+host)
			if stripped := strings.TrimPrefix(host, "www."); stripped != host {
				origins = append(origins, intlschemeOf(u)+"://api."+stripped)
			}
		}
	}
	if cn {
		// Not used by trae-intl (CN-only helpers live in trae-cn/trae-solo-cn);
		// kept for parity with the shared helper shape.
		origins = append(origins, "https://api.trae.cn", "https://api.trae.com.cn")
	} else {
		origins = append(origins, intlDefaultOrigins...)
	}
	return intldedupKeepOrder(origins)
}

// intlbuildAPIURLs maps candidate origins onto an endpoint path.
func intlbuildAPIURLs(loginHost, path string, cn bool) []string {
	var urls []string
	for _, origin := range intlcandidateAPIOorigins(loginHost, cn) {
		urls = append(urls, strings.TrimRight(origin, "/")+path)
	}
	return intldedupKeepOrder(urls)
}

func intlhostOnly(u string) string {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func intlschemeOf(u string) string {
	if i := strings.Index(u, "://"); i > 0 {
		return u[:i]
	}
	return "https"
}

func intldedupKeepOrder(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// intlrequestLoginGuidance mirrors cockpit-tools request_login_guidance: POST
// {loginTraceID, login_trace_id} to every platform endpoint until one returns
// a parseable LoginHost. (CN additionally degrades to a default host; Intl
// surfaces the aggregated error, matching cockpit-tools.)
// intlLoginGuidanceProbeTimeout bounds each GetLoginGuidance endpoint probe.
// 5s per endpoint (parity with the CN flow, v0.12.9): guidance runs
// SYNCHRONOUSLY inside the browser-facing auth-url request — at 15s the
// 3-endpoint fallback blocked up to 45s when the upstream is unreachable, and
// the management UI's axios call died with ERR_NETWORK ("网络错误") long
// before any response. Total failure degrades to the default Intl login host,
// so a short timeout costs nothing but latency (v0.12.10: the CN flow got
// this fix but INTL was missed — that is why INTL logins kept failing).
var intlLoginGuidanceProbeTimeout = 5 * time.Second

func intlrequestLoginGuidance(cn bool, loginTraceID string) (loginHost string, lastErr error) { //nolint:revive // cn kept for signature parity with requestLoginGuidance; INTL always passes false
	endpoints := intlLoginGuidanceURLs
	var errs []string
	body, _ := json.Marshal(map[string]any{
		"loginTraceID":   loginTraceID,
		"login_trace_id": loginTraceID,
	})
	client := &http.Client{Timeout: intlLoginGuidanceProbeTimeout}
	for _, endpoint := range endpoints {
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s => %v", endpoint, err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Trae/"+intlOauthPluginVersion+" antigravity-cockpit-tools")
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s => %v", endpoint, err))
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			errs = append(errs, fmt.Sprintf("%s => HTTP %d (body=%s)", endpoint, resp.StatusCode, intltruncate(string(raw), 120)))
			continue
		}
		if host := intlextractLoginHost(raw); host != "" {
			return host, nil
		}
		errs = append(errs, fmt.Sprintf("%s => response missing LoginHost (body=%s)", endpoint, intltruncate(string(raw), 120)))
	}
	// Total failure degrades to the default Intl login host instead of
	// erroring out (cockpit-tools parity: CN does the same with its default).
	// The guidance call is best-effort — its only job is picking the login
	// page domain; the actual login happens in the user's browser.
	log.Printf("[Trae OAuth] intl login guidance failed (%s) — falling back to default login host %s",
		strings.Join(errs, " | "), intlOAuthDefaultHost)
	return intlOAuthDefaultHost, nil
}

// intlexchangeTokenCandidates tries each candidate URL in turn for the auth-code
// ExchangeToken call and returns the first 2xx JSON body that carries an
// access token (any of the field names cockpit-tools accepts). Every failure
// is recorded so the caller can surface a meaningful error.
func intlexchangeTokenCandidates(urls []string, body []byte) (raw []byte, err error) {
	var errs []string
	client := &http.Client{Timeout: 30 * time.Second}
	for _, u := range urls {
		req, rerr := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s => %v", u, rerr))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-cloudide-token", "")
		req.Header.Set("User-Agent", "Trae/"+intlOauthPluginVersion+" antigravity-cockpit-tools")
		resp, rerr := client.Do(req)
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s => %v", u, rerr))
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			// 404 page not found and friends: try the next candidate.
			errs = append(errs, fmt.Sprintf("%s => HTTP %d (body=%s)", u, resp.StatusCode, intltruncate(string(data), 120)))
			continue
		}
		// www.* hosts return an HTML page with 200 — treat a JSON parse
		// failure (or a body without an access token) as a failed candidate.
		if at := intlextractExchangeAccessToken(data); at != "" {
			return data, nil
		}
		errs = append(errs, fmt.Sprintf("%s => no access token (body=%s)", u, intltruncate(string(data), 120)))
	}
	return nil, fmt.Errorf("ExchangeToken failed: %s", strings.Join(errs, " | "))
}

// intlextractExchangeAccessToken mirrors cockpit-tools extract_exchange_access_token:
// checks Result/{AccessToken,accessToken,Token,token} plus top-level variants.
func intlextractExchangeAccessToken(raw []byte) string {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	tokenKeys := []string{"AccessToken", "accessToken", "Token", "token"}
	if sub, ok := top["Result"].(map[string]any); ok {
		for _, k := range tokenKeys {
			if s := intljsonString(sub[k]); s != "" {
				return s
			}
		}
	}
	if sub, ok := top["result"].(map[string]any); ok {
		for _, k := range tokenKeys {
			if s := intljsonString(sub[k]); s != "" {
				return s
			}
		}
	}
	for _, k := range tokenKeys {
		if s := intljsonString(top[k]); s != "" {
			return s
		}
	}
	return ""
}

// intljsonString returns v as a string if it is a JSON string; empty otherwise.
func intljsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// intlbuildOfficialDeviceInfo builds the DeviceInfo for ExchangeToken, mirroring
// cockpit-tools build_official_device_info (trae_oauth.rs:2087-2118). The
// previous implementation sent a bare {DeviceId, MachineId} map with RANDOM
// hex ids — mismatching the machine/device ids embedded in the login URL and
// breaking the device-binding expectations upstream. DevicePublicKey stays
// empty (no device key pair), matching traework2api's behavior.
func intlbuildOfficialDeviceInfo(deviceID, machineID, platformCode, deviceName, deviceBrand, appVersion, deviceType, osVersion string) map[string]any {
	return map[string]any{
		"DeviceID":        deviceID,
		"MachineID":       machineID,
		"PlatformCode":    platformCode,
		"DeviceType":      "PC",
		"DeviceName":      deviceName,
		"DeviceModel":     deviceBrand,
		"ClientVersion":   appVersion,
		"DevicePublicKey": "",
		"DeviceBrand":     deviceBrand,
		"DeviceCPU":       "",
		"OSInfo":          deviceType,
		"OSVersion":       osVersion,
	}
}
