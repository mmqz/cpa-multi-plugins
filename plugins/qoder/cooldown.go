// cooldown.go implements per-(account, model) throttling for Qoder.
//
// An upstream failure on one model says nothing about the rest of the
// account's catalog. Cooling the whole credential would turn a single
// degraded model (for example an empty_stream on one upstream model) into an
// auth-wide outage — especially harmful when only one Qoder auth is
// configured. Throttling is therefore scoped to the failed (auth, model)
// pair.
//
// Unlike the fork this was adapted from (bfSan/qoder-cpa-plugin, where the
// table ended up observability-only after routing moved to the host), the
// table here actively shapes routing in two places:
//  1. the per-credential model catalog is filtered by modelIsCooling, so the
//     host's built-in scheduler stops offering the degraded pair, and
//  2. the opt-in scheduler_mode=credits picker skips cooling candidates.
//
// Marking a cooldown also drops the dynamic model catalog cache so the next
// host model query re-builds it without the cooled model immediately.
//
// State is process-local: it survives config reloads but not a CPA restart.
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// Default durations. Rate limits back off longer than protocol-level
	// failures because the upstream explicitly asked us to slow down.
	modelCooldownRateLimit = 300 * time.Second
	modelCooldownUnknown   = 60 * time.Second

	// modelCooldownMaxEntries bounds the table so a hostile or buggy upstream
	// cannot grow it without limit.
	modelCooldownMaxEntries = 4096
)

type cooldownReason string

const (
	cooldownReasonRateLimit cooldownReason = "rate_limit"
	cooldownReasonUnknown   cooldownReason = "upstream_error"
)

type modelCooldownKey struct {
	AuthID  string
	ModelID string
}

type modelCooldownEntry struct {
	Reason    cooldownReason
	Until     time.Time
	UpdatedAt time.Time
}

// cooldownAuthIDFor derives the cooldown key for a credential, mirroring the
// host auth ID convention in toAuthDataOpts (sanitized UID, else provider
// name). Executor requests carry the same identifier in AuthID, so keys
// recorded at failure time match keys queried by the catalog filter.
func cooldownAuthIDFor(sa *storedAuth) string {
	if id := sanitizeUIDForFileName(sa.Account.UID); id != "" {
		return id
	}
	return providerName
}

// requestModelForCooldown resolves the model key used for per-(auth, model)
// cooldown state.
//
// CPA may rewrite the client-facing model into an alias/upstream key between
// auth selection and execution, so the cooldown key is normalized through the
// same alias table the request path uses — keeping the recorded pair stable
// across rewrites. Metadata values recorded by the host's execution handlers
// (the routing model before rewrite) win over req.Model; req.Model stays the
// fallback for paths that do not populate metadata.
func requestModelForCooldown(reqModel string, metadata map[string]any) string {
	for _, key := range []string{"auth_selection_model", "requested_model"} {
		if model := metadataModelName(metadata, key); model != "" {
			return normalizeCooldownModel(model)
		}
	}
	return normalizeCooldownModel(reqModel)
}

func metadataModelName(metadata map[string]any, key string) string {
	if len(metadata) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// normalizeCooldownModel maps any CPA-facing spelling ("qoder/dfmodel",
// "deepseek-flash") onto the catalog key space the filter compares against.
func normalizeCooldownModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strings.TrimSpace(cpaToUpstreamKey(stripProviderPrefix(model)))
}

var (
	cooldownMu    sync.Mutex
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownNowFn = time.Now
)

// markModelCooldown records a throttled (account, model) pair. Empty halves
// are ignored: without a model ID the entry would freeze the account, and
// without an auth ID it would freeze the model everywhere.
func markModelCooldown(authID, model string, reason cooldownReason) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return
	}
	ttl := modelCooldownUnknown
	if reason == cooldownReasonRateLimit {
		ttl = modelCooldownRateLimit
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	cooldownSweepLocked(now)
	if len(cooldownTable) >= modelCooldownMaxEntries {
		// Table full: evict the entry that expires soonest — it is the one
		// closest to natural expiry anyway.
		var oldestKey modelCooldownKey
		oldestUntil := time.Time{}
		first := true
		for k, v := range cooldownTable {
			if first || v.Until.Before(oldestUntil) {
				oldestKey, oldestUntil, first = k, v.Until, false
			}
		}
		delete(cooldownTable, oldestKey)
	}
	cooldownTable[modelCooldownKey{AuthID: authID, ModelID: model}] = modelCooldownEntry{
		Reason:    reason,
		Until:     now.Add(ttl),
		UpdatedAt: now,
	}
	cooldownMu.Unlock()
	// The cooled model must not ride a stale catalog: drop the discovery
	// cache so the next per-auth model query rebuilds without it.
	dropDynamicModelsCache()
}

