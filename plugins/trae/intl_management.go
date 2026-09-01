// management.go implements the Trae Intl management API and web panel.
//
// Trae Intl uses the Web SOLO remote protocol (chat_sessions + events SSE on
// core-normal.trae.ai); it has NO daily check-in and NO multi-account pool /
// scheduler (each request goes to whichever account the host routed it to).
// The management API therefore exposes only the account listing + a simple
// status check + the embedded panel.html. This is enough to:
//   - see which Trae Intl accounts are configured in the CPA UI,
//   - inspect their on-disk metadata (uid, nickname, expiry),
//   - test the upstream reachability with a /status ping.
//
// Routes (intlproviderName = "trae-intl"):
//   - GET  /v0/management/plugins/trae-intl/accounts — list accounts from host.auth.list
//   - GET  /v0/management/plugins/trae-intl/status   — pool/health summary
//   - GET  /v0/resource/plugins/trae-intl/panel      — embedded panel.html (browser UI)
//
// Envelope contract (same as workbuddy / trae-solo-cn / trae-cn):
// intlhandleManagement returns an envelope-wrapped pluginapi.ManagementResponse;
// CPA's rpcPluginAdapter.callPlugin[pluginapi.ManagementResponse] unwraps the
// envelope, and CPA's ServeManagementHTTP / ServeResourceHTTP write only
// resp.Body to the browser.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/intlupstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// Route + response types
// -----------------------------------------------------------------------------

type intlmanagementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type intlresourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type intlmanagementRegistrationResponse struct {
	Routes    []intlmanagementRoute `json:"routes,omitempty"`
	Resources []intlresourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so intlhandleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
// managementBasePathCache / Mu are shared with the CN-side management code.

func intlloadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func intlsetManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

// intlmanagementRegistration lists the plugin-owned management routes + browser
// resources. Trae Intl has no check-in (the upstream Web SOLO remote protocol
// has no equivalent endpoint), so we only expose accounts + status + the panel.
func intlmanagementRegistration() intlmanagementRegistrationResponse {
	base := "/plugins/" + intlproviderName
	return intlmanagementRegistrationResponse{
		Routes: []intlmanagementRoute{
			{Method: http.MethodGet, Path: base + "/intl/accounts", Description: "List Trae Intl accounts with uid, nickname, and token expiry."},
			{Method: http.MethodGet, Path: base + "/intl/status", Description: "Trae Intl plugin status (no pool — reflects host auth store count + upstream reachability)."},
			{Method: http.MethodPost, Path: base + "/intl/import", Description: "Import Trae Intl credential JSON into host auth store."},
		},
		// v0.12.2: no separate Intl menu — /panel covers all variants.
		Resources: []intlresourceRoute{},
	}
}

// -----------------------------------------------------------------------------
// intlhandleManagement
// -----------------------------------------------------------------------------

func intlhandleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := "/v0/resource/plugins/" + intlproviderName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(intlmgmtHTMLResponse(intlservePanel(sub)))
	}

	base := intlloadedManagementBasePath() + "/plugins/" + intlproviderName
	switch {
	case req.Method == http.MethodGet && path == base+"/intl/accounts":
		return okEnvelope(intlmgmtJSONResponse(http.StatusOK, intlbuildDashboard()))
	case req.Method == http.MethodGet && path == base+"/intl/status":
		return okEnvelope(intlmgmtJSONResponse(http.StatusOK, buildStatus()))
	case req.Method == http.MethodPost && path == base+"/intl/import":
		return okEnvelope(intlmgmtJSONResponse(http.StatusOK, intlhandleImportAuth(req)))
	}
	return okEnvelope(intlmgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------------

func intlmgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func intlmgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// -----------------------------------------------------------------------------
// Panel HTML embed
// -----------------------------------------------------------------------------

//go:embed intl_panel.html
var intlpanelHTML []byte

// intlservePanel returns the embedded panel.html for valid sub-paths, or a 404
// stub for unknown resources.
func intlservePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/intl_panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	return intlpanelHTML
}

// -----------------------------------------------------------------------------
// Dashboard / account listing
// -----------------------------------------------------------------------------

// intltraeAccount is one row of the dashboard. Trae Intl has no credits / check-in
// concept — only identity + token expiry (refresh handled by host.auth.refresh).
type intltraeAccount struct {
	AuthIndex    string `json:"auth_index"`
	AuthID       string `json:"auth_id,omitempty"`
	Name         string `json:"name"`
	Label        string `json:"label"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Region       string `json:"region,omitempty"`
	Status       string `json:"status"`
	Disabled     bool   `json:"disabled"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
	Error        string `json:"error,omitempty"`
}

