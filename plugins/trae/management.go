// management.go implements the Trae SOLO CN management API and web panel:
// the account dashboard (uid, nickname, credits, plan, check-in status),
// manual check-in (single or all accounts), credit query, token refresh,
// and the account-pool status (cooling / disabled state).
//
// Routes are exposed under two prefixes:
//   - /v0/management/plugins/trae-solo-cn/*  — JSON endpoints (CPA-management-
//     authenticated by host middleware; plugin-layer auth only kicks in when
//     management_key is configured).
//   - /v0/resource/plugins/trae-solo-cn/*    — unauthenticated browser UI
//     (panel.html) and menu entries surfaced in the CPA management UI.
//
// The contract follows workbuddy/management.go: handleManagement returns an
// envelope-wrapped pluginapi.ManagementResponse (envelope is consumed by
// CPA's rpcPluginAdapter.callPlugin[pluginapi.ManagementResponse]; CPA's
// HTTP layer writes only resp.Body to the browser, so JSON.parse / HTML
// rendering work transparently).
package main

import (
        _ "embed"
        "encoding/json"
        "fmt"
        "log"
        "net/http"
        "strings"
        "sync"
        "time"

        "github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// Route + response types
// -----------------------------------------------------------------------------

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

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
        managementBasePathCache   = "/v0/management"
        managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
        managementBasePathCacheMu.RLock()
        defer managementBasePathCacheMu.RUnlock()
        return managementBasePathCache
}

func setManagementBasePath(p string) {
        p = strings.TrimRight(strings.TrimSpace(p), "/")
        if p == "" {
                return
        }
        managementBasePathCacheMu.Lock()
        managementBasePathCache = p
        managementBasePathCacheMu.Unlock()
}

// managementRegistration describes the routes + resources this plugin serves.
// Paths are registered relative to /plugins/trae-solo-cn — the host prepends
// either /v0/management or /v0/resource/plugins/<provider> based on which list
// they appear in (Routes vs Resources).
func managementRegistration() managementRegistrationResponse {
        base := "/plugins/" + providerName
        return managementRegistrationResponse{
                Routes: []managementRoute{
                        {Method: http.MethodGet, Path: base + "/accounts", Description: "List Trae SOLO CN accounts with credits, plan, and check-in status."},
                        {Method: http.MethodPost, Path: base + "/checkin", Description: "Manually check in one account (auth_index) or all accounts."},
                        {Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
                        {Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh access tokens + credits for all accounts."},
                        {Method: http.MethodGet, Path: base + "/status", Description: "Account-pool state: cooling / disabled reasons per account."},
                        {Method: http.MethodPost, Path: base + "/import", Description: "Import Trae credential JSON (nested or flat) into host auth store."},
                        {Method: http.MethodGet, Path: base + "/intl/accounts", Description: "Trae Intl: list accounts with uid, nickname, and token expiry."},
                        {Method: http.MethodGet, Path: base + "/intl/status", Description: "Trae Intl: plugin status."},
                        {Method: http.MethodPost, Path: base + "/intl/import", Description: "Trae Intl: import credential JSON into host auth store."},
                },
                // Single menu entry (v0.12.2): /panel covers CN + SOLO + Intl
                // accounts. Legacy /intl_panel path serves the same panel.
                Resources: []resourceRoute{
                        {Path: "/panel", Menu: "Trae", Description: "Trae dashboard: CN/SOLO credits, check-in, accounts + Intl accounts."},
                        // Menu-less: routable browser resources without UI entries.
                        {Path: "/intl_panel", Description: "Trae Intl dashboard (linked from the main panel)."},
                        {Path: "/oauth_callback", Description: "OAuth login callback redirect target."},
                        {Path: "/oauth_submit", Description: "Paste-to-complete fallback: GET with cb_url=<url-encoded failed redirect URL> finishes the login remotely (host resource routes are GET-only)."},
                        {Path: "/login_status", Description: "Live login state for the panel status line: pending variant, callback endpoint, TTL remaining, last outcome (data only, no secrets)."},
                },
        }
}

// -----------------------------------------------------------------------------
// handleManagement
// -----------------------------------------------------------------------------

// handleManagement dispatches one ManagementRequest to the right handler and
// returns an envelope-wrapped pluginapi.ManagementResponse.
//
// Envelope shape contract: CPA's rpcPluginAdapter.callPlugin decodes the
// envelope, extracts Result, and unmarshals it into pluginapi.ManagementResponse.
// CPA's HTTP layer (ServeManagementHTTP / ServeResourceHTTP) writes only
// resp.Body to the browser (with resp.Headers / resp.StatusCode) — the envelope
// never reaches the browser, so JSON.parse works on the raw JSON body.
func handleManagement(raw []byte) ([]byte, error) {
        var req pluginapi.ManagementRequest
        if err := json.Unmarshal(raw, &req); err != nil {
                return nil, err
        }
        path := strings.TrimRight(req.Path, "/")

        // Browser UI resource routes (unauthenticated).
        resPrefix := "/v0/resource/plugins/" + providerName
        if req.Method == http.MethodGet && path == resPrefix+"/oauth_callback" {
                return okEnvelope(mgmtHTMLResponse(handleOAuthCallbackResource(req)))
        }
        if (req.Method == http.MethodGet || req.Method == http.MethodPost) && path == resPrefix+"/oauth_submit" {
                return okEnvelope(mgmtHTMLResponse(handleOAuthSubmitResource(req)))
        }
        if req.Method == http.MethodGet && path == resPrefix+"/login_status" {
                return okEnvelope(mgmtJSONResourceResponse(handleLoginStatusResource()))
        }
        if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
                sub := strings.TrimPrefix(path, resPrefix)
                return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
        }

        base := loadedManagementBasePath() + "/plugins/" + providerName
        switch {
        case req.Method == http.MethodGet && path == base+"/accounts":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboard()))
        case req.Method == http.MethodPost && path == base+"/checkin":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, handleManualCheckin(req)))
        case req.Method == http.MethodGet && path == base+"/credits":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
        case req.Method == http.MethodPost && path == base+"/refresh":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, handleRefresh()))
        case req.Method == http.MethodGet && path == base+"/status":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, buildPoolStatus()))
        case req.Method == http.MethodPost && path == base+"/import":
                return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportAuth(req)))
        }
        return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// Panel HTML embed
