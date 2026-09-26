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

// mimoManagementRegistration advertises the login surfaces. v0.2.7: the
// oauth_submit resource carries a Menu label — the management panel renders
// labeled plugin resources as sidebar navigation entries (menu →
// /plugin-pages/<id>/<idx> iframe), giving remote/Docker users a DISCOVERABLE
// registration fallback instead of a URL they must know.
func mimoManagementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Resources: []resourceRoute{
			{Path: "/oauth_submit", Menu: "登录兜底粘贴", Description: "MiMo paste-to-complete fallback: open this page (or GET ?cb_url=<url-encoded failed redirect URL>) to finish a login whose localhost redirect failed."},
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

const mimoSubmitFormHTML = `<p>远程部署时浏览器无法跳回本机完成 MiMo 登录。请：</p>
<ol>
<li>回到 CPA 重新点「登录」，在打开的页面中完成小米账号授权；</li>
<li>浏览器最后会跳转 <code>http://localhost:…/auth?u=…</code> 并打开失败——复制地址栏<b>完整链接</b>（登录 6 分钟内有效）；</li>
<li>粘贴到下面并提交。</li>
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
				body := fmt.Sprintf(`<ol>
<li>点击下方按钮，在<strong>新标签页</strong>打开小米授权页并完成登录授权；本页请保持打开。</li>
<li>授权后浏览器会跳转 <code>http://localhost:…/auth?u=…</code>：本机部署会自动完成；远程 / Docker 部署该页打不开——复制地址栏<b>完整链接</b>（本次登录 6 分钟内有效）。</li>
<li>把完整链接粘贴到下面并点「完成登录」，然后回到 CPA 登录窗口等待自动完成。</li>
</ol>
<p><a href="%s" target="_blank" rel="noopener noreferrer" style="display:inline-block;background:#ff6900;color:#fff;padding:10px 22px;border-radius:8px;text-decoration:none;font-weight:600">前往小米登录</a></p>
<p>授权记录名 <code>%s</code></p>`,
					html.EscapeString(lc.authorizeURL), html.EscapeString(lc.keyName)) + mimoGateFormHTML
				return mimoSubmitPage("MiMo 登录引导", body)
			}
		}
	}
	return mimoSubmitPage("MiMo 登录引导", mimoGateStaleHTML)
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
		return mimoSubmitPage("MiMo 登录兜底", mimoSubmitFormHTML)
	}
	res, ok := mimoCompletePendingLogin(blob)
	if !ok {
		return mimoSubmitPage("登录未完成", "粘贴的链接无法匹配任何进行中的登录。请确认：① 登录是在 6 分钟内从 CPA 发起的；② 粘贴的是<b>本次</b>登录失败页地址栏的完整链接（含 u= 参数）。回到 CPA 重新点「登录」后再试一次。")
	}
	summary := "密钥已接收"
	if uid := strings.TrimSpace(res.UID); uid != "" {
		summary += "（账号 uid " + html.EscapeString(uid) + "）"
	}
	return mimoSubmitPage("登录完成", summary+" —— 回到 CPA 的登录窗口，它会在下一次轮询时自动完成；若该窗口已关闭，直接在 CPA 里重新登录一次即可（密钥已生效）。")
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

// mimoSubmitPage renders the minimal browser-facing fallback page. body may
// carry trusted HTML (the form); dynamic interpolations are escaped by the
// callers.
func mimoSubmitPage(title, body string) []byte {
	return []byte(fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head><body style="font-family:system-ui,sans-serif;max-width:640px;margin:48px auto;padding:0 16px;line-height:1.7"><h2>%s</h2>%s<p style="color:#888;font-size:13px;margin-top:32px">cpa-multi-plugins · mimo</p></body></html>`,
		html.EscapeString(title), html.EscapeString(title), body))
}
