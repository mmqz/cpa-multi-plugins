// oauth_callback.go: CPA-host-aware OAuth callback plumbing for the merged
// trae plugin (v0.12.4).
//
// v0.12.1 shipped a 127.0.0.1:0 listener bound INSIDE the CPA process with
// that ephemeral URL as auth_callback_url; the browser redirect then failed
// with "无法连接到 127.0.0.1:46837" whenever the listener was gone or the
// browser was not on the CPA host machine (remote / docker deployments).
// v0.12.2/3 hardened the listener (loop-Accept, callback_bind/public_host
// knobs) but still required manual configuration for remote deployments.
//
// v0.12.4 (this file): when CPA supplies BaseURL via auth.login.start
// (pluginapi.AuthLoginStartRequest — its own oauth-callback endpoint), the
// callback targets this plugin's UNAUTHENTICATED resource route
// /v0/resource/plugins/trae/oauth_callback on the SAME origin the user is
// already browsing — zero-config, browser-reachable for local/LAN/docker.
// The full upstream query (authCode / refreshToken / loginHost) reaches the
// plugin unchanged. The configured local listener stays as the stand-alone
// fallback, and PollLogin additionally consumes the host-written
// .oauth-<provider>-<state>.oauth file as a last resort.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginHostContext carries the host-provided login context parsed from an
// auth.login.start request. pluginapi types have no json tags, so the wire
// format uses capitalized Go field names.
type loginHostContext struct {
	BaseURL string
	AuthDir string
}