// intlbuildDashboard lists every Trae Intl credential known to the host. We do
// not call upstream for each account (that would be slow + cost 1 HTTP req
// per account on every page load); the panel can lazily trigger /status if
// it wants live reachability.
func intlbuildDashboard() map[string]any {
	files, err := intlhostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := make([]intltraeAccount, 0, len(files))
	for _, f := range files {
		acct := intltraeAccount{
			AuthIndex: f.AuthIndex,
			AuthID:    f.ID,
			Name:      f.Name,
			Label:     f.Label,
			Status:    f.Status,
			Disabled:  f.Disabled,
		}
		sa, err := intlhostAuthGet(f.AuthIndex)
		if err != nil {
			acct.Error = "load auth: " + err.Error()
			out = append(out, acct)
			continue
		}
		// Sa is the parsed nested {auth, account} shape (see intlhostAuthGet).
		// Trae Intl stores extended identity fields directly under auth:
		// region, scope, tenant, etc.
		acct.UID = sa.Account.UID
		acct.Nickname = sa.Account.Nickname
		acct.EnterpriseID = sa.Account.EnterpriseID
		if sa.Auth.ExpiresAt > 0 {
			acct.TokenExpiry = time.Unix(sa.Auth.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, acct)
	}
	return map[string]any{
		"accounts":    out,
		"provider":    intlproviderName,
		"server_time": time.Now().Format("2006-01-02 15:04:05"),
	}
}

// buildStatus returns a simple status summary. Trae Intl has no in-process pool
// (each request goes to whichever account the host's auth router picked), so
// we can only report the host auth store count + a per-account token-expiry
// summary.
func buildStatus() map[string]any {
	files, err := intlhostAuthList()
	now := time.Now()
	resp := map[string]any{
		"provider":    intlproviderName,
		"server_time": now.Format("2006-01-02 15:04:05"),
	}
	if err != nil {
		resp["error"] = err.Error()
		resp["account_count"] = 0
		return resp
	}
	expired := 0
	expiringSoon := 0 // within 24h
	for _, f := range files {
		sa, err := intlhostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if sa.Auth.ExpiresAt <= 0 {
			continue
		}
		exp := time.Unix(sa.Auth.ExpiresAt, 0)
		if exp.Before(now) {
			expired++
		} else if exp.Before(now.Add(24 * time.Hour)) {
			expiringSoon++
		}
	}
	resp["account_count"] = len(files)
	resp["expired_count"] = expired
	resp["expiring_soon_count"] = expiringSoon
	resp["pool_enabled"] = false // trae-intl has no account pool
	return resp
}

// -----------------------------------------------------------------------------
// host auth bridge (host.auth.list / host.auth.get)
// -----------------------------------------------------------------------------

// intlrpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type intlrpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type intlrpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// intlhostAuthList returns all Trae Intl credentials known to the host. We filter
// by filename prefix because some legacy auth files don't carry a "type"/
// "provider" field.
func intlhostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp intlrpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	prefix := intlproviderName + "-"
	for _, f := range resp.Files {
		if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			out = append(out, f)
		}
	}
	return out, nil
}

// intlstoredAuth is the nested {auth, account} shape persisted by trae-intl's
// login flow. The Auth block carries Intl-specific identity fields (region,
// scope, tenant, appLanguage, appVersion) alongside the OAuth tokens.
type intlstoredAuth struct {
	Auth    intlstoredTokens  `json:"auth"`
	Account intlstoredAccount `json:"account"`
}

type intlstoredTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	APIHost      string `json:"apiHost"`
	Region       string `json:"region,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	AppLanguage  string `json:"appLanguage,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
}

type intlstoredAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// intlhostAuthGet fetches one credential (by auth_index) from the host and parses
// it into the nested {auth, account} shape.
func intlhostAuthGet(authIndex string) (*intlstoredAuth, error) {
	reqBody, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, reqBody)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp intlrpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	var sa intlstoredAuth
	if err := json.Unmarshal(resp.JSON, &sa); err != nil {
		return nil, fmt.Errorf("parse stored auth: %w", err)
	}
	return &sa, nil
}

// intlhostAuthSave persists credential JSON via host.auth.save RPC.
func intlhostAuthSave(name string, raw []byte) error {
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

// intlpersistRefreshedAuth writes updated token fields back to host auth store.
func intlpersistRefreshedAuth(req pluginapi.ExecutorRequest, a *upstream.Auth) {
	fileName := req.AuthID
	if fileName == "" {
		fileName = fmt.Sprintf("%s-%s.json", intlproviderName, a.UID)
	}
	storageJSON, _ := json.MarshalIndent(map[string]any{
		"type":     intlproviderName,
		"provider": intlproviderName,
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.APIHost,
			"region":       a.Region,
			"scope":        a.Scope,
			"tenant":       a.Tenant,
			"appLanguage":  a.AppLanguage,
			"appVersion":   a.AppVersion,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
		"disabled": false,
	}, "", "  ")
	if err := intlhostAuthSave(fileName, storageJSON); err != nil {
		log.Printf("persist refreshed auth %s: %v", a.UID, err)
	}
}

// intlhandleImportAuth imports a Trae Intl credential JSON into the host auth store.
func intlhandleImportAuth(req pluginapi.ManagementRequest) map[string]any {
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
	a, err := intlparseStoredAuth(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	storageJSON, _ := json.MarshalIndent(map[string]any{
		"type":     intlproviderName,
		"provider": intlproviderName,
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.APIHost,
			"region":       a.Region,
			"scope":        a.Scope,
			"tenant":       a.Tenant,
			"appLanguage":  a.AppLanguage,
			"appVersion":   a.AppVersion,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
		"disabled": false,
	}, "", "  ")
	fileName := fmt.Sprintf("%s-%s.json", intlproviderName, a.UID)
	if err := intlhostAuthSave(fileName, storageJSON); err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{
		"success":  true,
		"name":     fileName,
		"uid":      a.UID,
		"nickname": a.Nickname,
	}
}
