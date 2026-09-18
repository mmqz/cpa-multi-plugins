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
	"log"
	"net/http"
	"strings"
	"sync"
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
		// DeepSeek V4.1 Flash: released 2026-09-10, official launch partner
		// WorkBuddy/CodeBuddy (deepseek.com news260910; free for 2 weeks).
		// 552B-backbone MoE, 1M context. Upstream ID follows the lowercase
		// convention of deepseek-v4-flash/-pro. Dynamic discovery is the
		// primary source; this static entry covers the discovery-failure
		// fallback so the rollout window stays visible.
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

// Realm static catalogs (v0.12.19). Policy: a static fallback entry must be
// SUPPORTED by that realm's upstream. A false negative (model exists but is
// not listed) heals via dynamic discovery or the models_cn/models_global/
// models_intl config pins; a false positive (model listed but not registered
// upstream) is a hard upstream 400 code 11102 "model [...] service info not
// found". So the non-CN catalogs only carry models with direct upstream
// evidence: deepseek-v4-pro/-flash (non-4.1) and minimax-* stay out even
// though they dominate the CN catalog, while hy3/glm/kimi appear in the
// external ones below because the live /v2 enterprise endpoints list them
// there, and deepseek-v4.1-flash is included per the 2026-09-10 official
// launch promotion (free 2 weeks, covers international realms too).
//
// Evidence: the external (Global/Intl) REAL user-facing catalog was captured
// live from the workbuddy.ai/v2 and codebuddy.ai/v2 enterprise endpoints
// (GET .../v2/enterprises/personal/models, HTTP 200) alongside the reference
// workbuddy2api project's static external list. The ids below are exactly
// what those external realms advertise — they are the actual model engines
// (OpenAI gpt-5.x family, Gemini, GLM, Kimi, Hunyuan, DeepSeek) rather than
// the IDE display-only tier slots (default-model/fast-model/...), which stay
// out of this STATIC fallback because they have a history of upstream 11102
// on some plans. Dynamic discovery still surfaces the tier slots live when
// the upstream itself returns them.
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
var discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
	return callModelsAPI(accessToken, realm, uid)
}

// extractAccountUID pulls the account uid out of a stored auth blob the same
// way extractAccessToken does (flat shape first, then the nested plugin OAuth
// shape). Used for the /v3/config X-User-Id header; empty = omit the header.
func extractAccountUID(raw []byte) string {
	var flat struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && strings.TrimSpace(flat.UID) != "" {
		return strings.TrimSpace(flat.UID)
	}
	var nested storedAuth
	if err := json.Unmarshal(raw, &nested); err == nil {
		return strings.TrimSpace(nested.Account.UID)
	}
	return ""
}

// fetchDynamicModelsFromStorage resolves ONE credential's advertised model
// list, in priority order:
//  1. the realm's pinned config (models_cn / models_global / models_intl) —
//     the user-written "supported models" list, which also skips discovery;
//  2. per-realm dynamic discovery, cached (v0.12.18);
//  3. the realm's static catalog (v0.12.19: per-realm, no longer the shared
//     CN-flavored list that made Intl credentials advertise
//     deepseek-v4-flash and fail with upstream 11102).
//
// v0.9.9: every branch records its decision into the realm's diagnostics
// entry (source + count + failure reason) so the panel and logs answer
// "why does this realm show these models" without guesswork. Discovery
// failures — the previously SILENT path that made realms look stuck on a
// 1-model static catalog — now log a throttled reason line.
func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	accessToken := ""
	if len(storageJSON) > 0 {
		if tok, ok := extractAccessToken(storageJSON); ok {
			accessToken = tok
		}
	}
	uid := extractAccountUID(storageJSON)
	realm := realmForStorage(storageJSON, accessToken)
	if pinned := pinnedModelsForRealm(realm); len(pinned) > 0 {
		noteRealmSource(realm, "pin", len(pinned))
		return pinned
	}
	if accessToken == "" {
		st := mergedFreeModels(realm)
		noteRealmSource(realm, "static (no token in storage)", len(st))
		return st
	}
	if cached, ok := cachedDynamicModels(realm); ok {
		return cached
	}
	dyn, err := discoverModelsFn(accessToken, realm, uid)
	if err != nil {
		noteRealmError(realm, err.Error())
		return mergedFreeModels(realm)
	}
	if len(dyn) == 0 {
		noteRealmError(realm, "discovery payload had no user-facing models")
		return mergedFreeModels(realm)
	}
	dyn = mergeFreeModelsForRealm(dyn, realm)
	storeDynamicModels(realm, dyn)
	log.Printf("models: realm=%s discovery ok: %d model(s)", realm, len(dyn))
	return dyn
}

