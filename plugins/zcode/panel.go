// panel.go serves the management dashboard: the aggregated account list the
// web UI consumes (buildDashboardEx) and the embedded HTML page itself.
package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// zcAccount is one row of the dashboard.
type zcAccount struct {
	AuthIndex string           `json:"auth_index"`
	AuthID    string           `json:"auth_id,omitempty"`
	Name      string           `json:"name"`
	Label     string           `json:"label"`
	Nickname  string           `json:"nickname"`
	UID       string           `json:"uid"`
	Provider  string           `json:"provider"` // zai | bigmodel
	Plan      string           `json:"plan"`     // coding-plan | start-plan
	Status    string           `json:"status"`
	Disabled  bool             `json:"disabled"`
	Exhausted bool             `json:"exhausted"`
	Selected  bool             `json:"selected"`
	Credits   *creditsSummary  `json:"credits,omitempty"`
	Cooling   []map[string]any `json:"cooling,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// creditsSummary is the shared quota snapshot shape (panel + notes).
type creditsSummary struct {
	TotalRemain int64            `json:"total_remain"`
	TotalUsed   int64            `json:"total_used"`
	TotalSize   int64            `json:"total_size"`
	PackCount   int              `json:"pack_count"`
	FetchedAt   string           `json:"fetched_at,omitempty"`
	Packages    []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
}

// fetchPlanName resolves the plan tier display name (best-effort; empty on
// any failure). zcode.z.ai has no dedicated plan endpoint beyond the billing
// snapshot, so the plan comes from the account's own plan field plus the
// balance row count.
func fetchPlanName(sa *storedAuth) string {
	plan := authPlanFor(sa)
	if sa.Auth.JWT == "" {
		return plan + " (no JWT)"
	}
	return plan
}

// buildDashboardEx aggregates the account list. Credits fields are left
// empty on light loads — the panel renders skeletons and fetches them lazily
// via /credits?auth_index=<idx> (avoids stampeding billing on page load).
func buildDashboardEx(force, fetchQuota bool) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Prune cache entries for accounts that no longer exist or expired.
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	accountCache.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			accountCache.Delete(key)
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*accountCacheEntry); ok && time.Since(e.fetched) > 4*accountCacheTTL {
			accountCache.Delete(key)
		}
		return true
	})
	pruneLifecycleState(live)
	out := make([]zcAccount, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			acct := zcAccount{
				AuthIndex: f.AuthIndex,
				AuthID:    f.ID,
				Name:      f.Name,
				Label:     f.Label,
				Status:    f.Status,
				Disabled:  f.Disabled,
			}
			sa, phys, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				acct.Error = "load auth: " + err.Error()
				out[i] = acct
				return
			}
			if phys != nil {
				acct.Disabled = phys.Disabled
				if phys.Name != "" {
					acct.Name = phys.Name
				}
			}
			acct.Nickname = sa.Account.Nickname
			acct.UID = sa.Account.UID
			acct.Provider = authProviderFor(sa)
			acct.Plan = authPlanFor(sa)
			acct.Cooling = cooldownSnapshotFor(f.ID)
			if fetchQuota {
				_, cr, errs := cachedAccountDetails(f.ID, sa, force)
				acct.Credits = cr
				acct.Exhausted = isCreditsExhausted(cr)
				_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, acct.Disabled)
				acct.Error = strings.Join(errs, "; ")
			} else {
				if v, ok := accountCache.Load(f.ID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						acct.Credits = e.credits
						acct.Exhausted = isCreditsExhausted(e.credits)
						acct.Plan = firstNonEmpty(e.plan, acct.Plan)
					}
				}
			}
			out[i] = acct
		}(i, f)
	}
	wg.Wait()
	var life []map[string]any
	if force && lifecycleEnabled() {
		life = reconcileAllAccounts(true)
		if files2, err2 := hostAuthList(); err2 == nil {
			live2 := make(map[string]struct{}, len(files2))
			disabledBy := make(map[string]bool, len(files2))
			for _, f := range files2 {
				live2[f.AuthIndex] = struct{}{}
				disabledBy[f.AuthIndex] = f.Disabled
			}
			filtered := out[:0]
			for _, a := range out {
				if _, ok := live2[a.AuthIndex]; !ok {
					continue
				}
				if d, ok := disabledBy[a.AuthIndex]; ok {
					a.Disabled = d
				}
				if v, ok := accountCache.Load(a.AuthID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						if e.credits != nil {
							a.Credits = e.credits
							a.Exhausted = isCreditsExhausted(e.credits)
						}
						if e.plan != "" {
							a.Plan = e.plan
						}
					}
				}
				filtered = append(filtered, a)
			}
			out = filtered
		}
	}
	activeID := ensureDefaultActiveAuth(out)
	sum := summarizeQuotas(out)
	for i := range out {
		out[i].Selected = out[i].AuthID == activeID
	}
	resp := map[string]any{
		"accounts":       out,
		"active_auth":    activeID,
		"lifecycle_auto": lifecycleEnabled(),
		"login_provider": loadedLoginProvider(),
		"server_time":    time.Now().Format("2006-01-02 15:04:05"),
		"summary":        sum,
	}
	if len(life) > 0 {
		resp["lifecycle"] = life
	}
	return resp
}

// summarizeQuotas aggregates remain/used across dashboard accounts.
func summarizeQuotas(accounts []zcAccount) map[string]any {
	var remain, used, size, zaiRemain, zaiUsed, zaiSize, bmRemain, bmUsed, bmSize int64
	var known, disabledN, exhaustedN, packs int
	for _, a := range accounts {
		if a.Disabled {
			disabledN++
		}
		if a.Exhausted {
			exhaustedN++
		}
		if a.Credits == nil {
			continue
		}
		cr := a.Credits
		if cr.TotalRemain == 0 && cr.TotalUsed == 0 && cr.TotalSize == 0 && len(cr.Packages) == 0 {
			continue
		}
		known++
		remain += cr.TotalRemain
		used += cr.TotalUsed
		size += cr.TotalSize
		packs += cr.PackCount
		if a.Provider == providerBigmodel {
			bmRemain += cr.TotalRemain
			bmUsed += cr.TotalUsed
			bmSize += cr.TotalSize
		} else {
			zaiRemain += cr.TotalRemain
			zaiUsed += cr.TotalUsed
			zaiSize += cr.TotalSize
		}
	}
	total := remain + used
	if size > total {
		total = size
	}
	return map[string]any{
		"account_count":   len(accounts),
		"known_count":     known,
		"disabled_count":  disabledN,
		"exhausted_count": exhaustedN,
		"pack_count":      packs,
		"total_remain":    remain,
		"total_used":      used,
		"total_size":      size,
		"total":           total,
		"zai_remain":      zaiRemain,
		"zai_used":        zaiUsed,
		"zai_size":        zaiSize,
		"bigmodel_remain": bmRemain,
		"bigmodel_used":   bmUsed,
		"bigmodel_size":   bmSize,
	}
}

// -----------------------------------------------------------------------------
// Management API routes + handler
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

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List ZCode accounts (Z.AI + BigModel) with plan, quota and cooldown status."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/lifecycle for all accounts."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time quota for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/plan", Description: "Switch an account's plan routing (body: {auth_index, plan: coding-plan|start-plan})."},
			{Method: http.MethodGet, Path: base + "/cooldowns", Description: "List active per-(account, model) cooldown entries."},
			{Method: http.MethodPost, Path: base + "/cooldowns/clear", Description: "Clear cooldown for one account (auth_id) or one pair (auth_id + model)."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "ZCode", Description: "ZCode dashboard (Z.AI + BigModel): plan, quota, cooldowns."},
		},
	}
}

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

	// Plugin-layer auth + rate limit for mutating endpoints (defence-in-depth
	// when management_key is configured; host middleware is trusted otherwise).
	if req.Method == http.MethodPost || mutatingManagementPath(path) {
		ip := managementClientIP(req)
		if status, msg := checkManagementAuth(req); status != 0 {
			if !allowManagementRequest(ip) {
				return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
					"error": "rate limit exceeded, try again later",
				}))
			}
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(false, false)))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(true, true)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req)))
	case req.Method == http.MethodPost && path == base+"/plan":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handlePlanSwitch(req)))
	case req.Method == http.MethodGet && path == base+"/cooldowns":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCooldownList(req)))
	case req.Method == http.MethodPost && path == base+"/cooldowns/clear":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCooldownClear(req)))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// mutatingManagementPath reports whether the path performs a write.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/select",
		base + "/plan",
		base + "/cooldowns/clear":
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit
// -----------------------------------------------------------------------------

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow (trust host middleware; the
// plugin-layer key is defence-in-depth only).
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, ""
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtleConstantTimeCompare(token, want) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
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

// -----------------------------------------------------------------------------
// Management endpoint implementations
// -----------------------------------------------------------------------------

// handleSelectAuth sets the panel-selected account used for chat routing.
func handleSelectAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required", "active_auth": getActiveAuthID()}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled {
			return map[string]any{"error": "账号已禁用，无法选中", "auth_index": authIndex}
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error(), "auth_index": authIndex}
		}
		setActiveAuthID(f.ID)
		return map[string]any{
			"ok":          true,
			"active_auth": f.ID,
			"provider":    authProviderFor(sa),
			"plan":        authPlanFor(sa),
			"uid":         sa.Account.UID,
		}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handlePlanSwitch rewrites an account's plan field (coding-plan ↔ start-plan).
// The executor refuses start-plan traffic until the Anthropic-format upstream
// ships, so this is a visible, deliberate operator action.
func handlePlanSwitch(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Plan      string `json:"plan"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	plan := normalizePlan(body.Plan)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		sa.Auth.Plan = plan
		note := displayNote(sa, nil, parseDisabledFromAuthJSON(nil))
		// Preserve on-disk disabled state + note credit segment.
		phys, err := hostAuthGetPhysicalFn(authIndex)
		disabled := false
		if err == nil && phys != nil {
			disabled = phys.Disabled
			note = displayNoteWithPrev(sa, nil, disabled, noteCreditsFromJSON(phys.JSON))
		}
		raw, err := buildAuthFileJSON(sa, disabled, note, nil)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		name, _ := resolveAuthFileTarget(sa, phys)
		if err := hostAuthPersistFn(name, raw); err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{"ok": true, "auth_index": authIndex, "plan": plan}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handleCreditsQuery returns real-time quota for one or all accounts.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			_, cr, errs := cachedAccountDetails(f.ID, sa, true)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"provider":   authProviderFor(sa),
				"plan":       authPlanFor(sa),
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"selected":   getActiveAuthID() == f.ID,
			}
			if len(errs) > 0 {
				acct["error"] = strings.Join(errs, "; ")
			}
			if cr != nil {
				acct["credits"] = cr
				acct["exhausted"] = isCreditsExhausted(cr)
			}
			// Propagate the fresh quota into the host auth-file note.
			_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, f.Disabled)
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: simplified list.
	type acctQuota struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Provider  string          `json:"provider"`
		Plan      string          `json:"plan"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctQuota
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctQuota{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		_, cr, errs := cachedAccountDetails(f.ID, sa, false)
		ac := acctQuota{
			AuthIndex: f.AuthIndex,
			Nickname:  sa.Account.Nickname,
			UID:       sa.Account.UID,
			Provider:  authProviderFor(sa),
			Plan:      authPlanFor(sa),
		}
		if len(errs) > 0 {
			ac.Error = strings.Join(errs, "; ")
		}
		ac.Credits = cr
		_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, f.Disabled)
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}

// Web panel (self-contained HTML, no external assets)

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	return panelHTML
}

//go:embed panel.html
var panelHTML []byte

// subtleConstantTimeCompare avoids importing crypto/subtle twice.
func subtleConstantTimeCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	if v == 0 {
		return 1
	}
	return 0
}
