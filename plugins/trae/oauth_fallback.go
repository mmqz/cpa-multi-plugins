// oauth_fallback.go: multi-endpoint OAuth fallback helpers, ported from
// cockpit-tools trae_oauth.rs to fix "404 page not found" during OAuth login.
//
// Fixes applied (all mirror cockpit-tools behavior):
//   - ensureHTTPSScheme: cockpit-tools ensure_https_url (trae_oauth.rs:1117-1126).
//     GetLoginGuidance returns LoginHost as a BARE domain ("www.trae.cn"); the
//     login URL must get an https:// prefix or the panel/browser resolves it as
//     a RELATIVE URL against the CPA server itself → gin default 404 body
//     "404 page not found".
//   - requestLoginGuidance: cockpit-tools request_login_guidance
//     (trae_oauth.rs:1762-1848) — tries every GetLoginGuidance endpoint in the
//     platform list before failing; CN falls back to the default login host.
//   - candidateAPIOorigins + buildAPIURLs: cockpit-tools candidate_api_origins /
//     build_api_urls (trae_oauth.rs:1994-2037) — derives candidate API origins
//     from the callback loginHost (rewriting www. → api.), then appends the
//     platform defaults. ExchangeToken/GetUserInfo try each URL in turn until
//     one returns an access token, instead of hard-coding a single host.
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
// TRAE_CN_LOGIN_GUIDANCE_URLS / TRAE_LOGIN_GUIDANCE_URLS (trae_oauth.rs:36-44).
var (
        traeCNLoginGuidanceURLs = []string{
                "https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
                "https://api.trae.com.cn/cloudide/api/v3/trae/GetLoginGuidance",
                "https://www.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
        }
        traeIntlLoginGuidanceURLs = []string{
                "https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
                "https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
                "https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
        }
)

// Platform default API origins — cockpit-tools TRAE_ACCOUNT_API_ORIGIN_* and
// candidate_api_origins platform branches (trae_oauth.rs:51-56, 2009-2022).
var (
        traeCNDefaultOrigins = []string{
                "https://api.trae.cn",
                "https://api.trae.com.cn",
                "https://www.trae.cn",
        }
        // Intl auth-code ExchangeToken uses the account API origins (cockpit-tools
        // default_account_api_config, trae_oauth.rs:683-688): normal → grow-normal,
        // sg → growsg-normal. api.marscode.com / api.trae.ai are kept as extra
        // fallbacks for resilience.
        traeIntlDefaultOrigins = []string{
                "https://grow-normal.trae.ai",
                "https://growsg-normal.trae.ai",
                "https://api.marscode.com",
                "https://api.trae.ai",
        }
)

// ensureHTTPSScheme mirrors cockpit-tools ensure_https_url: a bare domain such
// as "www.trae.cn" gets an https:// prefix. Without this the verification URI
// returned to the panel is scheme-less and the browser treats it as a relative
// path on the CPA server → "404 page not found".
func ensureHTTPSScheme(raw string) string {
        normalized := strings.TrimSpace(raw)
        if normalized == "" {
                return ""
        }
        if strings.HasPrefix(normalized, "http://") || strings.HasPrefix(normalized, "https://") {
                return normalized
        }
        return "https://" + strings.TrimLeft(normalized, "/")
}

// candidateAPIOorigins mirrors cockpit-tools candidate_api_origins:
//  1. origin derived from the callback loginHost (scheme + host);
//  2. if that host starts with "www.", also the api. variant;
//  3. platform default origins.
//
// Results are de-duplicated preserving order (dedup_keep_order).
func candidateAPIOorigins(loginHost string, cn bool) []string {
        var origins []string
        if u := ensureHTTPSScheme(loginHost); u != "" {
                if host := hostOnly(u); host != "" {
                        origins = append(origins, schemeOf(u)+"://"+host)
                        if stripped := strings.TrimPrefix(host, "www."); stripped != host {
                                origins = append(origins, schemeOf(u)+"://api."+stripped)
                        }
                }
        }
        if cn {
                origins = append(origins, traeCNDefaultOrigins...)
        } else {
                origins = append(origins, traeIntlDefaultOrigins...)
        }
        return dedupKeepOrder(origins)
}

// buildAPIURLs maps candidate origins onto an endpoint path.
func buildAPIURLs(loginHost, path string, cn bool) []string {
        var urls []string
        for _, origin := range candidateAPIOorigins(loginHost, cn) {
                urls = append(urls, strings.TrimRight(origin, "/")+path)
        }
        return dedupKeepOrder(urls)
}

func hostOnly(u string) string {
        rest := u
        if i := strings.Index(rest, "://"); i >= 0 {
                rest = rest[i+3:]
        }
        if i := strings.IndexAny(rest, "/?#"); i >= 0 {
                rest = rest[:i]
        }
        return rest
}

func schemeOf(u string) string {
        if i := strings.Index(u, "://"); i > 0 {
                return u[:i]
        }
        return "https"
}

