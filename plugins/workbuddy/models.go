// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels is the CN realm static catalog (copilot.tencent.com), mirroring
// Tencent's official CN built-in model table. v0.12.19: this list is NO
// LONGER the fallback for Intl/Global credentials — see staticModelsForRealm.
// Sharing it across realms is exactly what made Intl credentials advertise
// deepseek-v4-flash and die with upstream 11102 "service info not found".
func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.1", Name: "GLM-5.1", ContextLength: 131072, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5v-turbo", Name: "GLM-5V Turbo", ContextLength: 131072, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.7", Name: "Kimi K2.7", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "minimax-m3", Name: "MiniMax M3", ContextLength: 204800, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3", Name: "Hy3", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3-preview", Name: "Hy3 Preview", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3-preview-agent", Name: "Hy3 Preview Agent", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		// Hy4 Preview: Tencent Hunyuan 4 preview (770B/A49B, 1M context),
		// free for 14 days in WorkBuddy/CodeBuddy since 2026-08-28 launch.
		{ID: "hy4-preview", Name: "Hy4 Preview", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

// Realm static catalogs (v0.12.19). Policy: a static fallback entry must be
// SUPPORTED by that realm's upstream. A false negative (model exists but is
// not listed) heals via dynamic discovery or the models_cn/models_global/
// models_intl config pins; a false positive (model listed but not registered
// upstream) is a hard upstream 400 code 11102 "model [...] service info not
// found". So the non-CN catalogs only carry models with direct upstream
// evidence, and CN brand models (deepseek-v4-*, glm-*, kimi-*, minimax-*,
// hy3*) stay out of them even though they dominate the CN catalog.
//
// Evidence for intl/global hy4-preview: Tencent's 2026-08-28 Hy4 Preview
// launch covers WorkBuddy/CodeBuddy CN AND international editions (official
// announcement; upstream API id "hy4-preview"). Community sessions on the
// intl CLI additionally show claude/gpt/gemini families, but their exact
// upstream IDs could not be verified without a live intl token — add them
// via the models_intl / models_global config pins instead of guessing here.
func staticModelsGlobal() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "hy4-preview", Name: "Hy4 Preview", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

// staticModelsIntl is the codebuddy.ai (Intl) fallback catalog. See
// staticModelsGlobal for the inclusion policy.
func staticModelsIntl() []pluginapi.ModelInfo {
	return staticModelsGlobal()
}

// staticModelsForRealm dispatches a realm key to its static catalog. The
// legacy default stays the CN list for unknown realms.
func staticModelsForRealm(realm string) []pluginapi.ModelInfo {
	switch realm {
	case "intl":
		return staticModelsIntl()
	case "global":
		return staticModelsGlobal()
	default:
		return wbModels()
	}
}

// pinnedModelsForRealm returns the ModelInfo list pinned via config_yaml
// models_cn / models_global / models_intl for this realm, or nil. Pinned
// lists are authoritative: they replace discovery for that realm entirely,
// so the credential output is exactly the user-written "supported models"
// list (the v0.12.19 formalization of writing supported models into the
// credential output).
func pinnedModelsForRealm(realm string) []pluginapi.ModelInfo {
	ids := pinnedModelIDsForRealm(realm)
	if len(ids) == 0 {
		return nil
	}
	return buildModelInfos(ids)
}

// buildModelInfos maps raw upstream model IDs to ModelInfo, reusing the
// static catalogs' metadata for known IDs and generic defaults otherwise —
// an unknown ID still gets advertised because the user pinned it deliberately.
func buildModelInfos(ids []string) []pluginapi.ModelInfo {
	meta := map[string]pluginapi.ModelInfo{}
	for _, m := range wbModels() {
		meta[strings.ToLower(m.ID)] = m
	}
	for _, m := range staticModelsGlobal() {
		meta[strings.ToLower(m.ID)] = m
	}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if m, ok := meta[strings.ToLower(id)]; ok {
			out = append(out, m)
			continue
		}
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}})
	}
	return out
}

