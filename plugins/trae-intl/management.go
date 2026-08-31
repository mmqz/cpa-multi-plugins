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
// Routes (providerName = "trae-intl"):
//   - GET  /v0/management/plugins/trae-intl/accounts — list accounts from host.auth.list
//   - GET  /v0/management/plugins/trae-intl/status   — pool/health summary
//   - GET  /v0/resource/plugins/trae-intl/panel      — embedded panel.html (browser UI)
//
// Envelope contract (same as workbuddy / trae-solo-cn / trae-cn):
// handleManagement returns an envelope-wrapped pluginapi.ManagementResponse;
// CPA's rpcPluginAdapter.callPlugin[pluginapi.ManagementResponse] unwraps the
// envelope, and CPA's ServeManagementHTTP / ServeResourceHTTP write only
// resp.Body to the browser.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

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

// managementRegistration lists the plugin-owned management routes + browser
// resources. Trae Intl has no check-in (the upstream Web SOLO remote protocol
// has no equivalent endpoint), so we only expose accounts + status + the panel.
func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List Trae Intl accounts with uid, nickname, and token expiry."},
			{Method: http.MethodGet, Path: base + "/status", Description: "Trae Intl plugin status (no pool — reflects host auth store count + upstream reachability)."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Trae Intl", Description: "Trae Intl dashboard: accounts."},
		},
	}
}

// -----------------------------------------------------------------------------
// handleManagement
// -----------------------------------------------------------------------------

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
	case req.Method == http.MethodGet && path == base+"/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildStatus()))
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

// traeAccount is one row of the dashboard. Trae Intl has no credits / check-in
// concept — only identity + token expiry (refresh handled by host.auth.refresh).
type traeAccount struct {
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id,omitempty"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Region      string `json:"region,omitempty"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	TokenExpiry string `json:"token_expiry,omitempty"`
	Error       string `json:"error,omitempty"`
}

// buildDashboard lists every Trae Intl credential known to the host. We do
// not call upstream for each account (that would be slow + cost 1 HTTP req
// per account on every page load); the panel can lazily trigger /status if
// it wants live reachability.
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
		// Sa is the parsed nested {auth, account} shape (see hostAuthGet).
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
		"provider":    providerName,
		"server_time": time.Now().Format("2006-01-02 15:04:05"),
	}
}

// buildStatus returns a simple status summary. Trae Intl has no in-process pool
// (each request goes to whichever account the host's auth router picked), so
// we can only report the host auth store count + a per-account token-expiry
// summary.
func buildStatus() map[string]any {
	files, err := hostAuthList()
	now := time.Now()
	resp := map[string]any{
		"provider":    providerName,
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
		sa, err := hostAuthGet(f.AuthIndex)
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

// hostAuthList returns all Trae Intl credentials known to the host. We filter
// by filename prefix because some legacy auth files don't carry a "type"/
// "provider" field.
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

// storedAuth is the nested {auth, account} shape persisted by trae-intl's
// login flow. The Auth block carries Intl-specific identity fields (region,
// scope, tenant, appLanguage, appVersion) alongside the OAuth tokens.
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
	Region       string `json:"region,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	AppLanguage  string `json:"appLanguage,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// hostAuthGet fetches one credential (by auth_index) from the host and parses
// it into the nested {auth, account} shape.
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