// mergeFreeModels guarantees every free-trial model appears in the advertised
// list even when the account's discovery payload lags behind a promotional
// launch (e.g. deepseek-v4.1-flash free since 2026-09-10). A free model
// costs no credits, so advertising it is safe on accounts whose table omits
// it; without this, the missing entry makes the gateway answer model_not_found.
// Discovery stays authoritative for paid models: entries already present are
// left untouched (their metadata wins), only absent free ids are appended.
//
// mergedFreeModels is the same guarantee applied to the static fallback
// catalog. Every realm's static table must surface the free-trial ids too,
// otherwise a discovery outage above the realm table alone (e.g.
// global/hy4-preview) collapses the advertised list and the host registers a
// thin, inconsistent per-auth model list. mergeFreeModelsForRealm keeps the
// merge realm-bounded: the CN/global free promos must never leak onto an Intl
// account (upstream refuses them with 11102).
func mergedFreeModels(realm string) []pluginapi.ModelInfo {
	return mergeFreeModelsForRealm(staticModelsForRealm(realm), realm)
}

// mergeFreeModelsForRealm merges only the free-trial ids that are valid for
// the realm. Intl accounts (codebuddy.ai) must never be handed a CN promo
// model; the dynamic-discovery path shares this guard so a stale Intl table
// can't advertise a model the upstream account cannot serve.
func mergeFreeModelsForRealm(models []pluginapi.ModelInfo, realm string) []pluginapi.ModelInfo {
	if realm == "intl" {
		return models
	}
	return mergeFreeModels(models)
}

func mergeFreeModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	seen := make(map[string]bool, len(models)+len(freeModels))
	for _, m := range models {
		if m.ID != "" {
			seen[strings.ToLower(m.ID)] = true
		}
	}
	for _, id := range freeModelIDs() {
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		models = append(models, pluginapi.ModelInfo{
			ID:                         id,
			Name:                       id,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return models
}

// realmModelsState is the dashboard-facing snapshot of one realm's model
// source — the answer to "why does this realm list these models".
type realmModelsState struct {
	Source     string `json:"source"`
	Count      int    `json:"count"`
	FetchedAt  string `json:"fetched_at,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	LastErrorA string `json:"last_error_at,omitempty"`
}

// noteRealmSource records that realm's advertised list came from a non-
// discovery source (config pin or static fallback).
func noteRealmSource(realm, source string, count int) {
	dynamicModelsCache.Lock()
	defer dynamicModelsCache.Unlock()
	entry := dynamicModelsCache.realms[realm]
	entry.source = source
	entry.srcCount = count
	dynamicModelsCache.realms[realm] = entry
}

// noteRealmError records a discovery failure for the realm and logs it with a
// per-realm throttle: immediately on a NEW message, otherwise at most once a
// minute (model.for_auth can fire per models query, and silent failure is
// exactly what made thin/stale model lists undiagnosable).
//
// A transient failure must NOT wipe the last successful discovery. The host
// re-runs model.for_auth per auth over time; blanking models here makes
// different auths register different lists at different instants, which the
// host then turns into a shrunken candidate pool for routing (one account
// seen as the only supporter of a model). Only when nothing was ever cached
// do we fall over to the static catalog.
func noteRealmError(realm, msg string) {
	now := time.Now()
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.realms[realm]
	fallback := mergedFreeModels(realm)
	// Keep a previously discovered model list: a transient discovery failure
	// does not make those models invalid, and clearing them would leave host
	// per-auth registrations divergent (the exact inconsistency that shrank
	// the candidate pool to one account). On the FIRST failure — nothing
	// cached yet — we record the static-fallback state so the dashboard shows
	// where the list came from while we serve the merged static catalog.
	if len(entry.models) == 0 && entry.fetched.IsZero() {
		entry.source = "static (discovery failed)"
		entry.srcCount = len(fallback)
		entry.fetched = now // mark so a later failure stays "static" until discovery succeeds
	}
	entry.lastErr = msg
	entry.lastErrAt = now
	shouldLog := entry.lastLogAt.IsZero() || now.Sub(entry.lastLogAt) >= time.Minute
	if shouldLog {
		entry.lastLogAt = now
	}
	dynamicModelsCache.realms[realm] = entry
	dynamicModelsCache.Unlock()
	if shouldLog {
		log.Printf("models: realm=%s discovery failed (%s) — serving previously cached list (or static catalog if none) until next successful discovery", realm, msg)
	}
}

// realmModelStateFor snapshots the realm's diagnostics for the dashboard.
// Returns nil when the realm has never been resolved (host has not queried
// models for it yet).
func realmModelStateFor(realm string) *realmModelsState {
	dynamicModelsCache.RLock()
	entry, ok := dynamicModelsCache.realms[realm]
	if !ok {
		dynamicModelsCache.RUnlock()
		return nil
	}
	st := &realmModelsState{
		Source:    entry.source,
		Count:     len(entry.models),
		LastError: entry.lastErr,
	}
	if st.Count == 0 {
		st.Count = entry.srcCount
	}
	if !entry.fetched.IsZero() {
		st.FetchedAt = entry.fetched.Format("2006-01-02 15:04:05")
		st.AgeSeconds = int64(time.Since(entry.fetched).Seconds())
	}
	if !entry.lastErrAt.IsZero() {
		st.LastErrorA = entry.lastErrAt.Format("2006-01-02 15:04:05")
	}
	dynamicModelsCache.RUnlock()
	if st.Source == "" && st.LastError == "" {
		return nil
	}
	return st
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
	dynamicModelsCache.realms[realm] = realmModelsEntry{models: models, fetched: time.Now(), source: "discovery"}
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

// modelsEndpointCandidates returns the ordered enterprise-catalog URLs to try
// per realm. Following the external (Global/Intl) findings from the reference
// workbuddy2api project, the /v2/enterprises/personal/models family is
// probed FIRST for the external realms — the /console family returns HTTP 500
// (openresty/apigate) on those realms even though the account's credentials
// are valid, which leaves discovery on the narrow /v3/config tier-slot names
// only. CN keeps the /console path (its live shape).
func modelsEndpointCandidates(realm string) []string {
	switch realm {
	case "global":
		return []string{
			upstreamBaseGlobal + "/v2/enterprises/personal/models",
			upstreamBaseGlobal + "/console/enterprises/personal/models",
		}
	case "intl":
		return []string{
			upstreamBaseIntl + "/v2/enterprises/personal/models",
			upstreamBaseIntl + "/console/enterprises/personal/models",
		}
	default:
		return []string{endpointModels}
	}
}

// callModelsAPI resolves a realm's live model catalog. v0.9.12 dual probe
// (mirrors workbuddy2api-panel FetchModels):
//   - enterprise: GET /console/enterprises/personal/models — the account's
//     registration table; cli agent list gives ordering, data.models the
//     capabilities. Ordering authority.
//   - v3/config: the official IDE configuration catalog — UA-sensitive
//     (CodeBuddyIDE/* required; CLI UAs get a reduced table), returns the
//     full capability set (maxInputTokens/maxOutputTokens, supportsImages,
//     reasoning supportedEfforts, tags) and family models the enterprise
//     table lacks (gpt-5.3-codex etc. on global).
//
// Both probes run concurrently with independent failure handling: one
// path's failure degrades to the other (warn logged), only a double
// failure fails the call with BOTH reasons. Realm semantics unchanged
// (v0.12.18): Global tokens query workbuddy.ai, Intl (codebuddy.ai)
// tokens query codebuddy.ai, CN tokens query copilot.tencent.com.
func callModelsAPI(accessToken string, realm ...string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// An empty realm keeps the legacy JWT-iss derivation (Global vs CN)
	// for old callers.
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
	uid := ""
	if len(realm) > 1 {
		uid = realm[1]
	}
	type probe struct {
		enterprise []pluginapi.ModelInfo
		v3         []discoveredModel
		entErr     error
		v3Err      error
	}
	res := probe{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res.enterprise, res.entErr = callEnterpriseModelsAPI(ctx, accessToken, r)
	}()
	go func() {
		defer wg.Done()
		res.v3, res.v3Err = fetchV3ConfigModels(ctx, accessToken, r, uid)
	}()
	wg.Wait()
	if res.entErr != nil && res.v3Err != nil {
		return nil, fmt.Errorf("models discovery failed: enterprise: %v; v3/config: %v",
			res.entErr, res.v3Err)
	}
	if res.entErr != nil {
		log.Printf("models: realm=%s enterprise probe failed (v3/config only): %v", r, res.entErr)
	}
	if res.v3Err != nil {
		log.Printf("models: realm=%s v3/config probe failed (enterprise only): %v", r, res.v3Err)
	}
	out := mergeDiscoveryLists(res.enterprise, res.v3)
	if len(out) == 0 {
		return nil, fmt.Errorf("no user-facing models in discovery payload (cli agent list empty and data.models empty/disabled)")
	}
	return out, nil
}

// callEnterpriseModelsAPI GETs the realm's enterprise model catalog and
// builds the ordered list (cli agent base + promoted entries). External
// realms (global/intl) probe the /v2/enterprises/personal/models family
// first, falling back to the /console family — the /console path serves a
// 500 (openresty) on external realms, which previously left discovery stuck
// on the narrow /v3/config tier names.
func callEnterpriseModelsAPI(ctx context.Context, accessToken, realm string) ([]pluginapi.ModelInfo, error) {
	candidates := modelsEndpointCandidates(realm)
	origin := originReferer
	switch realm {
	case "global":
		origin = originRefererGlobal
	case "intl":
		origin = originRefererIntl
	}
	var lastErr error
	for _, modelsURL := range candidates {
		out, err := fetchEnterpriseEndpointOnce(ctx, accessToken, realm, modelsURL, origin)
		if err != nil {
			lastErr = err
			continue
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, lastErr
}

// fetchEnterpriseEndpointOnce performs a single enterprise-model GET and
// parses the {code,data{models,agents}} payload into discovery entries.
func fetchEnterpriseEndpointOnce(ctx context.Context, accessToken, realm, modelsURL, origin string) ([]pluginapi.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	if realm == "intl" {
		// The Intl gateway expects the IDE client header set (parity with
		// applyRealmHeaders on the billing path).
		req.Header.Set("X-IDE-Type", "IDE")
		req.Header.Set("X-IDE-Name", "CodeBuddy")
		req.Header.Set("X-IDE-Version", "1.100.0")
		req.Header.Set("X-Product-Version", "1.100.0")
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", modelsURL, err)
	}
	body := resp.Body
	if resp.StatusCode != http.StatusOK {
		// v0.12.49: carry the URL and a body snippet in the error — the
		// panel/log then shows whether the gateway answered with a login
		// redirect (302 HTML), an auth wall (401), or a server fault (5xx)
		// instead of a bare status code.
		snippet := strings.TrimSpace(string(body))
		snippet = strings.Map(func(r rune) rune {
			if r == 0x09 || r == 0x0A || r == 0x0D || (r >= 0x20 && r != 0x7F) {
				return r
			}
			return -1
		}, snippet)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		if snippet == "" {
			snippet = "(empty body)"
		}
		return nil, fmt.Errorf("models API status %d from %s: %s", resp.StatusCode, modelsURL, snippet)
	}
	var apiResp struct {
		Code int `json:"code"`
		Data struct {
			Models []discoveredModel `json:"models"`
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
	out := modelsFromDiscovery(apiResp.Data.Models, cliModelIDs)
	if len(out) == 0 {
		return nil, fmt.Errorf("models API payload had no selectable chat models")
	}
	return out, nil
}

// v3ConfigUA is the IDE User-Agent the /v3/config catalog requires. The
// endpoint is UA-sensitive: the CLI three-segment WorkBuddy UA gets a
// reduced table (flash output capped 128K, no supportedEfforts), while the
// official IDE shape returns full capabilities (flash: 393216 + low/high/
// max). The version must track upstream IDE releases; stale versions may
// also serve the reduced table (workbuddy2api-panel reference detail).
const v3ConfigUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// v3ConfigEndpointFor returns the /v3/config URL per realm. Same bases as
// the model endpoints; realm keys: "cn" | "global" | "intl".
func v3ConfigEndpointFor(realm string) string {
	switch realm {
	case "global":
		return upstreamBaseGlobal + "/v3/config"
	case "intl":
		return upstreamBaseIntl + "/v3/config"
	default:
		return upstreamBaseCN + "/v3/config"
	}
}

// v3ConfigDomainFor is the X-Domain header value: the realm's console host.
func v3ConfigDomainFor(realm string) string {
	switch realm {
	case "global":
		return strings.TrimPrefix(originRefererGlobal, "https://")
	case "intl":
		return strings.TrimPrefix(originRefererIntl, "https://")
	default:
		return strings.TrimPrefix(upstreamBaseCN, "https://")
	}
}

// fetchV3ConfigModels probes GET /v3/config (official IDE config catalog).
// Response envelope: {code, data:{models:[...]}} — the models array carries
// the full capability set. Transport/HTTP errors are wrapped with the URL
// and a body snippet for the discovery-failure diagnostics line.
func fetchV3ConfigModels(ctx context.Context, accessToken, realm, uid string) ([]discoveredModel, error) {
	endpoint := v3ConfigEndpointFor(realm)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Domain", v3ConfigDomainFor(realm))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", v3ConfigUA)
	req.Header.Set("X-CodeBuddy-Request", "1")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(resp.Body))
		snippet = strings.Map(func(r rune) rune {
			if r == 0x09 || r == 0x0A || r == 0x0D || (r >= 0x20 && r != 0x7F) {
				return r
			}
			return -1
		}, snippet)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		if snippet == "" {
			snippet = "(empty body)"
		}
		return nil, fmt.Errorf("v3/config status %d from %s: %s", resp.StatusCode, endpoint, snippet)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []discoveredModel `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code %d", env.Code)
	}
	out := make([]discoveredModel, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if m.ID == "" || m.Disabled || m.isNonChat() {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned no selectable chat models")
	}
	return out, nil
}

// discoveredModel is one entry of the discovery payload's data.models array.
type discoveredModel struct {
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
	// v0.9.12: /v3/config generation field names (maxInputTokens/
	// maxOutputTokens) and capability extras (tags/vendor/supportsExtra).
	// The enterprise endpoint historically used contextWindow/maxTokens;
	// both shapes are accepted so one struct serves both probes (0/absent
	// falls back to the other).
	MaxInputTokens  json.RawMessage `json:"maxInputTokens"`
	MaxOutputTokens json.RawMessage `json:"maxOutputTokens"`
	Tags            []string        `json:"tags"`
	Vendor          string          `json:"vendor"`
	SupportsExtra   bool            `json:"supportsExtra"`
}

// inputTokens returns the effective max input tokens: the /v3/config
// maxInputTokens field when present, else the enterprise contextWindow.
func (m discoveredModel) inputTokens() int64 {
	if v := rawJSONI64(m.MaxInputTokens); v > 0 {
		return v
	}
	return rawJSONI64(m.ContextWindow)
}

// outputTokens returns the effective max output tokens: maxOutputTokens when
// present, else the enterprise maxTokens field.
func (m discoveredModel) outputTokens() int64 {
	if v := rawJSONI64(m.MaxOutputTokens); v > 0 {
		return v
	}
	return rawJSONI64(m.MaxTokens)
}

// reasoningMeta decodes the reasoning object's effort controls. Both endpoint
// generations nest it under "reasoning"; supportedEfforts is the enumerable
// level list (absent on single-effort models like glm-5.1/kimi).
type reasoningMeta struct {
	SupportedEfforts   []string `json:"supportedEfforts"`
	DefaultEffort      string   `json:"defaultEffort"`
	Effort             string   `json:"effort"`
	CanDisableThinking bool     `json:"canDisableThinking"`
}

func (m discoveredModel) reasoning() reasoningMeta {
	var r reasoningMeta
	if len(m.Reasoning) > 0 && string(m.Reasoning) != "null" {
		_ = json.Unmarshal(m.Reasoning, &r)
	}
	return r
}

// nonChatModel reports whether a discovery entry is a non-chat model that
// must never reach the selectable list. Mirrors harness buddy.ts isChatModel
// and workbuddy2api-panel nonChatModel (both verified against live payloads):
//   - id prefix nes-/completion-/codewise-: embedding/completion/code-only
//     models; selecting one dies with code 11102.
//   - supportsExtra (codewise-completions/rewrite/jump markers): IDE-internal
//     completions endpoints, not user-facing chat models.
//   - maxOutputTokens in (0,256]: tiny-output completion models (chat models
//     are >= 24000 upstream).
//   - tags contain text-to-image: image GENERATION models (hunyuan-image-*),
//     not chat-with-vision — a chat request to them fails.
func nonChatModel(id string, maxOutputTokens int64, tags []string, supportsExtra bool) bool {
	lid := strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(lid, p) {
			return true
		}
	}
	if supportsExtra {
		return true
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), "text-to-image") {
			return true
		}
	}
	return false
}

