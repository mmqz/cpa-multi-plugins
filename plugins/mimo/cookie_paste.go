// cookie_paste.go — v0.2.14 manual bootstrap-row paste lane for the cookie
// credential. Container/remote deployments cannot read the desktop app's
// Chromium partition jar (different machine — and a Windows DPAPI jar is
// undecryptable from a Linux container), yet the mint itself never needed
// the jar: serviceLogin→STS consumes only the account-domain bootstrap rows
// (passToken/userId/cUserId/uLocale). This page takes those rows pasted from
// ANY browser logged into the same Xiaomi account on account.xiaomi.com and
// runs the exact exchange the desktop-adoption flow uses — so remote users
// can mint a [COOKIE] credential that rides the desktop membership ledger
// without co-locating CPA with the desktop app (user request 2026-09-27).
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

// handleMimoCookieSubmit serves GET/POST /v0/resource/plugins/mimo/cookie_submit.
// POST (urlencoded form: cookies + region) parses the rows, mints the service
// ticket and persists the credential. GET since v0.2.15 renders the SAME
// combined page as the menu entry (/oauth_submit) — one "Mimo" surface, both
// paste lanes; the OAuth section's state-awareness comes along for free.
// v0.2.17: GET with a non-empty `cookies` query processes the paste exactly
// like POST — the panel page's fetch-based submitter fires this shape so a
// paste never navigates the browser (user report 2026-09-30: submitting the
// form navigated to a host-relative URL that could not load, leaving no
// credential behind; the fetch path keeps the page and renders the result
// inline, immune to mount-prefix and trailing-slash resolution).
func handleMimoCookieSubmit(req pluginapi.ManagementRequest) []byte {
	if strings.EqualFold(req.Method, http.MethodPost) {
		vals, err := url.ParseQuery(string(req.Body))
		if err != nil {
			return mimoSubmitPage("提交无法解析", "表单数据不是合法的 urlencoded 载荷："+html.EscapeString(err.Error()))
		}
		return handleMimoCookieSubmitValues(vals)
	}
	if q := strings.TrimSpace(req.Query.Get("cookies")); q != "" {
		return handleMimoCookieSubmitValues(req.Query)
	}
	return handleMimoOAuthSubmit(req)
}

// handleMimoCookieSubmitValues runs the paste lane for one submitted form
// (POST body or GET query — same fields: cookies, region).
func handleMimoCookieSubmitValues(vals url.Values) []byte {
	blob := vals.Get("cookies")
	pinned := normalizePasteRegion(vals.Get("region"))

	jar := parseBootstrapCookieBlob(blob)
	if msg := pasteJarMissing(jar); msg != "" {
		return mimoSubmitPage("提交不完整", msg+"<br>"+mimoCookiePasteFormatHint())
	}

	uid := userIdFromCookies(jar)
	if uid == "" {
		uid = "pasted-" + sha256Sum(cookieFingerprint(jar))
	}
	sa := &storedAuth{
		Auth: mimoTokens{
			Lane:      laneCookie,
			UID:       uid,
			Cookies:   jar,
			Region:    pinned,
			Source:    "manual-paste",
			AdoptedAt: time.Now().Unix(),
		},
		Account: mimoAccount{UID: uid},
	}
	minted := exchangeForCredentialWithPreference(sa, pinned)

	// Keep the canonical name when the same uid already has a cookie
	// credential (same policy as the adoption flow's re-adoption).
	name, _ := resolveAuthFileTarget(sa, nil)
	if files, err := hostAuthList(); err == nil {
		for _, f := range files {
			prev, err := hostAuthGet(f.AuthIndex)
			if err != nil || prev == nil || authLaneFor(prev) != laneCookie {
				continue
			}
			if sanitizeUIDForFileName(prev.Account.UID) == sanitizeUIDForFileName(uid) {
				name, _ = resolveAuthFileTarget(prev, nil)
				break
			}
		}
	}
	raw, err := buildAuthFileJSON(sa, false, "手动粘贴引导行 · "+time.Now().Format("2006-01-02 15:04"), nil)
	if err != nil {
		return mimoSubmitPage("保存失败", "凭证序列化失败："+html.EscapeString(err.Error()))
	}
	if err := hostAuthPersistFn(name, raw); err != nil {
		return mimoSubmitPage("保存失败", "写入宿主凭据库失败："+html.EscapeString(err.Error()))
	}

	summary := fmt.Sprintf("引导行已接收（uid %s，区域偏好 %s）—— 凭证已保存为 <code>%s</code>。",
		html.EscapeString(uid), html.EscapeString(regionDisplay(pinned)), html.EscapeString(name))
	if minted {
		summary += "现场换票成功：serviceToken 已铸入，回到 CPA 刷新凭据列表即可使用；之后运行时会自动续票。"
	} else {
		summary += "现场换票未成功：凭证先以 bootstrap-only 形态保存，首次调用会自动重试换票。若之后提示 passToken 被拒，说明该登录态已失效——请在浏览器重新登录 account.xiaomi.com 后重新复制提交。"
	}
	return mimoSubmitPage("粘贴登录完成", summary)
}