// -----------------------------------------------------------------------------

//go:embed panel.html
var panelHTML []byte

// servePanel returns the embedded panel.html for valid sub-paths, or a 404
// stub for unknown resources.
func servePanel(sub string) []byte {
        // /intl_panel stays as a hidden alias — it serves the unified panel
        // (v0.12.2 removed the separate Intl menu entry).
        if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" && sub != "/intl_panel" {
                return []byte("<h1>404</h1>")
        }
        return panelHTML
}

// -----------------------------------------------------------------------------
// Dashboard / account listing
// -----------------------------------------------------------------------------

// traeAccount is one row of the dashboard. It is built from the host auth
// store (host.auth.list) so it reflects the CPA UI's on-disk truth — the
// accountPool may lag behind by one config-reload cycle.
type traeAccount struct {
        AuthIndex string       `json:"auth_index"`
        AuthID    string       `json:"auth_id,omitempty"`
        Name      string       `json:"name"`
        Label     string       `json:"label"`
        UID       string       `json:"uid"`
        Nickname  string       `json:"nickname"`
        Status    string       `json:"status"`
        Disabled  bool         `json:"disabled"`
        Credits   *traeCredits `json:"credits,omitempty"`
        Checkin   *traeCheckin `json:"checkin,omitempty"`
        Error     string       `json:"error,omitempty"`
        Variant   string       `json:"variant,omitempty"`
}

type traeCredits struct {
        // TotalRemain is the SUBSCRIPTION pack quota. nil = never fetched — NOT
        // the same as 0 (a checkin-only free account has a 0 pack but a non-zero
        // wallet). v0.12.25: the check-in WALLET lives in checkin.credits
        // (traeCheckin).
        // v0.12.28: TotalRemain 改为上游用量模型值（fast 可用次数 / basic 剩余），
        // 且 RemainKnown=false 时为 nil —— 未知剩余不再渲染成 0（对齐 cockpit-tools
        // "无可靠剩余时不猜测"：Free 显示 "免费剩余：--"）。
        TotalRemain *int64 `json:"total_remain"`
        Plan        string `json:"plan"`
        FetchedAt   string `json:"fetched_at,omitempty"`
        // v0.12.28 用量模型扩展（对齐 trae.ts TraeUsage）。
        UsageModel  string `json:"usage_model,omitempty"` // fast|basic|unknown
        RemainKnown bool   `json:"remain_known"`
        Used        *int64 `json:"used,omitempty"`  // basic: 已用
        Total       *int64 `json:"total,omitempty"` // basic: 额度池
        FastLimit   *int64 `json:"fast_limit,omitempty"`
        FastUsed    *int64 `json:"fast_used,omitempty"`
        // v0.12.29 ide_user_pay_status 补充维度（Free/SOLO 账户的真实数值）。
        FastRequestPer *int64  `json:"fast_request_per,omitempty"` // 快请求/月
        SoloParallel   *int64  `json:"solo_parallel,omitempty"`    // SOLO 并发数
        SoloPackage    bool    `json:"solo_package,omitempty"`     // enable_solo_* 任一
        PlanType       string  `json:"plan_type,omitempty"`        // user_pay_identity_str
}

