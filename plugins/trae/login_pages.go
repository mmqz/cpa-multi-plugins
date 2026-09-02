// login_pages.go implements the plugin-served per-region OAuth login pages
// (v0.12.9). The CPA management UI renders plugin-declared menus as sidebar
// entries and opens them in an iframe; each login page starts a variant-
// PINNED OAuth flow (login_start), opens the upstream authorization URL in a
// new browser tab and polls login_wait until the flow completes. login_wait
// drives the SAME poll handlers the host uses, then persists the credential
// via host.auth.save — so a self-serve login reuses the existing OAuth
// machinery end to end, and CN/SOLO/Intl logins can run concurrently without
// touching the global login_variant (which previously was the only way to
// pick a region, forcing one OAuth entry per plugin).
//
// Routes (declared in managementRegistration, dispatched by handleManagement):
//
//	GET /v0/resource/plugins/trae/login_cn    — login page (menu: CN 登录)
//	GET /v0/resource/plugins/trae/login_solo  — login page (menu: SOLO 登录)
//	GET /v0/resource/plugins/trae/login_intl  — login page (menu: Intl 登录)
//	GET /v0/resource/plugins/trae/login_start — starts a pinned flow (JSON)
//	GET /v0/resource/plugins/trae/login_wait  — polls/completes a flow (JSON)
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginRegionMeta describes one self-serve login page.
type loginRegionMeta struct {
	path    string // resource sub-path, e.g. "/login_cn"
	variant string // pinned variant: cn | solo | intl
	title   string // page <h2> title
	hint    string // instruction shown under the spinner
}

var loginRegions = []loginRegionMeta{
	{path: "/login_cn", variant: variantCN, title: "Trae CN 登录", hint: "在打开的新标签页中完成 Trae CN 授权；完成后本页自动更新。"},
	{path: "/login_solo", variant: variantSolo, title: "Trae SOLO 登录", hint: "在打开的新标签页中完成 Trae SOLO 授权；完成后本页自动更新。"},
	{path: "/login_intl", variant: variantIntl, title: "Trae Intl 登录", hint: "在打开的新标签页中完成 Trae Intl 授权；完成后本页自动更新。"},
}

