// models.go implements the ModelProvider capability: per-auth model lists
// via upstream discovery, alias reverse resolution (client-facing alias →
// upstream model id), and the host-config oauth-excluded-models filter.
// v0.9.33: the hand-maintained static catalogs were removed — discovery
// (fresh > cached > stale) is the only advertisement source; see the
// historical note in this file.
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

// Historical note (v0.9.33, 2026-09-22): this file previously carried
// hand-maintained per-realm static catalogs — CN: glm-5.2/glm-5.1/glm-5v-turbo,
// kimi-k2.7, minimax-m3, hy3/hy3-preview/hy3-preview-agent, hy4-preview,
// deepseek-v4-pro/deepseek-v4-flash/deepseek-v4.1-flash; Intl/Global:
// hy4-preview. Upstream retires and renames model ids without notice: the
// hy3 family was retired upstream on 2026-09-22 while the static catalog
// still advertised it, producing host-level "unknown provider" 400s that no
// plugin log could explain. Static catalogs are gone on purpose —
// advertisement mirrors discovery only, chat passes the client's model id
// through verbatim, and models_cn / models_intl / models_global config pins
// remain the explicit user override.

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

// buildModelInfos maps pinned upstream model IDs to ModelInfo with generic
// metadata — an unknown ID still gets advertised because the user pinned it
// deliberately. Display names come from upstream discovery, not a local
// table (v0.9.33 removed the static catalogs).
func buildModelInfos(ids []string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
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
//  2. per-realm dynamic discovery: fresh answer > today's cache > stale
//     cache (v0.12.18; stale-on-failure since v0.12.71);
//  3. nothing — deliberate since v0.9.33. A hand-maintained static catalog
//     rots silently: upstream retired the hy3 family (2026-09-22) while the
//     old table still advertised it, and the stale ids turned into
//     host-level "unknown provider" 400s no plugin log could explain.
//     Advertising only what upstream currently serves keeps the visible
//     list the truth; an empty list is honest, a wrong list is a trap.
//
// v0.9.9: every branch records its decision into the realm's diagnostics
// entry (source + count + failure reason) so the panel and logs answer
// "why does this realm show these models" without guesswork. Discovery
// failures log a throttled reason line (the pre-v0.9.9 path was silent).
func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	list := fetchDynamicModelsFromStorageInner(storageJSON)
	// v0.9.25: overlay runtime-learned alias→real display names on every
	// branch (pin / static / cache-hit / fresh discovery) — a mapping learned
	// mid-TTL must reach the host without waiting for cache expiry.
	return applyLearnedAliasNames(list)
}

func fetchDynamicModelsFromStorageInner(storageJSON []byte) []pluginapi.ModelInfo {
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
		// v0.9.33: no token → no discovery possible, and the static catalog
		// that used to fake a list here is gone. Advertise nothing.
		noteRealmSource(realm, "none (no token in storage)", 0)
		return nil
	}
	if models, ok := cachedDynamicModels(realm); ok {
		return models
	}
	dyn, err := discoverModelsFn(accessToken, realm, uid)
	if err != nil {
		noteRealmError(realm, err.Error())
		if stale, ok := cachedDynamicModelsStale(realm); ok {
			return stale
		}
		return nil
	}
	if len(dyn) == 0 {
		noteRealmError(realm, "discovery payload had no user-facing models")
		if stale, ok := cachedDynamicModelsStale(realm); ok {
			return stale
		}
		return nil
	}
	storeDynamicModels(realm, dyn)
	log.Printf("models: realm=%s discovery ok: %d model(s)", realm, len(dyn))
	return dyn
}