// modelCoolingUntil reports when the pair becomes usable again. The zero time
// means the pair is not cooling down.
func modelCoolingUntil(authID, model string) time.Time {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return time.Time{}
	}
	now := cooldownNowFn()
	key := modelCooldownKey{AuthID: authID, ModelID: model}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	entry, ok := cooldownTable[key]
	if !ok {
		return time.Time{}
	}
	if !entry.Until.After(now) {
		delete(cooldownTable, key)
		return time.Time{}
	}
	return entry.Until
}

// modelIsCooling reports whether the pair is cooling right now.
func modelIsCooling(authID, model string) bool {
	return !modelCoolingUntil(authID, model).IsZero()
}

// clearModelCooldown removes one pair, or every pair for the account when the
// model is empty. Returns how many entries were removed.
func clearModelCooldown(authID, model string) int {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" {
		return 0
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	removed := 0
	if model != "" {
		key := modelCooldownKey{AuthID: authID, ModelID: model}
		if _, ok := cooldownTable[key]; ok {
			delete(cooldownTable, key)
			removed++
		}
		return removed
	}
	for k := range cooldownTable {
		if k.AuthID == authID {
			delete(cooldownTable, k)
			removed++
		}
	}
	return removed
}

// cooldownSnapshotFor lists the still-active pairs for one account.
func cooldownSnapshotFor(authID string) []map[string]any {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if k.AuthID != authID || !v.Until.After(now) {
			continue
		}
		out = append(out, map[string]any{
			"model":      k.ModelID,
			"reason":     string(v.Reason),
			"until":      v.Until.UTC().Format(time.RFC3339),
			"seconds":    int(v.Until.Sub(now).Round(time.Second) / time.Second),
			"updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	cooldownMu.Unlock()
	sortCooldownSnapshot(out)
	return out
}

// cooldownSnapshotAll lists every active pair across accounts.
func cooldownSnapshotAll() []map[string]any {
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			continue
		}
		out = append(out, map[string]any{
			"auth_id":    k.AuthID,
			"model":      k.ModelID,
			"reason":     string(v.Reason),
			"until":      v.Until.UTC().Format(time.RFC3339),
			"seconds":    int(v.Until.Sub(now).Round(time.Second) / time.Second),
			"updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	cooldownMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["auth_id"].(string)
		b, _ := out[j]["auth_id"].(string)
		if a != b {
			return a < b
		}
		am, _ := out[i]["model"].(string)
		bm, _ := out[j]["model"].(string)
		return am < bm
	})
	return out
}

func sortCooldownSnapshot(rows []map[string]any) {
	sort.Slice(rows, func(i, j int) bool {
		a, _ := rows[i]["model"].(string)
		b, _ := rows[j]["model"].(string)
		return a < b
	})
}

// cooldownSweepLocked drops expired entries. Callers must hold cooldownMu.
func cooldownSweepLocked(now time.Time) {
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			delete(cooldownTable, k)
		}
	}
}

// recordUpstreamFailure routes an upstream failure into the model-scoped
// cooldown table.
//
// Qoder's worst failure mode is not an HTTP 429: the gateway can accept the
// request and then close the SSE stream before the first payload, which the
// plugin observes as empty_stream with status 0. Record that against the
// specific model so the catalog filter stops re-offering the degraded pair.
// Account-level failures (hard credit exhaustion, invalid credentials) stay
// with the existing lifecycle/status handling.
func recordUpstreamFailure(authID, model string, status int, body string) {
	model = normalizeCooldownModel(model)
	if model == "" {
		return
	}
	if isHardCreditError(status, body) {
		return
	}
	if status == 429 || isSoftRateLimit(status, body) {
		markModelCooldown(authID, model, cooldownReasonRateLimit)
		return
	}
	// Empty stream / transport-level failures may arrive with no HTTP status
	// (direct read failure) or ride a gateway status such as 503. The body is
	// the reliable discriminator, so do not gate this on status == 0.
	if isEmptyStreamFailure(body) {
		markModelCooldown(authID, model, cooldownReasonUnknown)
	}
}

func isEmptyStreamFailure(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(body, "empty_stream") ||
		strings.Contains(body, "stream closed before first payload")
}

// handleCooldownList reports every active pair, or one account's pairs when
// auth_id is given. State lives in memory, so the response says so.
func handleCooldownList(req pluginapi.ManagementRequest) map[string]any {
	if vals := req.Query["auth_id"]; len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
		entries := cooldownSnapshotFor(vals[0])
		return map[string]any{
			"entries":    entries,
			"count":      len(entries),
			"persistent": false,
		}
	}
	entries := cooldownSnapshotAll()
	return map[string]any{
		"entries":    entries,
		"count":      len(entries),
		"persistent": false,
	}
}

// handleCooldownClear clears one pair (auth_id + model) or the whole account
// (auth_id only). Panel action: "give this model another chance now".
func handleCooldownClear(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthID string `json:"auth_id"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		return map[string]any{"error": "auth_id required"}
	}
	removed := clearModelCooldown(authID, body.Model)
	if removed > 0 {
		// Let the next catalog rebuild re-offer the cleared model at once.
		dropDynamicModelsCache()
	}
	return map[string]any{"success": true, "removed": removed}
}