type traeCheckin struct {
        CheckedIn bool  `json:"checked_in"`
        Credits   int64 `json:"credits"`
        Enable    bool  `json:"enable"`
}

// buildDashboard aggregates every Trae SOLO CN credential from the host auth
// store, plus a snapshot of credits / checkin status from the account cache
// (or live upstream if the cache is stale).
func buildDashboard() map[string]any {
        // v0.12.27: migrate shared-namespace solo credentials FIRST (rename +
        // re-register) so the inventory below and the heal scan both see the
        // final names. Idempotent; a no-op once every solo file is namespaced.
        if n := migrateSoloFileNames(); n > 0 {
                log.Printf("trae dashboard: migrated %d solo credential(s) into the solo namespace", n)
        }
        // v0.12.26: heal next — if the host's manager dropped any on-disk
        // credential (torn-read reconciliation, see heal.go), re-register it so
        // the panel reflects the REAL credential inventory, not the manager's.
        if n := healAuthRegistration(providerName+"-", hostAuthList); n > 0 {
                log.Printf("trae dashboard: healed %d credential(s) missing from host manager", n)
        }
        files, err := hostAuthList()
        if err != nil {
                return map[string]any{"error": err.Error()}
        }
        out := make([]traeAccount, 0, len(files))
        for _, f := range files {
                acct := traeAccount{
                        AuthIndex: f.AuthIndex,
                        AuthID:    f.ID,
                        Name:      f.Name,
                        Label:     f.Label,
                        Status:    f.Status,
                        Disabled:  f.Disabled,
                }
                sa, err := hostAuthGet(f.AuthIndex)
                if err != nil {
                        acct.Error = "load auth: " + err.Error()
                        out = append(out, acct)
                        continue
                }
                acct.UID = sa.Account.UID
                acct.Nickname = sa.Account.Nickname
                acct.Variant = sa.Variant

                // Cached credits / checkin (filled by scheduler + manual endpoints).
                // v0.12.25: credits < 0 = pack quota never fetched — leave the
                // object off so the panel lazy-loads it via /credits.
                if v, ok := accountCache.Load(f.AuthIndex); ok {
                        if e, ok2 := v.(*accountCacheEntry); ok2 && (e.credits >= 0 || e.usageFilled) {
                                acct.Credits = creditsFromCache(e)
                        }
                        if e, ok2 := v.(*accountCacheEntry); ok2 && e.checkin != nil {
                                acct.Checkin = &traeCheckin{
                                        CheckedIn: e.checkin.CheckedIn,
                                        Credits:   e.checkin.Credits,
                                        Enable:    e.checkin.Enable,
                                }
                        }
                }
                out = append(out, acct)
        }
        return map[string]any{
                "accounts":    out,
                "provider":    providerName,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
}

// buildPoolStatus surfaces the accountPool's cooling / disabled state — useful
// for debugging why traffic is being routed away from an account.
func buildPoolStatus() map[string]any {
        if accountPool == nil {
                return map[string]any{"provider": providerName, "accounts": []any{}}
        }
        statuses := accountPool.List()
        return map[string]any{
                "provider":    providerName,
                "accounts":    statuses,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
}

// -----------------------------------------------------------------------------
// host auth bridge (host.auth.list / host.auth.get)
// -----------------------------------------------------------------------------

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
        Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
        AuthIndex string          `json:"auth_index"`
        Name      string          `json:"name"`
        Path      string          `json:"path"`
        JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all Trae SOLO CN credentials known to the host. We
// filter by filename prefix because some legacy auth files don't carry a
// "type"/"provider" field (pre-config-convention files).
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
        raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
        if err != nil {
                return nil, err
        }
        var env envelope
        if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
                return nil, fmt.Errorf("host.auth.list: bad envelope")
        }
        var resp rpcHostAuthListResponse
        if err := json.Unmarshal(env.Result, &resp); err != nil {
                return nil, err
        }
        out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
        prefix := providerName + "-"
        for _, f := range resp.Files {
                if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
                        out = append(out, f)
                }
        }
        return out, nil
}