func dedupKeepOrder(values []string) []string {
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

// requestLoginGuidance mirrors cockpit-tools request_login_guidance: POST
// {loginTraceID, login_trace_id} to every platform endpoint until one returns
// a parseable LoginHost. On CN, a total failure falls back to the default
// login host instead of erroring out (cockpit-tools trae_oauth.rs:1834-1841).
func requestLoginGuidance(cn bool, loginTraceID string) (loginHost string, lastErr error) {
        endpoints := traeIntlLoginGuidanceURLs
        if cn {
                endpoints = traeCNLoginGuidanceURLs
        }
        var errs []string
        body, _ := json.Marshal(map[string]any{
                "loginTraceID":   loginTraceID,
                "login_trace_id": loginTraceID,
        })
        // 5s per endpoint: GetLoginGuidance runs SYNCHRONOUSLY inside the
        // browser-facing auth-url request. At 15s the 3-endpoint fallback could
        // block for 45s when the upstream is unreachable (e.g. overseas CPA
        // deployments) — the management UI's axios call then died with
        // ERR_NETWORK ("网络错误") long before the request ever returned. Total
        // failure degrades to the default login host, so a short timeout costs
        // nothing but latency (v0.12.9).
        client := &http.Client{Timeout: 5 * time.Second}
        for _, endpoint := range endpoints {
                req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
                if err != nil {
                        errs = append(errs, fmt.Sprintf("%s => %v", endpoint, err))
                        continue
                }
                req.Header.Set("Content-Type", "application/json")
                req.Header.Set("Accept", "application/json")
                req.Header.Set("User-Agent", "Trae/"+oauthPluginVersion+" antigravity-cockpit-tools")
                resp, err := client.Do(req)
                if err != nil {
                        errs = append(errs, fmt.Sprintf("%s => %v", endpoint, err))
                        continue
                }
                raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
                resp.Body.Close()
                if resp.StatusCode >= 400 {
                        errs = append(errs, fmt.Sprintf("%s => HTTP %d (body=%s)", endpoint, resp.StatusCode, truncate(string(raw), 120)))
                        continue
                }
                if host := extractLoginHost(raw); host != "" {
                        return host, nil
                }
                errs = append(errs, fmt.Sprintf("%s => response missing LoginHost (body=%s)", endpoint, truncate(string(raw), 120)))
        }
        if cn {
                // cockpit-tools: CN degrades to the default login host rather than
                // blocking the login flow (trae_oauth.rs:1834-1841).
                log.Printf("[Trae OAuth] login guidance failed (%s) — falling back to default login host %s",
                        strings.Join(errs, " | "), oauthDefaultHost)
                return oauthDefaultHost, nil
        }
        return "", fmt.Errorf("login guidance failed: %s", strings.Join(errs, " | "))
}

// exchangeTokenCandidates tries each candidate URL in turn for the auth-code
// ExchangeToken call and returns the first 2xx JSON body that carries an
// access token (any of the field names cockpit-tools accepts). Every failure
// is recorded so the caller can surface a meaningful error.
func exchangeTokenCandidates(urls []string, body []byte) (raw []byte, err error) {
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
                req.Header.Set("User-Agent", "Trae/"+oauthPluginVersion+" antigravity-cockpit-tools")
                resp, rerr := client.Do(req)
                if rerr != nil {
                        errs = append(errs, fmt.Sprintf("%s => %v", u, rerr))
                        continue
                }
                data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
                resp.Body.Close()
                if resp.StatusCode >= 400 {
                        // 404 page not found and friends: try the next candidate.
                        errs = append(errs, fmt.Sprintf("%s => HTTP %d (body=%s)", u, resp.StatusCode, truncate(string(data), 120)))
                        continue
                }
                // www.* hosts return an HTML page with 200 — treat a JSON parse
                // failure (or a body without an access token) as a failed candidate.
                if at := extractExchangeAccessToken(data); at != "" {
                        return data, nil
                }
                errs = append(errs, fmt.Sprintf("%s => no access token (body=%s)", u, truncate(string(data), 120)))
        }
        return nil, fmt.Errorf("ExchangeToken failed: %s", strings.Join(errs, " | "))
}

// extractExchangeAccessToken mirrors cockpit-tools extract_exchange_access_token:
// checks Result/{AccessToken,accessToken,Token,token} plus top-level variants.
func extractExchangeAccessToken(raw []byte) string {
        var top map[string]any
        if err := json.Unmarshal(raw, &top); err != nil {
                return ""
        }
        tokenKeys := []string{"AccessToken", "accessToken", "Token", "token"}
        if sub, ok := top["Result"].(map[string]any); ok {
                for _, k := range tokenKeys {
                        if s := jsonString(sub[k]); s != "" {
                                return s
                        }
                }
        }
        if sub, ok := top["result"].(map[string]any); ok {
                for _, k := range tokenKeys {
                        if s := jsonString(sub[k]); s != "" {
                                return s
                        }
                }
        }
        for _, k := range tokenKeys {
                if s := jsonString(top[k]); s != "" {
                        return s
                }
        }
        return ""
}
