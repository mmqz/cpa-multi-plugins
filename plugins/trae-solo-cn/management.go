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

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/auth"
        "github.com/mmqz/cpa-multi-plugins/plugins/trae-solo-cn/upstream"
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
                },
                Resources: []resourceRoute{
                        {Path: "/panel", Menu: "Trae SOLO CN", Description: "Trae Work CN dashboard: credits, check-in, accounts."},
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
        if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
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
        AuthIndex string          `json:"auth_index"`
        AuthID    string          `json:"auth_id,omitempty"`
        Name      string          `json:"name"`
        Label     string          `json:"label"`
        UID       string          `json:"uid"`
        Nickname  string          `json:"nickname"`
        Status    string          `json:"status"`
        Disabled  bool            `json:"disabled"`
        Credits   *traeCredits    `json:"credits,omitempty"`
        Checkin   *traeCheckin     `json:"checkin,omitempty"`
        Error     string          `json:"error,omitempty"`
}

type traeCredits struct {
        TotalRemain int64  `json:"total_remain"`
        Plan        string `json:"plan"`
        FetchedAt   string `json:"fetched_at,omitempty"`
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

                // Cached credits / checkin (filled by scheduler + manual endpoints).
                if v, ok := accountCache.Load(f.AuthIndex); ok {
                        if e, ok2 := v.(*accountCacheEntry); ok2 {
                                acct.Credits = &traeCredits{
                                        TotalRemain: e.credits,
                                        FetchedAt:   e.fetched.Format(time.RFC3339),
                                }
                                if e.checkin != nil {
                                        acct.Checkin = &traeCheckin{
                                                CheckedIn: e.checkin.CheckedIn,
                                                Credits:   e.checkin.Credits,
                                                Enable:    e.checkin.Enable,
                                        }
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
}

type storedTokens struct {
        AccessToken  string `json:"accessToken"`
        RefreshToken string `json:"refreshToken"`
        ExpiresAt    int64  `json:"expiresAt"`
        Domain       string `json:"domain"`
        APIHost string `json:"apiHost"`
        MachineID    string `json:"machineId"`
        DeviceID     string `json:"deviceId"`
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
        }
}

// -----------------------------------------------------------------------------
// Manual management endpoints
// -----------------------------------------------------------------------------

// handleManualCheckin triggers checkin for one (auth_index) or all accounts.
// Body: {"auth_index":"<idx>"} — empty / omitted triggers all.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
        var body struct {
                AuthIndex string `json:"auth_index"`
        }
        if len(req.Body) > 0 {
                _ = json.Unmarshal(req.Body, &body)
        }
        results := []map[string]any{}

        files, err := hostAuthList()
        if err != nil {
                return map[string]any{"error": err.Error()}
        }
        for _, f := range files {
                if body.AuthIndex != "" && f.AuthIndex != body.AuthIndex {
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
                accountCache.Store(f.AuthIndex, &accountCacheEntry{
                        credits: status.Credits,
                        checkin: &checkinStatus{
                                CheckedIn: status.CheckedIn,
                                Credits:   status.Credits,
                                Enable:    status.Enable,
                        },
                        fetched: time.Now(),
                })
                // Re-enable account in pool if checkin restored credits.
                if accountPool != nil {
                        accountPool.ReenableIfCredits(sa.Account.UID, status.Credits)
                }
                results = append(results, entry)
        }
        return map[string]any{
                "provider":    providerName,
                "results":     results,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
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
                selected := upstream.SelectActivePack(usage.UserEntitlementPackList, true)
                var remain int64
                plan := "Unknown"
                if selected != nil {
                        remain = selected.EntitlementBaseInfo.Quota.CreditsLimit
                        plan = upstream.ProductTypeIdentity(selected.EntitlementBaseInfo.ProductType, true)
                }
                entry["total_remain"] = remain
                entry["plan"] = plan
                // Update cache + pool.
                accountCache.Store(f.AuthIndex, &accountCacheEntry{
                        credits: remain,
                        checkin: &checkinStatus{Credits: remain},
                        fetched: time.Now(),
                })
                if accountPool != nil {
                        accountPool.SetCredits(sa.Account.UID, remain)
                }
                results = append(results, entry)
        }
        return map[string]any{
                "provider":    providerName,
                "results":     results,
                "server_time": time.Now().Format("2006-01-02 15:04:05"),
        }
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
                fileName = fmt.Sprintf("%s-%s.json", providerName, a.UID)
        }
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
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
        storageJSON, _ := json.MarshalIndent(map[string]any{
                "auth": map[string]any{
                        "accessToken":  a.AccessToken,
                        "refreshToken": a.RefreshToken,
                        "expiresAt":    a.ExpiresAt,
                        "domain":       a.Domain,
                        "apiHost":      a.APIHost,
                        "machineId":    a.MachineID,
                        "deviceId":     a.DeviceID,
                },
                "account": map[string]any{
                        "uid":          a.UID,
                        "enterpriseId": a.EnterpriseID,
                        "nickname":     a.Nickname,
                },
        }, "", "  ")
        fileName := fmt.Sprintf("%s-%s.json", providerName, a.UID)
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
