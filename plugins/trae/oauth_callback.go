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
        "time"

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
        rememberAuthDir(out.AuthDir) // v0.12.17: restore-path AuthDir warm
        return out
}

// resourceCallbackPath is the plugin resource route serving the OAuth
// callback (registered in managementRegistration WITHOUT a Menu label: the
// host skips Menu-less resources in its UI menu list, so the route is
// reachable without adding a second sidebar entry).
const resourceCallbackPath = "/v0/resource/plugins/" + providerName + "/oauth_callback"

// resourceSubmitPath is the paste-to-complete fallback route (v0.12.16).
// Referenced by start-login log guidance and intl Metadata — the previous
// "resourceCallbackPath + \"_submit\"" spelling produced a non-existent
// /oauth_callback_submit path in user-facing guidance.
const resourceSubmitPath = "/v0/resource/plugins/" + providerName + "/oauth_submit"

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

// resolveCallbackState determines which in-flight login a callback request
// belongs to (v0.12.12). The REAL Trae authorization-page redirect does NOT
// echo a "state" parameter — the plugin never sends one. It echoes the login
// session id the plugin DID send: login_trace_id (cockpit-tools parses
// loginTraceID / loginTraceId / login_trace_id / trace_id out of the
// callback payload — TraeCallbackPayload.login_trace_id, trae_oauth.rs:1671).
// The old handler required "state" and rejected every real redirect with
// "missing state parameter" — user-visible as "收不到回调链接". The test
// suite masked this because its simulated callbacks always passed state.
// Resolution order:
//  1. "state" — the host-driven oauth-callback convention (kept for compat).
//  2. login_trace_id variants — the real Trae redirect echo.
//  3. exactly-one live login across both maps — mirrors cockpit-tools'
//     port-scoped listener, where the callback port IS the login identity.
func resolveCallbackState(vals url.Values) string {
        if s := strings.TrimSpace(vals.Get("state")); s != "" {
                return s
        }
        for _, k := range []string{"loginTraceID", "loginTraceId", "login_trace_id", "trace_id"} {
                if v := strings.TrimSpace(vals.Get(k)); v != "" {
                        return v
                }
        }
        return singleInflightLoginState()
}

// singleInflightLoginState returns the state key when exactly one live
// login (cn/solo or intl) is in flight, and "" otherwise. Expired states
// don't count — they can no longer complete.
func singleInflightLoginState() string {
        var live []string
        now := time.Now()
        loginStates.Range(func(k, v any) bool {
                lc := v.(*loginCtx)
                if now.Before(lc.expires) {
                        live = append(live, k.(string))
                }
                return true
        })
        intlloginStates.Range(func(k, v any) bool {
                lc := v.(*intlloginCtx)
                if now.Before(lc.expires) {
                        live = append(live, k.(string))
                }
                return true
        })
        if len(live) == 1 {
                return live[0]
        }
        return ""
}

// handleOAuthCallbackResource serves GET /v0/resource/plugins/trae/oauth_callback —
// the redirect target embedded in the Trae verification URI. The full
// upstream query arrives in req.Query; the login is selected via
// resolveCallbackState (cn/solo flows live in loginStates, intl flows in
// intlloginStates — matching the poll dispatch's state-location routing).
func handleOAuthCallbackResource(req pluginapi.ManagementRequest) []byte {
        vals := req.Query
        if vals == nil {
                vals = url.Values{}
        }
        // Bare hit with NO params at all (browser prefetch, user pasting the
        // plain callback URL, favicon probes): answer pending and keep the
        // in-flight login untouched — cockpit-tools keeps waiting on an empty
        // query (trae_oauth.rs:1611-1624).
        if len(vals) == 0 {
                return callbackResultHTML("Waiting for authorization",
                        "This URL is the Trae OAuth redirect target. Complete the login in the authorization window.")
        }
        state := resolveCallbackState(vals)
        // v0.12.17: resolve miss → try the disk-persisted pending login. Covers the
        // restart / process-bounce window (docker restart between click-登录 and
        // paste) and the bare-URL paste after a restart. A non-empty state must
        // match the record (PKCE pair ownership); empty adopts the single record.
        if !stateIsLive(state) {
                if restored := restorePendingLoginState(state); restored != "" {
                        state = restored
                }
        }
        if state == "" {
                return callbackResultHTML("Login failed", "missing state parameter — 回调链接缺少状态参数，请回到面板重新点「登录」，完成授权后立即粘贴新链接 (please restart the login)")
        }
        // CN/SOLO flow.
        if v, ok := loginStates.Load(state); ok {
                lc := v.(*loginCtx)
                p := parseCallbackParams(vals)
                if p.loginHost != "" {
                        lc.loginHost = p.loginHost
                } // else keep the start-login host (cockpit-tools fallback_login_host)
                lc.refreshToken = p.refreshToken
                lc.authCode = p.authCode
                lc.err = p.err
                if p.err == nil && p.authCode == "" && p.refreshToken == "" {
                        lc.err = fmt.Errorf("oauth callback: no authCode/refreshToken in callback params — 回调链接中没有授权码参数，请从浏览器地址栏完整复制（含 ? 后所有参数），不要复制被截断的链接")
                }
                completeLogin(lc)
                if lc.err != nil {
                        if lc.restored {
                                // Host poll channel is dead for a restored
                                // login — settle the outcome here so a re-paste
                                // explains what happened.
                                recordLoginOutcome(state, false, lc.err.Error())
                                clearPendingLogin(lc.authDir)
                                loginStates.Delete(state)
                        }
                        return callbackResultHTML("Login failed", lc.err.Error())
                }
                if lc.restored {
                        lc.restored = false // spawn once per restored login
                        go selfCompleteRestoredCN(lc)
                }
                return callbackResultHTML("Login successful", "You can close this window now.")
        }
        // Intl flow.
        if v, ok := intlloginStates.Load(state); ok {
                lc := v.(*intlloginCtx)
                p := parseCallbackParams(vals)
                if p.loginHost != "" {
                        lc.loginHost = p.loginHost
                } // else keep the start-login host
                lc.authCode = p.authCode
                lc.err = p.err
                if p.err == nil && p.authCode == "" {
                        lc.err = fmt.Errorf("oauth callback: no authCode in callback params — 回调链接中没有授权码参数，请复制浏览器地址栏完整链接（含 ? 后所有参数）")
                }
                intlCompleteLogin(lc)
                if lc.err != nil {
                        if lc.restored {
                                recordLoginOutcome(state, false, lc.err.Error())
                                clearPendingLogin(lc.authDir)
                                intlloginStates.Delete(state)
                        }
                        return callbackResultHTML("Login failed", lc.err.Error())
                }
                if lc.restored {
                        lc.restored = false // spawn once per restored login
                        go selfCompleteRestoredIntl(lc)
                }
                return callbackResultHTML("Login successful", "You can close this window now.")
        }
        // v0.12.17: the login already finished (panel poll drained the state, or
        // the auto-callback completed it before the user pasted). Answer from the
        // outcome cache instead of the confusing "unknown state" page.
        if o, ok := lookupLoginOutcome(state); ok {
                if o.ok {
                        return callbackResultHTML("Login successful",
                                "该回调链接的登录此前已完成，凭证已保存，无需重复粘贴。This login was already completed earlier — the credential is saved.")
                }
                return callbackResultHTML("Login failed",
                        "该链接的登录已结束，原错误："+o.msg+" — 请重新开始登录并粘贴新链接。")
        }
        return callbackResultHTML("Login failed",
                "unknown or expired login state — 登录状态不存在或已过期。常见原因：① 粘贴的是旧链接；② 登录开始已超过 15 分钟；③ 服务在登录途中重启过。请回到面板重新点「登录」，完成授权后立即复制地址栏新链接粘贴 (please restart the login and paste the NEW link)")
}