// isNonChat applies nonChatModel to this entry.
func (m discoveredModel) isNonChat() bool {
	return nonChatModel(m.ID, m.outputTokens(), m.Tags, m.SupportsExtra)
}

// rawJSONI64 decodes a JSON number field that may be number, numeric string
// or null; any other shape decodes to 0.
func rawJSONI64(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err == nil {
		return int64(v)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
			return int64(f)
		}
	}
	return 0
}

// modelsFromDiscovery builds the advertised list from one discovery payload.
// v0.9.8: the cli agent's model IDs form the base (order preserved), then any
// ENABLED data.models entry missing from that list is PROMOTED. Tencent's
// data.models is the account's own registration table — the official client
// picker shows exactly these — so a freshly rolled-out model (e.g.
// deepseek-v4.1-flash on 2026-09-10) must surface even while the cli agent
// list still lags; the old cli-only filter made the plugin trail the official
// client on every model launch. Promotions are logged so a potential upstream
// 11102 ("service info not found") chat failure is traceable to this decision.
// A renamed/missing cli agent no longer nukes discovery either: enabled
// data.models alone still produce the list (before: hard error → stale static
// fallback).
// v0.9.12: cli entries and promotions pass the nonChatModel filter — the
// registration table also carries completion/code-only/text-to-image entries
// that die with 11102/11133 when selected (closes the v0.9.8 promotion hole
// where such an entry could be promoted into the chat list).
func modelsFromDiscovery(dataModels []discoveredModel, cliModelIDs []string) []pluginapi.ModelInfo {
	byID := make(map[string]discoveredModel, len(dataModels))
	for _, m := range dataModels {
		if m.ID != "" {
			byID[m.ID] = m
		}
	}
	seen := make(map[string]bool, len(cliModelIDs)+len(dataModels))
	out := make([]pluginapi.ModelInfo, 0, len(cliModelIDs)+len(dataModels))
	for _, id := range cliModelIDs {
		m, ok := byID[id]
		if !ok || m.Disabled || m.isNonChat() {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, discoverToInfo(m))
	}
	var promoted []string
	for _, m := range dataModels {
		if m.ID == "" || m.Disabled || m.isNonChat() {
			continue
		}
		key := strings.ToLower(m.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, discoverToInfo(m))
		promoted = append(promoted, m.ID)
	}
	if len(promoted) > 0 {
		log.Printf("models: promoted %d upstream model(s) not in cli agent list: %s",
			len(promoted), strings.Join(promoted, ", "))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// discoverToInfo maps one discovery entry to the host-facing ModelInfo.
// v0.9.11: surface the registration table's own modality flags —
// supportsImages && !disabledMultimodal is Tencent's statement that the
// model accepts image input; advertising it lets modality-aware clients
// offer attachments correctly instead of guessing. Static catalogs stay
// un-declared (no per-realm upstream evidence — same policy as model IDs
// there).
// v0.9.12: also surface the token budgets (both endpoint generations'
// field names) and the reasoning effort controls into Thinking.Levels/
// ZeroAllowed.
func discoverToInfo(m discoveredModel) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         m.ID,
		Name:                       m.Name,
		ContextLength:              m.inputTokens(),
		InputTokenLimit:            m.inputTokens(),
		MaxCompletionTokens:        m.outputTokens(),
		OutputTokenLimit:           m.outputTokens(),
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
	if info.Name == "" {
		info.Name = info.ID
	}
	if m.SupportsImages && !m.DisabledMultimodal {
		info.SupportedInputModalities = []string{"text", "image"}
	}
	if r := m.reasoning(); len(r.SupportedEfforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels:      append([]string(nil), r.SupportedEfforts...),
			ZeroAllowed: r.CanDisableThinking,
		}
	}
	return info
}

