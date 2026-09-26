// management.go implements mimo's management surface: one browser-facing
// resource route (/oauth_submit) that completes a login whose loopback
// redirect failed. The sk-lane OAuth starts a loopback callback server on the
// HOST machine (127.0.0.1:<random port>) — when CPA runs on a remote server
// (or in Docker without the port published), the browser's redirect to
// http://localhost:<port>/auth?u=<blob> never lands, and the login pends until
// TTL while the user stares at a dead address-bar URL. The paste-to-complete
// page replays that failed redirect through the exact same decryption path,
// so the login completes without the callback ever reaching the host.
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Registration shapes mirror the other plugins' local declarations (zcode
// panel.go, trae management.go): Routes are host-proxied management APIs,
// Resources are browser-facing pages under /v0/resource/plugins/<provider>.
type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// mimoManagementRegistration advertises the login surfaces. The oauth_submit
// resource carries a Menu label ("Mimo", the user's explicit choice) — the
// management panel renders labeled plugin resources as sidebar navigation
// entries (menu → /plugin-pages/<id>/<idx> iframe on the CPA origin via
// apiBase-prefixed iframe src), giving remote/Docker users a DISCOVERABLE
// paste fallback. Since v0.2.9 StartLogin passes the platform authorize URL
// straight through, this page is the recovery surface for dead localhost
// redirects, no longer the login entry itself.
func mimoManagementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Resources: []resourceRoute{
			{Path: "/oauth_submit", Menu: "Mimo", Description: "MiMo login fallback: paste the full failed redirect URL (http://localhost:…/auth?u=…) here — or GET ?cb_url=<url-encoded> — to finish a login whose localhost redirect never landed (remote/Docker hosts). Shows the live authorize link while a login is pending."},
		},
	}
}