// handleOAuthSubmitResource serves GET/POST /v0/resource/plugins/trae/oauth_submit —
// the paste-to-complete fallback (v0.12.16). When the browser cannot reach the
// local listener (Docker without the port published, or the plugin runs on a
// remote host), the redirect to http://127.0.0.1:<port>/authorize fails and the
// FULL callback URL stays in the browser address bar. Pasting that URL here —
// GET ?cb_url=<url-encoded> or POST {"url":"..."} — replays it through the
// exact same resolution path as a real callback hit (resolveCallbackState +
// authCodeInfo extraction), so the login completes without manual prefix
// surgery. The host's own oauth-callback paste endpoint cannot serve plugins:
// it demands a pre-registered session state that plugin logins never create.
func handleOAuthSubmitResource(req pluginapi.ManagementRequest) []byte {
        raw := strings.TrimSpace(req.Query.Get("cb_url"))
        if raw == "" && len(req.Body) > 0 {
                var payload struct {
                        URL string `json:"url"`
                }
                if err := json.Unmarshal(req.Body, &payload); err == nil {
                        raw = strings.TrimSpace(payload.URL)
                }
        }
        if raw != "" {
                var truncMsg string
                raw, truncMsg = sanitizePastedCallback(raw)
                if truncMsg != "" {
                        return callbackResultHTML("Invalid callback URL", truncMsg)
                }
        }
        if raw == "" {
                return callbackResultHTML("Missing callback URL",
                        "Paste the full failed-redirect URL (the address-bar http://127.0.0.1:<port>/authorize?... link) — GET ?cb_url=<encoded> or POST {\"url\":\"...\"}.")
        }
        u, errParse := url.Parse(raw)
        if errParse != nil || len(u.Query()) == 0 {
                return callbackResultHTML("Invalid callback URL",
                        "The pasted text is not a parseable URL with query parameters. Copy the full address-bar URL shown after the authorization redirect fails.")
        }
        // Replay through the shared resolution path — cn/solo/intl states all
        // resolve here (loginStates + intlloginStates via resolveCallbackState).
        return handleOAuthCallbackResource(pluginapi.ManagementRequest{Query: u.Query()})
}

// sanitizePastedCallback normalizes the pasted redirect URL. Users paste from
// Firefox/Chrome address bars, IM windows and markdown quotes — tolerate
// wrapping quotes / CJK quotes, bare query strings and scheme-less host
// forms, and reject ellipsis-truncated copies with an explicit message
// (v0.12.17: a truncated authCodeInfo used to surface as the misleading
// "no authCode/refreshToken in callback params").
func sanitizePastedCallback(raw string) (clean, truncErr string) {
        s := strings.TrimSpace(raw)
        s = strings.Trim(s, "\"'`“”‘’<>()[]{}《》「」『』【】")
        s = strings.TrimSpace(strings.TrimRight(s, ",;，；"))
        if s == "" {
                return "", ""
        }
        if strings.Contains(s, "…") || strings.Contains(s, "...") {
                return "", "链接不完整（检测到省略号）— 请点击浏览器地址栏 → Ctrl+A 全选 → 复制完整链接再粘贴。The pasted link is TRUNCATED (ellipsis detected) — copy the FULL address-bar URL."
        }
        if !strings.Contains(s, "://") {
                switch {
                case strings.HasPrefix(s, "?"):
                        s = "http://127.0.0.1/authorize" + s
                case strings.Contains(s, "/authorize"):
                        s = "http://" + s
                case strings.Contains(s, "="):
                        s = "http://127.0.0.1/authorize?" + s
                }
        }
        return s, ""
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