// realmModelsState is the dashboard-facing snapshot of one realm's model
// source — the answer to "why does this realm list these models".
// v0.9.25: Learned carries the alias→real model ids observed in chat
// response echoes, so the panel can show which concrete model backs each
// Intl tier alias.
type realmModelsState struct {
	Source     string            `json:"source"`
	Count      int               `json:"count"`
	FetchedAt  string            `json:"fetched_at,omitempty"`
	AgeSeconds int64             `json:"age_seconds,omitempty"`
	LastError  string            `json:"last_error,omitempty"`
	LastErrorA string            `json:"last_error_at,omitempty"`
	Learned    map[string]string `json:"learned,omitempty"`
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
// v0.12.71 (adapted from PR #6 / ab64e63): a transient failure must NOT wipe
// the last successful discovery. The host re-runs model.for_auth per auth
// over time; blanking models here makes different auths register different
// lists at different instants, which the host then turns into a shrunken
// candidate pool for routing. The stale list stays cached (see
// cachedDynamicModelsStale) until the next successful discovery replaces it.
func noteRealmError(realm, msg string) {
	now := time.Now()
	dynamicModelsCache.Lock()
	entry := dynamicModelsCache.realms[realm]
	entry.lastErr = msg
	entry.lastErrAt = now
	if len(entry.models) > 0 {
		// Keep the previous discovery answer as the served list.
		entry.source = "last discovery (transient failure)"
		entry.srcCount = len(entry.models)
	} else {
		entry.source = "none (discovery failed)"
		entry.srcCount = 0
	}
	shouldLog := entry.lastLogAt.IsZero() || now.Sub(entry.lastLogAt) >= time.Minute
	if shouldLog {
		entry.lastLogAt = now
	}
	dynamicModelsCache.realms[realm] = entry
	dynamicModelsCache.Unlock()
	if shouldLog {
		if len(entry.models) > 0 {
			log.Printf("models: realm=%s discovery failed (%s) — serving last successful discovery (%d model(s)) until next success", realm, msg, len(entry.models))
		} else {
			log.Printf("models: realm=%s discovery failed (%s) — nothing cached and static catalogs removed (v0.9.33): advertising nothing until next successful discovery", realm, msg)
		}
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
		Learned:   learnedRealSnapshot(),
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

// cachedDynamicModelsStale returns the realm's last successful discovery
// result regardless of TTL (v0.12.71, adapted from PR #6 / ab64e63): on a
// transient discovery failure the previously discovered list is still the
// best-known answer for that realm — better than the static catalog, which
// may be a subset or carry retired entries. Only the fresh-TTL path
// (cachedDynamicModels) short-circuits discovery; this one is consulted
// exclusively from the failure fallback.
func cachedDynamicModelsStale(realm string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.realms[realm]
	if !ok || len(entry.models) == 0 {
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

// callEnterpriseModelsAPI GETs /console/enterprises/personal/models and
// builds the ordered list (cli agent base + promoted entries).
func callEnterpriseModelsAPI(ctx context.Context, accessToken, realm string) ([]pluginapi.ModelInfo, error) {
	modelsURL, origin := modelsEndpointFor(realm)
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
// intlAliasDisplayNames maps the opaque tier aliases codebuddy.ai (Intl)
// returns from its discovery endpoints onto friendlier display names.
// The alias IS the routable upstream id — chat requests send it verbatim —
// but it is a product-tier label, not a model-family name, and upstream does
// not publish which real model backs each tier (the limited-free "deepseek
// flash" of 2026-09, if exposed on Intl, hides behind one of these).
// Field report 2026-09-20 (initial): fast-model / auto-chat /
// balanced-model / default-model all surface as bare ids; o4-mini is a
// genuine model id and stays untouched.
// Field report 2026-09-20 (v0.9.25 user panel): the Intl discovery now also
// returns primary-model / deep-model / enhance-1.0 — same opaque tier-alias
// family, same annotation treatment. Any id NOT in this map is assumed real
// and never annotated.
var intlAliasDisplayNames = map[string]string{
	"fast-model":     "Fast Model（上游别名）",
	"auto-chat":      "Auto Chat（上游别名）",
	"balanced-model": "Balanced Model（上游别名）",
	"default-model":  "Default Model（上游别名）",
	"primary-model":  "Primary Model（上游别名）",
	"deep-model":     "Deep Model（上游别名）",
	"enhance-1.0":    "Enhance 1.0（上游别名）",
}

// intlLearnedReal maps an Intl tier alias to the REAL upstream model id
// observed in chat-completion responses (the `model` echo field). The alias
// is what upstream routable-wise accepts, but the response echo names the
// concrete model that actually served the request — evidence no static table
// could ever have (v0.9.19 concluded "upstream does not publish which real
// model backs each tier"; learning it from live traffic closes that gap).
// Populated by noteLearnedRealModel from the executor's response paths.
var intlLearnedReal sync.Map // alias (lowercase) -> string real model id

// noteLearnedRealModel records alias→real evidence from one completed chat
// response. Guards: only tier aliases participate; a missing/self/alias
// echo carries no information. Safe for concurrent executor calls.
func noteLearnedRealModel(requestedModel, respModel string) {
	alias := strings.ToLower(strings.TrimSpace(requestedModel))
	real := strings.TrimSpace(respModel)
	if _, ok := intlAliasDisplayNames[alias]; !ok {
		return // not a tier alias — nothing to learn
	}
	if real == "" || strings.EqualFold(real, alias) {
		return // echo missing or self-referential
	}
	if _, ok := intlAliasDisplayNames[strings.ToLower(real)]; ok {
		return // alias echoing another alias — still no real id
	}
	if prev, ok := intlLearnedReal.Load(alias); ok && prev.(string) == real {
		return // already learned
	}
	intlLearnedReal.Store(alias, real)
	log.Printf("models: intl alias %q served by upstream model %q (learned from chat response echo)", alias, real)
}

// learnedRealModel returns the real model id learned for an alias, or "".
func learnedRealModel(alias string) string {
	if v, ok := intlLearnedReal.Load(strings.ToLower(strings.TrimSpace(alias))); ok {
		return v.(string)
	}
	return ""
}

// learnedRealSnapshot copies the learned alias→real map (diagnostics only).
func learnedRealSnapshot() map[string]string {
	out := map[string]string{}
	intlLearnedReal.Range(func(k, v any) bool {
		out[k.(string)] = v.(string)
		return true
	})
	return out
}

// applyLearnedAliasNames overlays runtime-learned real model ids onto a
// model list's display names: "Fast Model（上游别名）" becomes
// "Fast Model（上游别名·实测 glm-x）" once a response echo proved the backing
// model. Always copies — callers pass cache-owned slices. Applied to every
// list the plugin serves (pin/static/discovery/cache-hit) so a learned
// mapping survives the discovery cache TTL.
func applyLearnedAliasNames(in []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]pluginapi.ModelInfo, len(in))
	for i, m := range in {
		if real := learnedRealModel(m.ID); real != "" {
			base := m.Name
			if base == "" || base == m.ID {
				base = m.ID
			}
			if !strings.Contains(base, "实测 ") {
				m.Name = base + "·实测 " + real
			}
		}
		out[i] = m
	}
	return out
}

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
	// Annotate known Intl tier aliases only when upstream gave no richer
	// display name (Name==ID means the discovery row carried the bare id).
	if d, ok := intlAliasDisplayNames[strings.ToLower(info.ID)]; ok && (info.Name == "" || info.Name == info.ID) {
		info.Name = d
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
	// v0.9.33: static advertisement is deliberately EMPTY. Workbuddy model
	// ids are upstream's to define and retire — the hy3 family vanished
	// upstream on 2026-09-22 while static tables still advertised it, which
	// surfaced as host-level "unknown provider" 400s no plugin log could
	// explain. All advertisement flows through model.for_auth discovery;
	// models_cn / models_intl / models_global pins remain the explicit
	// user override.
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: []pluginapi.ModelInfo{}})
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