// handleMimoManagement dispatches the plugin's single management surface.
func handleMimoManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	resPrefix := "/v0/resource/plugins/" + providerName
	if (req.Method == http.MethodGet || req.Method == http.MethodPost) && path == resPrefix+"/oauth_submit" {
		return okEnvelope(mgmtHTMLResponse(handleMimoOAuthSubmit(req)))
	}
	if (req.Method == http.MethodGet || req.Method == http.MethodPost) && path == resPrefix+"/login_gate" {
		return okEnvelope(mgmtHTMLResponse(handleMimoLoginGate(req)))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

const mimoSubmitFormHTML = `<p>本页是 MiMo 登录的兜底粘贴页：在 CPA 点「登录」后约 5 秒内，本页会自动变为引导页（含「重新打开小米授权页」按钮）。手动粘贴兜底：</p>
<ol>
<li>在 CPA 点「登录」，浏览器直接打开小米 OAuth 授权页并完成账号授权；</li>
<li>浏览器最后会跳转 <code>http://localhost:…/auth?u=…</code> 并打开失败（远程 / Docker 部署常态）——复制地址栏<b>完整链接</b>（登录 6 分钟内有效）；</li>
<li>粘贴到下面并提交（提交后凭证直接保存，无需等待）。</li>
</ol>
<form method="GET" action=""><input name="cb_url" style="width:78%" placeholder="http://localhost:…/auth?u=…"> <button>完成登录</button></form>`

// mimoGateFormHTML is the paste box shared by the gate page variants; the
// relative action resolves against /v0/resource/plugins/mimo/login_gate →
// /v0/resource/plugins/mimo/oauth_submit (same origin the panel serves from).
const mimoGateFormHTML = `<form method="GET" action="oauth_submit"><input name="cb_url" style="width:78%" placeholder="http://localhost:…/auth?u=…"> <button>完成登录</button></form>`

// mimoGateStaleHTML renders when the requested state is missing/expired:
// every login start rotates the state (v0.2.7 single-active policy), so old
// gate tabs must not look live.
const mimoGateStaleHTML = `<p>该登录会话不存在或已失效——每次在 CPA 点「登录」都会生成新会话，旧引导页随之作废。</p>
<p>请回到 CPA 重新发起 MiMo 登录并使用新打开的引导页；若手头已有失败页地址栏的完整链接（<code>http://localhost:…/auth?u=…</code>），且登录是最近 6 分钟内发起的，可直接在下方粘贴提交。</p>` + mimoGateFormHTML

// handleMimoLoginGate serves GET /v0/resource/plugins/mimo/login_gate — the
// registration panel (v0.2.7). StartLogin now RETURNS this page's URL
// (relative → the panel opens it against the CPA origin), so the login flow
// itself lands here: step 1 opens the platform authorize page in a new tab,
// step 2 explains the localhost redirect, step 3 is the paste-to-complete box.
// This closes the Docker/remote gap where the callback can never arrive and
// there was no visible place to submit it (user report 2026-09-26).
func handleMimoLoginGate(req pluginapi.ManagementRequest) []byte {
	state := strings.TrimSpace(req.Query.Get("state"))
	if state != "" {
		if v, ok := loginStates.Load(state); ok {
			lc := v.(*loginCtx)
			if time.Now().Before(lc.expires) && lc.authorizeURL != "" {
				return mimoSubmitPage("MiMo 登录引导", mimoGateLiveBody(lc))
			}
		}
	}
	return mimoSubmitPage("MiMo 登录引导", mimoGateStaleHTML)
}

// mimoPendingLogin returns the newest unexpired pending login, if any. The
// single-active policy (v0.2.7) keeps at most one alive; the scan is
// defensive against future multi-flow shapes. Expired states are skipped,
// not deleted — poll semantics own their lifecycle.
func mimoPendingLogin() *loginCtx {
	var found *loginCtx
	loginStates.Range(func(key, value any) bool {
		lc, ok := value.(*loginCtx)
		if !ok || time.Now().After(lc.expires) {
			return true
		}
		if found == nil || lc.startedAt > found.startedAt {
			found = lc
		}
		return true
	})
	return found
}

// mimoGateLiveBody renders the guided flow for a live pending login: the
// step list, a redundant authorize link (normally clicking Login in CPA
// already opened the real platform OAuth page — v0.2.9 passthrough), the
// auth record name and the shared paste box. Shared by the stateful gate
// page (v0.2.7) and the state-aware oauth_submit menu page (v0.2.8).
func mimoGateLiveBody(lc *loginCtx) string {
	return fmt.Sprintf(`<ol>
<li>在 CPA 点「登录」后浏览器会<strong>直接打开小米 OAuth 授权页</strong>（通常无需再点下面的按钮）；本页请保持打开。</li>
<li>授权后浏览器会跳转 <code>http://localhost:…/auth?u=…</code>：本机部署会自动完成；远程 / Docker 部署该页打不开——复制地址栏<b>完整链接</b>（本次登录 6 分钟内有效）。</li>
<li>把完整链接粘贴到下面并点「完成登录」——凭证会直接保存进 CPA（登录窗口若仍开着，也会随之自动完成）。</li>
</ol>
<p><a href="%s" target="_blank" rel="noopener noreferrer" style="display:inline-block;background:#ff6900;color:#fff;padding:10px 22px;border-radius:8px;text-decoration:none;font-weight:600">重新打开小米授权页</a></p>
<p>授权记录名 <code>%s</code></p>`,
		html.EscapeString(lc.authorizeURL), html.EscapeString(lc.keyName)) + mimoGateFormHTML
}

// handleMimoOAuthSubmit serves GET/POST /v0/resource/plugins/mimo/oauth_submit.
// GET ?cb_url=<url-encoded failed redirect URL> (or the page form, which
// submits the same param; POST {"url":"..."} also accepted) replays the failed
// redirect through the pending login's decryption path.
func handleMimoOAuthSubmit(req pluginapi.ManagementRequest) []byte {
	raw := strings.TrimSpace(req.Query.Get("cb_url"))
	if raw == "" && len(req.Body) > 0 {
		var payload struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(req.Body, &payload); err == nil {
			raw = strings.TrimSpace(payload.URL)
		}
	}
	blob, truncMsg := mimoExtractBlob(raw)
	if truncMsg != "" {
		return mimoSubmitPage("链接不完整", html.EscapeString(truncMsg))
	}
	if blob == "" {
		// v0.2.8: the menu page is state-aware. With a pending login it IS
		// the guided registration page — the cross-origin-safe entry (the
		// panel renders plugin menu pages on the CPA origin through
		// apiBase-prefixed iframes, while its OAuth dialog window.open's
		// StartLogin's URL raw, which 404s when the panel origin is not the
		// CPA origin). Idle, it keeps the paste instructions and
		// auto-refreshes so a login started afterwards is picked up.
		if lc := mimoPendingLogin(); lc != nil && lc.authorizeURL != "" {
			return mimoSubmitPage("MiMo 登录引导", mimoGateLiveBody(lc))
		}
		return mimoSubmitPageHead("MiMo 登录兜底", mimoIdleMetaRefresh, mimoSubmitFormHTML)
	}
	res, ok := mimoCompletePendingLogin(blob)
	if !ok {
		return mimoSubmitPage("登录未完成", "粘贴的链接无法匹配任何进行中的登录。请确认：① 登录是在 6 分钟内从 CPA 发起的；② 粘贴的是<b>本次</b>登录失败页地址栏的完整链接（含 u= 参数）。回到 CPA 重新点「登录」后再试一次。")
	}
	summary := "密钥已接收"
	if uid := strings.TrimSpace(res.UID); uid != "" {
		summary += "（账号 uid " + html.EscapeString(uid) + "）"
	}
	// v0.2.10: persist HERE, not via the host's next poll — the poll loop
	// is UI-driven (the panel's OAuth page polls every ~3s) and dies with
	// the dialog, so the old "回到登录窗口等下一次轮询；若已关闭就重新登录
	// 一次" advice orphaned an already-authorized key whenever the window
	// had been closed (user report 2026-09-26: the page said 登录完成 but
	// no credential ever landed). A still-open dialog completes normally
	// on its next poll and converges on the same auth file, so the double
	// save replaces one record instead of duplicating it.
	if name, err := persistOAuthLogin(res); err == nil {
		return mimoSubmitPage("登录完成", summary+" —— 凭证已直接保存为 <code>"+html.EscapeString(name)+"</code>。回到 CPA 刷新凭据列表即可看到；登录窗口若仍开着，它也会在下一次轮询时正常完成（同一份凭证，不会重复）。")
	}
	return mimoSubmitPage("登录完成", summary+" —— 已解出凭证并送交 CPA 登录窗口，它会在下一次轮询时自动完成；若窗口已关闭且刷新凭据列表后没有新凭证，请重新点「登录」并再走一次粘贴（上一次已授权的密钥作废属正常）。")
}

// mimoExtractBlob pulls the ?u= payload out of a pasted redirect URL. Users
// paste full address-bar URLs, bare query strings, quoted IM fragments and
// bare blobs — normalize before parsing. A truncated (ellipsis) copy is
// rejected with an explicit message instead of a misleading decrypt failure
// (trae v0.12.17 lesson).
func mimoExtractBlob(raw string) (blob, truncMsg string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	s = strings.Trim(s, "\"'`“”‘’<>()[]{}《》「」『』【】")
	s = strings.TrimSpace(strings.TrimRight(s, ",;，；"))
	if s == "" {
		return "", ""
	}
	if strings.Contains(s, "…") || strings.Contains(s, "...") {
		return "", "检测到省略号 —— 请点击浏览器地址栏 → Ctrl+A 全选 → 复制完整链接再粘贴。The pasted link is TRUNCATED — copy the FULL address-bar URL."
	}
	// A bare blob is accepted directly: base64url alphabet (with optional
	// trailing padding, which the platform may add) — this must run BEFORE the
	// pair-form check, or a padded blob's "=" misroutes it as a query pair.
	if isBase64URLBlob(s) {
		return s, ""
	}
	// A short punctuation-free fragment is treated as a blob too; a wrong
	// guess just fails the GCM tag downstream and lands on the error page.
	if !strings.ContainsAny(s, ":/?=&") {
		return s, ""
	}
	if !strings.Contains(s, "://") {
		switch {
		case strings.HasPrefix(s, "?"):
			s = "http://localhost/" + s
		case strings.Contains(s, "/"):
			// Scheme-less host form ("localhost:51000/auth?u=…") — only reachable
			// after the pair/base64url checks above.
			s = "http://" + s
		case strings.Contains(s, "="):
			s = "http://localhost/?" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(u.Query().Get("u")), ""
}

// isBase64URLBlob reports whether s is pure base64url (the callback blob's
// alphabet) with at most two trailing pad characters. Length floor keeps
// single words ("hello") out of the blob path.
func isBase64URLBlob(s string) bool {
	if len(s) < 16 {
		return false
	}
	pad := strings.TrimRight(s, "=")
	if len(s)-len(pad) > 2 {
		return false
	}
	for _, r := range pad {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// mimoCompletePendingLogin tries every live pending login against the blob.
// The blob embeds its own ephemeral X25519 key, so decryption with a wrong
// static key fails the AES-GCM authentication tag — the first successful
// decrypt is by construction the login the blob belongs to. Delivery goes to
// the pending flow's result channel; the HOST's next PollLogin consumes it
// and performs the state cleanup — this handler must NOT delete the state,
// or the poll would report "unknown state" and break the handshake.
func mimoCompletePendingLogin(blob string) (oauthResult, bool) {
	var res oauthResult
	found := false
	loginStates.Range(func(key, value any) bool {
		lc, ok := value.(*loginCtx)
		if !ok || time.Now().After(lc.expires) {
			return true
		}
		r, err := decryptOAuthBlobFn(lc.privKey, blob)
		if err != nil {
			return true
		}
		res = r
		found = true
		select {
		case lc.result <- res:
		default:
		}
		return false
	})
	return res, found
}

// mimoIdleMetaRefresh drives the idle menu page: a login started after the
// page was opened flips it to the guided flow within ~5s.
const mimoIdleMetaRefresh = `<meta http-equiv="refresh" content="5">`

// mimoSubmitPage renders the minimal browser-facing fallback page. body may
// carry trusted HTML (the form); dynamic interpolations are escaped by the
// callers.
func mimoSubmitPage(title, body string) []byte {
	return mimoSubmitPageHead(title, "", body)
}

// mimoSubmitPageHead additionally injects raw head HTML (headExtra is a
// trusted constant, e.g. the idle page's auto-refresh meta tag).
func mimoSubmitPageHead(title, headExtra, body string) []byte {
	return []byte(fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8">%s<title>%s</title></head><body style="font-family:system-ui,sans-serif;max-width:640px;margin:48px auto;padding:0 16px;line-height:1.7"><h2>%s</h2>%s<p style="color:#888;font-size:13px;margin-top:32px">cpa-multi-plugins · mimo</p></body></html>`,
		headExtra, html.EscapeString(title), html.EscapeString(title), body))
}