// hostAuthGet fetches one credential (by auth_index) from the host and parses
// it into the nested {auth, account} shape used by upstreamClient.
type storedAuth struct {
        Auth    storedTokens  `json:"auth"`
        Account storedAccount `json:"account"`
        // Variant is not persisted separately (it lives in Auth in the JSON);
        // it is resolved at load time (hostAuthGet) so management operations
        // hit the right upstream endpoints for cn/solo/intl accounts (v0.12.2).
        Variant string `json:"-"`
}

type storedTokens struct {
        AccessToken  string `json:"accessToken"`
        RefreshToken string `json:"refreshToken"`
        ExpiresAt    int64  `json:"expiresAt"`
        Domain       string `json:"domain"`
        APIHost      string `json:"apiHost"`
        MachineID    string `json:"machineId"`
        DeviceID     string `json:"deviceId"`
        Variant      string `json:"variant"`
}

type storedAccount struct {
        UID          string `json:"uid"`
        EnterpriseID string `json:"enterpriseId"`
        Nickname     string `json:"nickname"`
}

func hostAuthGet(authIndex string) (*storedAuth, error) {
        reqBody, _ := json.Marshal(map[string]string{"auth_index": authIndex})
        raw, err := hostCall(pluginabi.MethodHostAuthGet, reqBody)
        if err != nil {
                return nil, err
        }
        var env envelope
        if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
                return nil, fmt.Errorf("host.auth.get: bad envelope")
        }
        var resp rpcHostAuthGetResponse
        if err := json.Unmarshal(env.Result, &resp); err != nil {
                return nil, err
        }
        var sa storedAuth
        if err := json.Unmarshal(resp.JSON, &sa); err != nil {
                return nil, fmt.Errorf("parse stored auth: %w", err)
        }
        // Explicit auth.variant wins; sniff covers legacy files (v0.12.2).
        sa.Variant = sa.Auth.Variant
        if sa.Variant == "" {
                sa.Variant = sniffVariantFromJSON(resp.JSON)
        }
        return &sa, nil
}

// hostAuthAsUpstream converts the host-stored nested shape into the upstream
// *auth.Auth the trae-solo-cn upstream client expects.
func hostAuthAsUpstream(sa *storedAuth) *auth.Auth {
        return &auth.Auth{
                AccessToken:  sa.Auth.AccessToken,
                RefreshToken: sa.Auth.RefreshToken,
                ExpiresAt:    sa.Auth.ExpiresAt,
                Domain:       sa.Auth.Domain,
                APIHost:      sa.Auth.APIHost,
                MachineID:    sa.Auth.MachineID,
                DeviceID:     sa.Auth.DeviceID,
                UID:          sa.Account.UID,
                EnterpriseID: sa.Account.EnterpriseID,
                Nickname:     sa.Account.Nickname,
                Variant:      sa.Variant,
        }
}

// -----------------------------------------------------------------------------
// Manual management endpoints
// -----------------------------------------------------------------------------

// creditsFromCache projects the cache entry onto the dashboard's traeCredits.
// v0.12.28: usageFilled entries carry the upstream usage model; legacy
// cache entries (pre-upgrade) keep the old pack-only number.
func creditsFromCache(e *accountCacheEntry) *traeCredits {
        if e.usageFilled {
                c := &traeCredits{
                        Plan:        e.usagePlan(),
                        FetchedAt:   e.fetched.Format(time.RFC3339),
                        UsageModel:  e.usage.UsageModel,
                        RemainKnown: e.usage.RemainKnown,
                }
                if e.usage.RemainKnown {
                        r := e.usage.Remain
                        c.TotalRemain = &r
                }
                if e.usage.UsageModel == "basic" {
                        u, t := e.usage.Used, e.usage.Total
                        c.Used, c.Total = &u, &t
                }
                if e.usage.UsageModel == "fast" {
                        fl, fu := e.usage.FastLimit, e.usage.FastUsed
                        c.FastLimit, c.FastUsed = &fl, &fu
                }
                // v0.12.29: pay_status 补充维度随缓存透出（/accounts 免刷新可见）。
                c.FastRequestPer = e.usage.FastRequestPer
                c.SoloParallel = e.usage.SoloParallel
                c.SoloPackage = e.usage.SoloPackage
                c.PlanType = e.usage.PlanType
                return c
        }
        if e.credits < 0 {
                return nil
        }
        remain := e.credits
        return &traeCredits{TotalRemain: &remain, FetchedAt: e.fetched.Format(time.RFC3339), RemainKnown: true, UsageModel: "legacy"}
}