// mergeDiscoveryLists overlays v3/config capability data onto the enterprise
// list and appends v3-only models. Merge key = lowercase model id; per the
// workbuddy2api-panel reference the v3 entry wins for capability fields
// (full IDE UA catalog: real context windows, effort levels) while the
// enterprise probe remains the ordering authority (cli agent base + recent
// promotions). v3-only entries (gpt-5.3-codex etc. on global) are appended
// in v3 order after the enterprise block.
func mergeDiscoveryLists(enterprise []pluginapi.ModelInfo, v3 []discoveredModel) []pluginapi.ModelInfo {
	if len(v3) == 0 {
		return enterprise
	}
	caps := make(map[string]discoveredModel, len(v3))
	var v3Only []discoveredModel
	seenEnt := make(map[string]bool, len(enterprise))
	for _, mi := range enterprise {
		seenEnt[strings.ToLower(mi.ID)] = true
	}
	for _, m := range v3 {
		if m.ID == "" {
			continue
		}
		key := strings.ToLower(m.ID)
		if seenEnt[key] {
			caps[key] = m
		} else {
			v3Only = append(v3Only, m)
		}
	}
	out := make([]pluginapi.ModelInfo, 0, len(enterprise)+len(v3Only))
	for _, mi := range enterprise {
		if vm, ok := caps[strings.ToLower(mi.ID)]; ok {
			mi = overlayModelCaps(mi, vm)
		}
		out = append(out, mi)
	}
	for _, m := range v3Only {
		if m.Disabled || m.isNonChat() {
			continue
		}
		out = append(out, discoverToInfo(m))
	}
	return out
}