// parsePinnedModelList decodes a models_* config value: a comma-separated
// upstream model ID list, optionally quoted or YAML flow-style ([a, b]).
// Returns trimmed, de-duplicated (case-insensitive), non-empty IDs in order.
func parsePinnedModelList(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"'")
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	seen := map[string]struct{}{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

// discoverModelsFn is the seam for upstream realm discovery; tests swap it
// out to stay off the network.
var discoverModelsFn = func(accessToken, realm string) ([]pluginapi.ModelInfo, error) {
	return callModelsAPI(accessToken, realm)
}

// fetchDynamicModelsFromStorage resolves ONE credential's advertised model
// list, in priority order:
//  1. the realm's pinned config (models_cn / models_global / models_intl) —
//     the user-written "supported models" list, which also skips discovery;
//  2. per-realm dynamic discovery, cached (v0.12.18);
//  3. the realm's static catalog (v0.12.19: per-realm, no longer the shared
//     CN-flavored list that made Intl credentials advertise
//     deepseek-v4-flash and fail with upstream 11102).
func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	accessToken := ""
	if len(storageJSON) > 0 {
		if tok, ok := extractAccessToken(storageJSON); ok {
			accessToken = tok
		}
	}
	realm := realmForStorage(storageJSON, accessToken)
	if pinned := pinnedModelsForRealm(realm); len(pinned) > 0 {
		return pinned
	}
	if accessToken == "" {
		return staticModelsForRealm(realm)
	}
	if models, ok := cachedDynamicModels(realm); ok {
		return models
	}
	if dyn, err := discoverModelsFn(accessToken, realm); err == nil && len(dyn) > 0 {
		storeDynamicModels(realm, dyn)
		return dyn
	}
	return staticModelsForRealm(realm)
}

// cachedDynamicModels returns the cached discovery result for ONE realm.
// v0.12.18: the cache is keyed by realm (cn|global|intl) — a single shared
// entry let a CN discovery answer satisfy model.for_auth for an Intl account
// (and vice versa), advertising models the account's gateway never served.
func cachedDynamicModels(realm string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.realms[realm]
	if !ok || len(entry.models) == 0 || time.Since(entry.fetched) >= dynamicModelsCacheTTL {
		return nil, false
	}
	return entry.models, true
}

func storeDynamicModels(realm string, models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.realms[realm] = realmModelsEntry{models: models, fetched: time.Now()}
	dynamicModelsCache.Unlock()
}