// usagePlan returns the plan label stored alongside the usage snapshot
// (empty until handleCreditsQuery fills it).
func (e *accountCacheEntry) usagePlan() string { return e.plan }

// handleManualCheckin triggers checkin for one (auth_index) or all accounts.
// Body: {"auth_index":"<idx>","uid":"<uid>"} — empty / omitted triggers all.
// v0.12.28: uid 兼底匹配。凭证文件被 migrate/heal/adopt 改名或宿主管理器
// 重建后 auth_index 会变化，面板仍持旧 index 点击签到会匹配 0 个文件；
// 此时改按 uid 匹配（uid 稳定），并把实际使用的 auth_index 回传给面板。
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
        var body struct {
                AuthIndex string `json:"auth_index"`
                UID       string `json:"uid"`
        }
        if len(req.Body) > 0 {
                _ = json.Unmarshal(req.Body, &body)
        }
        results := []map[string]any{}

        files, err := hostAuthList()
        if err != nil {
                return map[string]any{"error": err.Error()}
        }
        // v0.12.28: auth_index 失效检测。面板传了 index 却匹配不到任何文件
        // （凭证被 migrate/heal/adopt 改名或宿主重建过），改按 uid 匹配；
        // 两者都匹配不到时返回明确的 stale_index 错误而不是空 results
        // （空 results 曾被面板渲染成"签到完成"假成功）。
        targets := make([]pluginapi.HostAuthFileEntry, 0, len(files))
        indexMatched := false
        for _, f := range files {
                if body.AuthIndex != "" && f.AuthIndex == body.AuthIndex {
                        indexMatched = true
                }
        }
        fallbackByUID := !indexMatched && body.AuthIndex != "" && body.UID != ""
        for _, f := range files {
                if body.AuthIndex == "" {
                        // all accounts
                } else if indexMatched {
                        if f.AuthIndex != body.AuthIndex {
                                continue
                        }
                } else if fallbackByUID {
                        // stale auth_index → uid 兜底
                } else {
                        continue
                }
                targets = append(targets, f)
        }
        if body.AuthIndex != "" && len(targets) == 0 {
                return map[string]any{
                        "provider":    providerName,
                        "results":     results,
                        "error":       "stale_index: auth_index 不存在（凭证可能已被重命名/重注册），请刷新列表后重试",
                        "stale_index": true,
                }
        }
        for _, f := range targets {
                entry := map[string]any{"auth_index": f.AuthIndex, "uid": "", "nickname": ""}
                sa, err := hostAuthGet(f.AuthIndex)
                if err != nil {
                        entry["error"] = "load auth: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                if fallbackByUID && strings.TrimSpace(sa.Account.UID) != strings.TrimSpace(body.UID) {
                        continue // uid 兜底时只处理同 uid 账号
                }
                entry["uid"] = sa.Account.UID
                entry["nickname"] = sa.Account.Nickname
                a := hostAuthAsUpstream(sa)
                status, err := upstreamClient.CheckinStatus(a)
                if err != nil {
                        entry["error"] = "checkin_status: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                entry["already_checked_in"] = status.CheckedIn
                entry["credits"] = status.Credits
                if !status.CheckedIn && status.Enable {
                        claim, err := upstreamClient.CheckinClaim(a)
                        if err != nil {
                                entry["error"] = "checkin_claim: " + err.Error()
                        } else {
                                entry["claim_code"] = claim.Code
                                entry["claim_message"] = claim.Message
                        }
                }
                // Refresh cached checkin / credits.
                // v0.12.25 semantics: e.credits = SUBSCRIPTION pack quota;
                // e.checkin.Credits = the CHECK-IN WALLET — two different wallets.
                // Storing the wallet into e.credits here made the card read 150 right
                // after check-in, then drop to the pack's 0 on the next /credits
                // refresh ("签到 150 积分一刷新就归零").
                prevCredits := int64(-1) // -1 = unknown, keep previous
                prevUsageFilled := false
                if v, ok := accountCache.Load(f.AuthIndex); ok {
                        if e, ok2 := v.(*accountCacheEntry); ok2 {
                                prevCredits = e.credits
                                prevUsageFilled = e.usageFilled
                        }
                }
                newEntry := &accountCacheEntry{
                        credits: prevCredits,
                        checkin: &checkinStatus{
                                CheckedIn: status.CheckedIn,
                                Credits:   status.Credits,
                                Enable:    status.Enable,
                        },
                        fetched:     time.Now(),
                        usageFilled: prevUsageFilled,
                }
                if prevUsageFilled {
                        newEntry.usage = cacheUsage(f.AuthIndex)
                        newEntry.plan = cachePlan(f.AuthIndex)
                }
                accountCache.Store(f.AuthIndex, newEntry)
                // Re-enable account in pool if checkin restored credits.
                // v0.12.28: pool score prefers the usage-model remain when known
                // (fast/basic); falls back to pack + wallet so a checkin-only
                // account (pack unknown, wallet 150) is neither starved nor shown
                // as exhausted.
                if accountPool != nil {
                        score := status.Credits
                        if prevUsageFilled && newEntry.usage.RemainKnown && newEntry.usage.Remain > 0 {
                                r := newEntry.usage.Remain
                                if r < 0 { // unlimited fast requests
                                        r = 1 << 30
                                }
                                score = r + status.Credits
                        } else if prevCredits > 0 {
                                score = prevCredits + status.Credits
                        }
                        accountPool.ReenableIfCredits(sa.Account.UID, score)
                }
                results = append(results, entry)
        }
        return map[string]any{
                "provider":    providerName,
                "results":     results,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
}

// cacheUsage/cachePlan read the current usage snapshot without mutating cache.
func cacheUsage(authIndex string) (u upstream.UsageSummary) {
        if v, ok := accountCache.Load(authIndex); ok {
                if e, ok2 := v.(*accountCacheEntry); ok2 {
                        return e.usage
                }
        }
        return
}
func cachePlan(authIndex string) (p string) {
        if v, ok := accountCache.Load(authIndex); ok {
                if e, ok2 := v.(*accountCacheEntry); ok2 {
                        return e.plan
                }
        }
        return
}

// handleCreditsQuery fetches live credits from upstream for one (auth_index)
// or all accounts. Updates the cache so the next /accounts reflects the new
// numbers.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
        // Read auth_index from query string (?auth_index=xxx) first, then body JSON.
        // panel.html uses GET /credits?auth_index=xxx, so req.Query is the primary source.
        authIndex := ""
        if req.Query != nil {
                authIndex = strings.TrimSpace(req.Query.Get("auth_index"))
        }
        if authIndex == "" && len(req.Body) > 0 {
                var body struct {
                        AuthIndex string `json:"auth_index"`
                }
                _ = json.Unmarshal(req.Body, &body)
                authIndex = body.AuthIndex
        }
        results := []map[string]any{}

        files, err := hostAuthList()
        if err != nil {
                return map[string]any{"error": err.Error()}
        }
        for _, f := range files {
                if authIndex != "" && f.AuthIndex != authIndex {
                        continue
                }
                entry := map[string]any{"auth_index": f.AuthIndex, "uid": "", "nickname": ""}
                sa, err := hostAuthGet(f.AuthIndex)
                if err != nil {
                        entry["error"] = "load auth: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                entry["uid"] = sa.Account.UID
                entry["nickname"] = sa.Account.Nickname
                a := hostAuthAsUpstream(sa)
                usage, err := upstreamClient.UserEntUsage(a)
                if err != nil {
                        entry["error"] = "ent_usage: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                // v0.12.28: 套餐剩余对齐 cockpit-tools 的用量模型（trae.ts）：
                //   fast  → 速通可用次数（-1 无限）
                //   basic → 选中包 basic_usage_limit - basic_usage_amount（含 bonus）
                //   unknown → 剩余不可知（面板显示 "--"；旧代码读不存在的
                //             credits_limit 字段把这里渲染成"剩余 0 积分 · 00%"）。
                sum := upstream.SummarizeUsage(usage.UserEntitlementPackList, true)
                selected := upstream.SelectActivePack(usage.UserEntitlementPackList, true)
                plan := "Unknown"
                if selected != nil {
                        // 上游 identityStr 优先取选中包 display_desc，回退 product_type 映射。
                        if d := strings.TrimSpace(selected.DisplayDesc); d != "" {
                                plan = d
                        } else {
                                plan = upstream.ProductTypeIdentity(selected.EntitlementBaseInfo.ProductType, true)
                        }
                }
                // v0.12.29: ide_user_pay_status —— 上游刷新链路的第二个数据源
                // （trae_account_core_refresh.rs 先 pay_status 再 ent_usage）。Free CN/SOLO
                // 的 ent_usage pack 里没有可解析 quota，剩余维度（快请求/月、SOLO 并发）
                // 只在 pay_status 的 detail/quota 里。best-effort：失败不阻塞 credits。
                if ps, psErr := upstreamClient.PayStatus(a); psErr == nil && ps.Code == 0 {
                        sum.FastRequestPer = ps.FastRequestPer()
                        sum.SoloParallel = ps.SoloParallelLimit()
                        sum.SoloPackage = ps.HasSoloPackage()
                        sum.PlanType = ps.PlanIdentity()
                        // 选中包缺失时回退 user_pay_identity_str 作为计划显示
                        // （上游 account.plan_type 即来自这里）。
                        if selected == nil {
                                if id := ps.PlanIdentity(); id != "" {
                                        plan = id
                                }
                        }
                }
                entry["usage_model"] = sum.UsageModel
                entry["remain_known"] = sum.RemainKnown
                if sum.RemainKnown {
                        entry["total_remain"] = sum.Remain
                } else {
                        entry["total_remain"] = 0 // 向后兼容；remain_known=false 时面板显示 "--"
                }
                entry["plan"] = plan
                if sum.UsageModel == "basic" {
                        entry["used"] = sum.Used
                        entry["total"] = sum.Total
                }
                if sum.UsageModel == "fast" {
                        entry["fast_limit"] = sum.FastLimit
                        entry["fast_used"] = sum.FastUsed
                }
                // v0.12.29: pay_status 补充维度透传（面板"快请求/月 / Solo 并发"）。
                if sum.FastRequestPer != nil {
                        entry["fast_request_per"] = *sum.FastRequestPer
                }
                if sum.SoloParallel != nil {
                        entry["solo_parallel"] = *sum.SoloParallel
                }
                if sum.SoloPackage {
                        entry["solo_package"] = true
                }
                if sum.PlanType != "" {
                        entry["plan_type"] = sum.PlanType
                }
                // v0.12.25: also fetch the CHECK-IN WALLET (a separate upstream
                // wallet from the subscription pack). The old code stored the
                // pack limit into BOTH slots, so a checkin-only account's card
                // flipped 150 → 0 on every refresh ("一刷新就归零").
                wallet := int64(-1)
                checkedIn, enable := false, false
                if st, stErr := upstreamClient.CheckinStatus(a); stErr == nil {
                        wallet = st.Credits
                        checkedIn, enable = st.CheckedIn, st.Enable
                } else {
                        entry["checkin_status_error"] = stErr.Error()
                }
                if wallet >= 0 {
                        entry["checkin_credits"] = wallet
                        entry["checked_in"] = checkedIn
                }
                // Update cache + pool. e.credits stays pack-only (legacy field); the
                // wallet lives in e.checkin.Credits. Pool score = usage remain (when
                // known) + wallet so a checkin-only account is not treated as exhausted.
                scoreRemain := int64(0)
                if sum.RemainKnown {
                        scoreRemain = sum.Remain
                        if scoreRemain < 0 { // unlimited
                                scoreRemain = 1 << 30
                        }
                }
                accountCache.Store(f.AuthIndex, &accountCacheEntry{
                        credits:     scoreRemain,
                        checkin:     &checkinStatus{CheckedIn: checkedIn, Credits: wallet, Enable: enable},
                        fetched:     time.Now(),
                        usage:       sum,
                        usageFilled: true,
                        plan:        plan,
                })
                if accountPool != nil {
                        accountPool.SetCredits(sa.Account.UID, scoreRemain+max64(wallet, 0))
                }
                results = append(results, entry)
        }
        return map[string]any{
                "provider":    providerName,
                "results":     results,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
}

func max64(a, b int64) int64 {
        if a > b {
                return a
        }
        return b
}

// handleRefresh forces an ExchangeToken refresh on every account whose token
// is within the refresh skew of expiry. Updates host-stored auth via host.auth.save
// is the host's responsibility (we only mutate the in-memory Auth and trigger
// upstream refresh); the next host.auth.refresh RPC will persist the new tokens.
func handleRefresh() map[string]any {
        files, err := hostAuthList()
        if err != nil {
                return map[string]any{"error": err.Error()}
        }
        results := []map[string]any{}
        for _, f := range files {
                entry := map[string]any{"auth_index": f.AuthIndex, "uid": "", "nickname": ""}
                sa, err := hostAuthGet(f.AuthIndex)
                if err != nil {
                        entry["error"] = "load auth: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                entry["uid"] = sa.Account.UID
                entry["nickname"] = sa.Account.Nickname
                a := hostAuthAsUpstream(sa)
                refreshed, err := upstreamClient.RefreshTokenIfNeeded(a, defaultRefreshSkew)
                if err != nil {
                        entry["error"] = "refresh: " + err.Error()
                        results = append(results, entry)
                        continue
                }
                entry["refreshed"] = refreshed
                results = append(results, entry)
        }
        return map[string]any{
                "provider":    providerName,
                "results":     results,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
}

// hostAuthSave persists credential JSON via host.auth.save RPC.
// Used by executor to write back refreshed tokens so they survive CPA restart.
func hostAuthSave(name string, raw []byte) error {
        // Use pluginapi.HostAuthSaveRequest so JSON field (json.RawMessage) is
        // embedded raw, NOT base64-encoded (which map[string]any{"json": raw} would do).
        saveReq := pluginapi.HostAuthSaveRequest{
                Name: name,
                JSON: raw,
        }
        saveBody, _ := json.Marshal(saveReq)
        rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
        if err != nil {
                return fmt.Errorf("host.auth.save RPC: %w", err)
        }
        var env envelope
        if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
                return fmt.Errorf("host.auth.save: bad envelope")
        }
        return nil
}

// persistRefreshedAuth writes updated token fields back to host auth store
// after a successful RefreshTokenIfNeeded in the executor path.
func persistRefreshedAuth(req pluginapi.ExecutorRequest, a *auth.Auth) {
        // Derive file name from auth ID or StorageJSON.
        fileName := req.AuthID
        if fileName == "" {
                // v0.12.27: per-variant fallback naming (solo → its own namespace).
                fileName = credentialFileName(a.Variant, a.UID)
        } else if !strings.HasSuffix(strings.ToLower(fileName), ".json") {
                // v0.12.8: a uid-shaped AuthID would land as an extension-less
                // file the watcher ignores, losing the refreshed token on restart.
                fileName += ".json"
        }
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                        // v0.12.6: keep the parsed variant — dropping it made a solo
                        // account degrade to cn on refresh/import.
                        "variant": a.Variant,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
                "disabled": false,
        }, "", "  ")
        if err := hostAuthSave(fileName, storageJSON); err != nil {
                log.Printf("persist refreshed auth %s: %v", a.UID, err)
        }
}

// handleImportAuth imports a Trae credential JSON (nested or flat) into the
// host auth store. Body: {"json": <raw json>} or {"raw": "<json string>"}.
// This lets users paste a token from another tool (e.g. traework2api login.sh)
// without going through the browser OAuth flow.
func handleImportAuth(req pluginapi.ManagementRequest) map[string]any {
        var body struct {
                JSON json.RawMessage `json:"json"`
                Raw  string          `json:"raw"`
        }
        _ = json.Unmarshal(req.Body, &body)
        raw := []byte(strings.TrimSpace(body.Raw))
        if len(body.JSON) > 0 {
                raw = body.JSON
        }
        if len(raw) == 0 {
                return map[string]any{"success": false, "error": "missing json/raw credential payload"}
        }
        a, err := auth.Parse(raw)
        if err != nil {
                return map[string]any{"success": false, "error": err.Error()}
        }
        // Build nested storage JSON for host.auth.save.
        // CRITICAL: include "type" and "provider" fields so CPA can route this
        // auth file to the correct plugin provider (matches workbuddy pattern).
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "type":     providerName,
                "provider": providerName,
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                        // v0.12.6: keep the parsed variant — dropping it made a solo
                        // account degrade to cn on refresh/import.
                        "variant": a.Variant,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
                "disabled": false,
        }, "", "  ")
        // v0.12.27: per-variant namespace — an imported solo credential must
        // not overwrite the cn file of the same Trae account.
        fileName := credentialFileName(a.Variant, a.UID)
        if err := hostAuthSave(fileName, storageJSON); err != nil {
                return map[string]any{"success": false, "error": err.Error()}
        }
        // Register in pool.
        accountPool.Add(a)
        return map[string]any{
                "success":  true,
                "name":     fileName,
                "uid":      a.UID,
                "nickname": a.Nickname,
        }
}