// overlayModelCaps fills enterprise ModelInfo gaps from the v3 entry. Every
// field is only taken when the v3 value is non-zero AND the enterprise value
// is empty, so live-verified enterprise data is never downgraded by a
// partial v3 entry. The modality rule matches discoverToInfo: advertise
// image input only on an explicit upstream statement.
func overlayModelCaps(base pluginapi.ModelInfo, v3 discoveredModel) pluginapi.ModelInfo {
	if base.ContextLength <= 0 {
		base.ContextLength = v3.inputTokens()
	}
	if base.InputTokenLimit <= 0 {
		base.InputTokenLimit = v3.inputTokens()
	}
	if base.MaxCompletionTokens <= 0 {
		base.MaxCompletionTokens = v3.outputTokens()
	}
	if base.OutputTokenLimit <= 0 {
		base.OutputTokenLimit = v3.outputTokens()
	}
	if len(base.SupportedInputModalities) == 0 && v3.SupportsImages && !v3.DisabledMultimodal {
		base.SupportedInputModalities = []string{"text", "image"}
	}
	if base.Thinking == nil {
		if r := v3.reasoning(); len(r.SupportedEfforts) > 0 {
			base.Thinking = &pluginapi.ThinkingSupport{
				Levels:      append([]string(nil), r.SupportedEfforts...),
				ZeroAllowed: r.CanDisableThinking,
			}
		}
	}
	if (base.Name == "" || base.Name == base.ID) && v3.Name != "" {
		base.Name = v3.Name
	}
	return base
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
	// Always return provider_registered_key. The host skips any
	// response whose Provider doesn't match the auth's ProviderID, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	log.Printf("models: model.for_auth auth=%q provider=%q count=%d ids=%v", req.AuthID, req.AuthProvider, len(models), ids)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