// normalizePasteRegion whitelists the region pin: cn/sgp pin themselves,
// everything else (incl. empty/auto) defers to the config region order.
func normalizePasteRegion(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "cn":
		return "cn"
	case "sgp":
		return "sgp"
	default:
		return ""
	}
}

func regionDisplay(r string) string {
	if r == "" {
		return "auto"
	}
	return r
}

// parseBootstrapCookieBlob accepts the shapes a human actually copies:
//   - a raw `Cookie:` request header line (prefix stripped case-insensitively)
//   - `k=v` pairs separated by `;` and/or newlines (DevTools copy style)
//   - a JSON object {"passToken": "...", "userId": "..."}
//
// Only the exchange's bootstrap names survive; every row is scoped to
// .account.xiaomi.com (pickBootstrap's preferred host rank) with "/" path.
// Values keep their literal form — passToken's own `V1:...` colon is part of
// the value, so splitting happens ONLY on the first `=` of each pair.
func parseBootstrapCookieBlob(raw string) []mimoCookie {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "Cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	s = strings.Trim(s, "\"'`“”‘’")
	if strings.HasPrefix(s, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil {
			jar := make([]mimoCookie, 0, len(m))
			for _, name := range []string{"passToken", "userId", "cUserId", "uLocale"} {
				v, ok := m[name]
				if !ok {
					continue
				}
				if vs := strings.TrimSpace(fmt.Sprintf("%v", v)); vs != "" && vs != "<nil>" {
					jar = append(jar, newBootstrapRow(name, vs))
				}
			}
			return jar
		}
	}
	jar := make([]mimoCookie, 0, 4)
	for _, pair := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), "\"'")
		if !bootstrapNames[k] || v == "" {
			continue
		}
		jar = append(jar, newBootstrapRow(k, v))
	}
	return jar
}

func newBootstrapRow(name, value string) mimoCookie {
	return mimoCookie{Name: name, Value: value, Domain: ".account.xiaomi.com", Path: "/", Secure: true, HTTPOnly: true}
}

// pasteJarMissing names the required rows a paste must carry: the exchange's
// P1 consumes passToken+userId at minimum; cUserId/uLocale stay optional
// (the measured working request had all four, but passport accepts fewer).
func pasteJarMissing(jar []mimoCookie) string {
	has := func(name string) bool {
		for _, c := range jar {
			if c.Name == name {
				return true
			}
		}
		return false
	}
	var missing []string
	for _, name := range []string{"passToken", "userId"} {
		if !has(name) {
			missing = append(missing, "<code>"+name+"</code>")
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "缺少必需行：" + strings.Join(missing, "、") + "（cUserId / uLocale 可选）。"
}

const mimoCookiePasteFormHTML = `<p><b>适用场景</b>：CPA 部署在 Docker/容器或与 MiMo 桌面端不同机，插件自动采纳读不到桌面端的会话库；换票本身不需要桌面端——只需要账号域的引导行。从<b>任意已登录同一小米账号</b>的浏览器获取即可。</p>
<ol>
<li>电脑浏览器打开 <code>https://account.xiaomi.com</code> 并登录（与 MiMo 桌面端同一账号）；</li>
<li>按 F12 打开开发者工具 → <b>Application（应用）→ Cookies → account.xiaomi.com</b>；</li>
<li>找到并复制 <code>passToken</code> 与 <code>userId</code> 两行的值（<code>cUserId</code>/<code>uLocale</code> 可选）；</li>
<li>按格式粘贴到下面提交：</li>
</ol>
<p style="background:#f6f8fa;padding:8px;border-radius:6px;font-family:monospace">passToken=粘贴的值; userId=粘贴的值; cUserId=粘贴的值</p>
<form method="POST" action="cookie_submit">
<textarea name="cookies" rows="4" style="width:96%" placeholder="passToken=…; userId=…; cUserId=…" required></textarea><br><br>
区域偏好：
<select name="region">
<option value="cn">cn（国内桌面会员账本，默认）</option>
<option value="sgp">sgp（国际区）</option>
<option value="auto">auto（按插件 region 配置的候选序）</option>
</select>
<button>提交并现场换票</button>
</form>
<p style="color:#8a6d3b;background:#fcf8e3;padding:8px;border-radius:6px">⚠ passToken 等同账号登录态：只提交到你自己的 CPA，提交后不要转发明文。</p>`

func mimoCookiePasteFormatHint() string {
	return `格式：<code>passToken=值; userId=值</code>（分号或换行分隔均可，也可粘贴 <code>{"passToken":"…","userId":"…"}</code> JSON）。`
}