// loginPageTemplate is the single-page login flow: start → open upstream →
// poll → result. The upstream authorization URL always opens in a new tab so
// the page (inside the CPA management iframe) survives to show the result.
//
// fmt verbs are explicit (%[1]s title / %[2]s variant / %[3]s hint). The JS
// poll budget (460 polls × 2s ≈ 15 min) mirrors loginTTL — the poll handlers
// expire the flow server-side anyway.
const loginPageTemplate = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s</title>
<style>
 body{font-family:system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;text-align:center;padding-top:10vh;color:#1f2329;background:#fff}
 h2{font-weight:600;margin-bottom:18px}
 .spin{display:inline-block;width:20px;height:20px;border:3px solid #d0d5dd;border-top-color:#4e5ba6;border-radius:50%%;animation:s .8s linear infinite;vertical-align:-6px;margin-right:8px}
 @keyframes s{to{transform:rotate(360deg)}}
 .ok{color:#067647}.bad{color:#b42318}
 #link{display:none;margin-top:14px}
 a{color:#4e5ba6;word-break:break-all}
 #hint{color:#667085;font-size:13px}
 #res{font-weight:600;margin-top:18px}
</style></head><body>
<h2>%[1]s</h2>
<p id="st"><span class="spin"></span>正在发起登录…</p>
<p id="link"><a id="linka" href="#" target="_blank" rel="noopener">若授权页未自动打开，点此打开授权页</a></p>
<p id="hint">%[3]s</p>
<p id="res"></p>
<script>
(function(){
 var st=document.getElementById('st'),res=document.getElementById('res'),link=document.getElementById('link'),linka=document.getElementById('linka');
 function done(cls,txt){st.style.display='none';res.className=cls;res.textContent=txt;}
 function fail(txt){st.style.display='none';link.style.display='block';res.className='bad';res.textContent=txt;}
 fetch('login_start?variant=%[2]s&o='+encodeURIComponent(location.origin),{cache:'no-store'})
  .then(function(r){return r.json().then(function(j){return{ok:r.ok,body:j};});})
  .then(function(p){
    if(!p.ok||!p.body.url||!p.body.state){fail('发起登录失败：'+(p.body.error||('HTTP '+p.status)));return;}
    linka.href=p.body.url;link.style.display='block';
    window.open(p.body.url,'_blank','noopener');
    st.innerHTML='<span class="spin"></span>等待授权完成…';
    var n=0;
    (function poll(){
      n++;
      fetch('login_wait?state='+encodeURIComponent(p.body.state),{cache:'no-store'})
       .then(function(r){return r.json();})
       .then(function(j){
         if(j.status==='success'){done('ok','登录成功'+(j.name?('：'+j.name):'')+'，账号已入库，可关闭此页。');return;}
         if(j.status==='error'){fail('登录失败：'+(j.error||'未知错误'));return;}
         if(n>460){fail('等待超时，请重新发起登录。');return;}
         setTimeout(poll,2000);
       })
       .catch(function(){if(n>460){fail('等待超时，请重新发起登录。');return;}setTimeout(poll,3000);});
    })();
  })
  .catch(function(e){fail('网络错误：'+e);});
})();
</script>
</body></html>
`

// handleLoginPage serves the embedded page for /login_cn, /login_solo and
// /login_intl (title/variant substituted).
func handleLoginPage(sub string) []byte {
	for _, r := range loginRegions {
		if r.path == sub {
			return []byte(fmt.Sprintf(loginPageTemplate, r.title, r.variant, r.hint))
		}
	}
	return []byte("<html><body><h2>404</h2><p>unknown login page</p></body></html>")
}

// selfServeStates records flows started via login_start; login_wait refuses
// every other state so it can never race the host-driven poller.
var selfServeStates sync.Map

// waitInflight ensures one terminal handling per state: concurrent page
// retries get "pending" instead of racing two token exchanges.
var waitInflight sync.Map

// waitResults caches terminal results so repeated polls (page retries, tab
// refreshes) see a stable answer after the underlying state was consumed.
var waitResults sync.Map

// loginRegionByVariant returns the page metadata for a pinned variant.
func loginRegionByVariant(v string) (loginRegionMeta, bool) {
	for _, r := range loginRegions {
		if r.variant == v {
			return r, true
		}
	}
	return loginRegionMeta{}, false
}

// sanitizeCallbackOrigin validates the page-supplied CPA origin and returns
// scheme://host[:port] (or ""). A forged origin cannot steal tokens: the
// PKCE verifier never leaves the plugin, so a hijacked callback URL simply
// fails to complete its own flow.
func sanitizeCallbackOrigin(o string) string {
	o = strings.TrimSpace(o)
	if o == "" {
		return ""
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// handleLoginStart serves GET /v0/resource/plugins/trae/login_start?variant=
// cn|solo|intl&o=<origin>. It synthesizes the host login context the poll
// machinery expects (BaseURL = the CPA origin the page runs on; AuthDir empty
// — self-serve flows complete via the resource callback, not the .oauth
// file), starts the variant-pinned flow and marks the state self-serve.
func handleLoginStart(req pluginapi.ManagementRequest) any {
	q := req.Query
	variant, origin := "", ""
	if q != nil {
		variant = strings.TrimSpace(q.Get("variant"))
		origin = sanitizeCallbackOrigin(q.Get("o"))
	}
	meta, okMeta := loginRegionByVariant(variant)
	if !okMeta {
		return map[string]any{"error": "missing or invalid variant (cn|solo|intl)"}
	}
	if origin == "" {
		return map[string]any{"error": "missing origin (o) — the login page must be served by CPA"}
	}
	fakeStart, _ := json.Marshal(map[string]any{
		"BaseURL": origin,
		"Host":    map[string]any{"AuthDir": ""},
	})
	var (
		raw []byte
		err error
	)
	if meta.variant == variantIntl {
		raw, err = intlhandleStartLogin(fakeStart)
	} else {
		raw, err = startLoginWithVariant(fakeStart, meta.variant)
	}
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || len(env.Result) == 0 {
		return map[string]any{"error": "login start: bad response envelope"}
	}
	var resp pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return map[string]any{"error": "login start: bad response payload"}
	}
	state := strings.TrimSpace(resp.State)
	if state == "" {
		return map[string]any{"error": "login start: empty state"}
	}
	selfServeStates.Store(state, struct{}{})
	return map[string]any{"url": resp.URL, "state": state}
}

// handleLoginWait serves GET /v0/resource/plugins/trae/login_wait?state=….
// It reuses the host poll handlers verbatim (routing intl states to the intl
// poller) and persists the returned credential via host.auth.save — the same
// path handleImportAuth uses, so the account lands in the watcher/parse
// pipeline exactly like an imported one.
func handleLoginWait(req pluginapi.ManagementRequest) any {
	state := ""
	if req.Query != nil {
		state = strings.TrimSpace(req.Query.Get("state"))
	}
	if state == "" {
		return map[string]any{"status": "error", "error": "missing state"}
	}
	if v, ok := waitResults.Load(state); ok {
		return v.(map[string]any)
	}
	if _, ok := selfServeStates.Load(state); !ok {
		return map[string]any{"status": "error", "error": "unknown or non-self-serve login state"}
	}
	if _, loaded := waitInflight.LoadOrStore(state, true); loaded {
		return map[string]any{"status": "pending"}
	}
	defer waitInflight.Delete(state)

	fakeReq, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: providerName, State: state})
	var (
		raw  []byte
		perr error
	)
	if pollStateIsIntl(fakeReq) {
		raw, perr = intlhandlePollLogin(fakeReq)
	} else {
		raw, perr = handlePollLogin(fakeReq)
	}
	if perr != nil {
		res := map[string]any{"status": "error", "error": perr.Error()}
		waitResults.Store(state, res)
		selfServeStates.Delete(state)
		return res
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || len(env.Result) == 0 {
		// Transient envelope problem — let the page retry.
		return map[string]any{"status": "pending"}
	}
	var resp pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return map[string]any{"status": "pending"}
	}
	switch resp.Status {
	case pluginapi.AuthLoginStatusError:
		res := map[string]any{"status": "error", "error": nonEmpty(resp.Message, "login failed")}
		waitResults.Store(state, res)
		selfServeStates.Delete(state)
		return res
	case pluginapi.AuthLoginStatusSuccess:
		res := finishSelfServeLogin(state, resp)
		waitResults.Store(state, res)
		selfServeStates.Delete(state)
		return res
	default:
		return map[string]any{"status": "pending", "message": resp.Message}
	}
}

// finishSelfServeLogin persists the credential returned by the poll handler.
func finishSelfServeLogin(state string, resp pluginapi.AuthLoginPollResponse) map[string]any {
	name := strings.TrimSpace(resp.Auth.FileName)
	if name == "" {
		name = strings.TrimSpace(resp.Auth.ID)
	}
	if name == "" || len(resp.Auth.StorageJSON) == 0 {
		return map[string]any{"status": "error", "error": "login success but plugin returned no credential payload"}
	}
	if err := hostAuthSave(name, resp.Auth.StorageJSON); err != nil {
		return map[string]any{"status": "error", "error": "save credential failed: " + err.Error()}
	}
	uid := ""
	if resp.Auth.Metadata != nil {
		if v, ok := resp.Auth.Metadata["uid"].(string); ok {
			uid = v
		}
	}
	return map[string]any{"status": "success", "name": name, "label": resp.Auth.Label, "uid": uid}
}