// realmForStorage classifies an auth storage blob into its upstream realm
// ("cn" | "global" | "intl"). Model discovery and the 11102 error hint both
// need the realm, but callers only have the raw storage JSON: plugin OAuth
// files are the nested {auth:{domain,region,...}} shape, credentials imported
// through the CPA manager UI may be flat {domain}/{region}, and legacy files
// carry neither — for those the JWT iss decides (Global vs CN).
func realmForStorage(raw []byte, accessToken string) string {
	var probe struct {
		Auth struct {
			Domain string `json:"domain"`
			Region string `json:"region"`
		} `json:"auth"`
		Domain string `json:"domain"`
		Region string `json:"region"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if r := realmFromRegionDomain(probe.Auth.Region, probe.Auth.Domain); r != "" {
			return r
		}
		if r := realmFromRegionDomain(probe.Region, probe.Domain); r != "" {
			return r
		}
	}
	if isGlobalToken(accessToken) {
		return "global"
	}
	return "cn"
}

// realmFromRegionDomain maps one (region, domain) pair to a realm key, or ""
// when neither field identifies a realm (empty/legacy files).
func realmFromRegionDomain(region, domain string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "intl":
		return "intl"
	case "global":
		return "global"
	case "cn":
		return "cn"
	}
	d := strings.ToLower(strings.TrimSpace(domain))
	if isGlobalDomain(d) {
		return "global"
	}
	if isIntlDomain(d) {
		return "intl"
	}
	return ""
}

// fetchDynamicModels calls the WorkBuddy API to get the latest model list.
// Falls back to the hardcoded list on any error.
// extractAccessToken handles both flat (CPA UI) and nested (plugin OAuth) auth file shapes.
func extractAccessToken(raw []byte) (string, bool) {
	// flat shape from CPA-Manager-Plus UI
	var flat struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && strings.TrimSpace(flat.AccessToken) != "" {
		return flat.AccessToken, true
	}
	// nested shape from plugin OAuth
	var nested storedAuth
	if err := json.Unmarshal(raw, &nested); err == nil && strings.TrimSpace(nested.Auth.AccessToken) != "" {
		return nested.Auth.AccessToken, true
	}
	return "", false
}

// realmFromToken decodes the JWT iss claim to determine the account realm.
// Global tokens have iss=...workbuddy.ai...; CN tokens have iss=...codebuddy.cn...
// Returns true if the token is Global.
func isGlobalToken(accessToken string) bool {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return false
	}
	payload := parts[1]
	// base64url padding
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var claims struct {
		ISS string `json:"iss"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return strings.Contains(strings.ToLower(claims.ISS), "workbuddy.ai")
}

// modelsEndpointFor returns the per-realm model-discovery URL and the
// Origin/Referer base for it. Realm keys: "cn" | "global" | "intl".
func modelsEndpointFor(realm string) (modelsURL, origin string) {
	switch realm {
	case "global":
		return upstreamBaseGlobal + "/console/enterprises/personal/models", originRefererGlobal
	case "intl":
		return upstreamBaseIntl + "/console/enterprises/personal/models", originRefererIntl
	default:
		return endpointModels, originReferer
	}
}

// callModelsAPI GETs /console/enterprises/personal/models from the upstream.
// Uses the shared client (connection pooling) with a per-request 15s budget;
// the shared client's own 120s timeout stays as the outer bound.
func callModelsAPI(accessToken string, realm ...string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Model discovery is per-realm (v0.12.18): Global tokens query
	// workbuddy.ai, Intl (codebuddy.ai) tokens query codebuddy.ai, and CN
	// tokens query copilot.tencent.com. The old code only special-cased
	// Global and sent Intl tokens to the CN endpoint, whose answer (or the
	// static fallback) then advertised CN-only models like deepseek-v4-flash
	// to Intl accounts — the Intl gateway rejected those with code 11102
	// "model [...] service info not found". An empty realm keeps the legacy
	// JWT-iss derivation (Global vs CN) for old callers.
	r := ""
	if len(realm) > 0 {
		r = realm[0]
	}
	if r == "" {
		if isGlobalToken(accessToken) {
			r = "global"
		} else {
			r = "cn"
		}
	}
	modelsURL, origin := modelsEndpointFor(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	if r == "intl" {
		// The Intl gateway expects the IDE client header set (parity with
		// applyRealmHeaders on the billing path).
		req.Header.Set("X-IDE-Type", "IDE")
		req.Header.Set("X-IDE-Name", "CodeBuddy")
		req.Header.Set("X-IDE-Version", "1.100.0")
		req.Header.Set("X-Product-Version", "1.100.0")
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	body := resp.Body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	var apiResp struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                 string          `json:"id"`
				Name               string          `json:"name"`
				Description        string          `json:"description"`
				Credits            string          `json:"credits"`
				Configurable       bool            `json:"configurable"`
				Configured         bool            `json:"configured"`
				IsDefault          bool            `json:"isDefault"`
				SupportsImages     bool            `json:"supportsImages"`
				SupportsReasoning  bool            `json:"supportsReasoning"`
				OnlyReasoning      bool            `json:"onlyReasoning"`
				Reasoning          json.RawMessage `json:"reasoning"`
				DisabledMultimodal bool            `json:"disabledMultimodal"`
				Disabled           bool            `json:"disabled"`
				DisabledReason     string          `json:"disabledReason"`
				ContextWindow      json.RawMessage `json:"contextWindow"`
				MaxTokens          json.RawMessage `json:"maxTokens"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}
	if apiResp.Code != 0 {
		return nil, fmt.Errorf("models API code %d", apiResp.Code)
	}
	var cliModelIDs []string
	for _, a := range apiResp.Data.Agents {
		if a.Name == "cli" {
			cliModelIDs = a.Models
			break
		}
	}
	if len(cliModelIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID                 string          `json:"id"`
		Name               string          `json:"name"`
		Description        string          `json:"description"`
		Credits            string          `json:"credits"`
		Configurable       bool            `json:"configurable"`
		Configured         bool            `json:"configured"`
		IsDefault          bool            `json:"isDefault"`
		SupportsImages     bool            `json:"supportsImages"`
		SupportsReasoning  bool            `json:"supportsReasoning"`
		OnlyReasoning      bool            `json:"onlyReasoning"`
		Reasoning          json.RawMessage `json:"reasoning"`
		DisabledMultimodal bool            `json:"disabledMultimodal"`
		Disabled           bool            `json:"disabled"`
		DisabledReason     string          `json:"disabledReason"`
		ContextWindow      json.RawMessage `json:"contextWindow"`
		MaxTokens          json.RawMessage `json:"maxTokens"`
	}, len(apiResp.Data.Models))
	for _, m := range apiResp.Data.Models {
		dynMap[m.ID] = m
	}
	var out []pluginapi.ModelInfo
	for _, id := range cliModelIDs {
		m, ok := dynMap[id]
		if !ok {
			continue
		}
		if m.Disabled {
			continue
		}
		ctxLen := int64(0)
		if len(m.ContextWindow) > 0 {
			var v float64
			if err := json.Unmarshal(m.ContextWindow, &v); err == nil {
				ctxLen = int64(v)
			}
		}
		maxTok := int64(0)
		if len(m.MaxTokens) > 0 {
			var v float64
			if err := json.Unmarshal(m.MaxTokens, &v); err == nil {
				maxTok = int64(v)
			}
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.ID,
			Name:                       m.Name,
			ContextLength:              ctxLen,
			MaxCompletionTokens:        maxTok,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out, nil
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the workbuddy provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := wbModels()
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