// parseLoginHostContext extracts BaseURL / Host.AuthDir from the raw
// auth.login.start request payload. Missing fields yield empty strings and
// the caller falls back to the configured local-listener flow.
func parseLoginHostContext(request []byte) loginHostContext {
	var out loginHostContext
	if len(request) == 0 {
		return out
	}
	var req struct {
		BaseURL string `json:"BaseURL"`
		Host    struct {
			AuthDir string `json:"AuthDir"`
		} `json:"Host"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return out
	}
	out.BaseURL = strings.TrimSpace(req.BaseURL)
	out.AuthDir = strings.TrimSpace(req.Host.AuthDir)
	return out
}

// resourceCallbackPath is the plugin resource route serving the OAuth
// callback (registered in managementRegistration WITHOUT a Menu label: the
// host skips Menu-less resources in its UI menu list, so the route is
// reachable without adding a second sidebar entry).
const resourceCallbackPath = "/v0/resource/plugins/" + providerName + "/oauth_callback"

// resourceCallbackURL derives the browser-reachable callback URL from the
// host's callback base URL: same scheme, host and port as the CPA server the
// user is browsing, but pointed at the plugin's own resource route so the
// full upstream query reaches the plugin.
func resourceCallbackURL(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + resourceCallbackPath
}

// closeListener closes a login listener that may be nil (resource-callback
// flows carry no local listener).
func closeListener(ln netListener) {
	if ln != nil {
		_ = ln.Close()
	}
}

// completeLogin marks a cn/solo login flow as completed. Idempotent and
// goroutine-safe: the local-listener flow closes lc.done in acceptCallback,
// while resource-callback / host-file flows complete here.
func completeLogin(lc *loginCtx) {
	lc.doneOnce.Do(func() {
		ch := make(chan struct{})
		close(ch)
		lc.done = ch
	})
}

// intlCompleteLogin is the intl-flow counterpart of completeLogin.
func intlCompleteLogin(lc *intlloginCtx) {
	lc.doneOnce.Do(func() {
		ch := make(chan struct{})
		close(ch)
		lc.done = ch
	})
}

// readHostCallbackFile reads the OAuth callback the CPA host persisted for a
// plugin login: <AuthDir>/.oauth-<provider>-<state>.oauth with body
// {"code":...,"state":...,"error":...} (CLIProxyAPI
// internal/api/handlers/management/oauth_sessions.go writeOAuthCallbackFile).
// Returns ok=false when there is nothing to read (no AuthDir or the callback
// has not been delivered yet). The file is removed after reading so it is
// consumed exactly once.
func readHostCallbackFile(authDir, state string) (code, cbErr string, ok bool) {
	if authDir == "" || state == "" {
		return "", "", false
	}
	path := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", providerName, state))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	_ = os.Remove(path)
	var payload struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", true
	}
	return strings.TrimSpace(payload.Code), strings.TrimSpace(payload.Error), true
}

// callbackParams is the parsed upstream callback query.
type callbackParams struct {
	err          error
	loginHost    string
	refreshToken string
	authCode     string
}

// parseCallbackParams applies the cockpit-tools callback parameter
// precedence for the resource callback route.
func parseCallbackParams(vals url.Values) callbackParams {
	var p callbackParams
	for _, k := range []string{"error", "error_code", "err", "errorCode"} {
		if ev := vals.Get(k); ev != "" {
			p.err = fmt.Errorf("oauth callback error: %s=%s", k, ev)
			return p
		}
	}
	if ir := vals.Get("isRedirect"); ir == "false" {
		p.err = fmt.Errorf("oauth callback: isRedirect=false")
		return p
	}
	for _, k := range []string{"loginHost", "login_host", "LoginHost", "host", "consoleHost"} {
		if v := vals.Get(k); v != "" {
			p.loginHost = v
			break
		}
	}
	for _, k := range []string{"refreshToken", "refresh_token", "RefreshToken", "refresh-token"} {
		if v := vals.Get(k); v != "" {
			p.refreshToken = v
			break
		}
	}
	for _, k := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
		if v := vals.Get(k); v != "" {
			p.authCode = v
			break
		}
	}
	if p.authCode == "" {
		for _, k := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			if v := vals.Get(k); v != "" {
				if ac := extractAuthCodeFromAuthCodeInfo(v); ac != "" {
					p.authCode = ac
					break
				}
			}
		}
	}
	return p
}

// handleOAuthCallbackResource serves GET /v0/resource/plugins/trae/oauth_callback —
// the redirect target embedded in the Trae verification URI. The full
// upstream query arrives in req.Query; the state token selects the in-flight
// flow (cn/solo flows live in loginStates, intl flows in intlloginStates —
// matching the poll dispatch's state-location routing).
func handleOAuthCallbackResource(req pluginapi.ManagementRequest) []byte {
	vals := req.Query
	if vals == nil {
		vals = url.Values{}
	}
	state := strings.TrimSpace(vals.Get("state"))
	if state == "" {
		return callbackResultHTML("Login failed", "missing state parameter — please restart the login")
	}
	// CN/SOLO flow.
	if v, ok := loginStates.Load(state); ok {
		lc := v.(*loginCtx)
		p := parseCallbackParams(vals)
		lc.loginHost = p.loginHost
		lc.refreshToken = p.refreshToken
		lc.authCode = p.authCode
		lc.err = p.err
		if p.err == nil && p.authCode == "" && p.refreshToken == "" {
			lc.err = fmt.Errorf("oauth callback: no authCode/refreshToken in callback params")
		}
		completeLogin(lc)
		if lc.err != nil {
			return callbackResultHTML("Login failed", lc.err.Error())
		}
		return callbackResultHTML("Login successful", "You can close this window now.")
	}
	// Intl flow.
	if v, ok := intlloginStates.Load(state); ok {
		lc := v.(*intlloginCtx)
		p := parseCallbackParams(vals)
		lc.loginHost = p.loginHost
		lc.authCode = p.authCode
		lc.err = p.err
		if p.err == nil && p.authCode == "" {
			lc.err = fmt.Errorf("oauth callback: no authCode in callback params")
		}
		intlCompleteLogin(lc)
		if lc.err != nil {
			return callbackResultHTML("Login failed", lc.err.Error())
		}
		return callbackResultHTML("Login successful", "You can close this window now.")
	}
	return callbackResultHTML("Login failed", "unknown or expired login state — please restart the login")
}

// callbackResultHTML renders the minimal browser-facing result page.
func callbackResultHTML(title, msg string) []byte {
	return []byte(fmt.Sprintf("<html><head><meta charset=\"utf-8\"><title>%s</title></head><body style=\"font-family:sans-serif;text-align:center;padding-top:60px\"><h2>%s</h2><p>%s</p></body></html>",
		htmlEscape(title), htmlEscape(title), htmlEscape(msg)))
}

// htmlEscape escapes the five XML entities for the callback result page.
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(s)
}
