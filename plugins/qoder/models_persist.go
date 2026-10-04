// models_persist.go — v0.8.27: persist each region's last successful model
// discovery INTO the credential files themselves (top-level "model_cache"
// key), so the last-known-good catalog survives host restarts and upstream
// outages. Port of the workbuddy v0.9.38 design, adapted to qoder's
// per-credential discovery chain (region.go cn/intl boundary).
//
// Why: fetchDynamicModelsFromStorage's only fallbacks are the single-slot
// in-memory dynamicModelsCache (dies with the process) and the compiled-in
// wbModels() static list (a guess, not a catalog). After a restart — or
// before the first successful discovery in a process lifetime — a discovery
// failure left the credential advertising the static guess while the panel's
// model-exclusion picker had nothing real to show. Persisting the snapshot
// closes that gap: on the next model.for_auth the plugin serves the persisted
// catalog (real upstream data, change-guarded), and the picker has a catalog
// without any upstream round-trip.
//
// Scope guard: snapshots are per-REGION and only ever served back to
// credentials of the SAME region (cn/intl — different gateways advertise
// different model ids; same boundary the v0.12.76 region-scoped fallback
// enforces for the in-memory path).
//
// Write hygiene: every host.auth.save re-fires the watcher → AuthUpdate →
// model.for_auth. persistModelSnapshot therefore writes ONLY when the
// discovered list actually changed (full ModelInfo compare), so the loop
// converges after one save per catalog change.
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelCacheDocKey is the top-level key stamped into credential files. It
// rides along every save because normalizeQoderAuthDoc merges unknown
// top-level keys as raw JSON (never re-marshaled through a typed struct).
const modelCacheDocKey = "model_cache"

// preservedPluginDocKeys lists top-level credential keys that must survive
// EVERY save funnel — including the typed buildAuthFileJSON rebuilds used by
// the lifecycle/notes paths, which construct a fresh document from a struct
// and would otherwise silently drop them. The whitelist (not a blanket
// merge) is deliberate: a rebuild that renames or re-imports a credential
// must not smuggle arbitrary stale keys from a same-named older file.
//
// v0.8.29 (issue #25): beyond the plugin-stamped model_cache, the list now
// carries the host-owned operator fields CPA writes into auth files via the
// auth manager (proxy_url / weight / priority / prefix / label / request_retry
// / headers). Every plugin-side note churn used to reset them to defaults.
var preservedPluginDocKeys = []string{
	modelCacheDocKey,
	"proxy_url",
	"weight",
	"priority",
	"prefix",
	"label",
	"request_retry",
	"headers",
}

// persistedModelCache is the on-disk snapshot shape.
type persistedModelCache struct {
	// Region the catalog was discovered under ("cn" | "intl").
	Region string `json:"realm"`
	// FetchedAt is the discovery time (RFC3339 UTC), used to pick the
	// freshest peer snapshot when the asking credential has none of its own.
	FetchedAt string `json:"fetched_at"`
	// Models is the raw discovered list (pre cooldown filter and pre
	// exclusion — both re-apply on every serve branch).
	Models []pluginapi.ModelInfo `json:"models"`
}

// Test seams — the real host RPC bridge is unavailable in unit tests.
// hostAuthGetPhysicalFn lives in authfile.go; the other two are declared here.
var (
	hostAuthListFn     = hostAuthList
	hostAuthSaveJSONFn = hostAuthSaveJSON
)

// readModelCacheDoc extracts the model_cache snapshot from one credential
// document. Returns nil when absent, malformed, or empty — a snapshot with
// no models is treated as "never persisted" so it can never shadow a peer's
// real catalog.
func readModelCacheDoc(doc []byte) *persistedModelCache {
	if len(doc) == 0 {
		return nil
	}
	var probe struct {
		Cache *persistedModelCache `json:"model_cache"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil || probe.Cache == nil {
		return nil
	}
	c := probe.Cache
	if strings.TrimSpace(c.Region) == "" || len(c.Models) == 0 {
		return nil
	}
	return c
}

// persistedSnapshotForStorage resolves the last persisted snapshot for the
// asking credential's region: its own document first, then the freshest
// same-region peer document. Cross-region snapshots are never served.
func persistedSnapshotForStorage(storageJSON []byte, region string) ([]pluginapi.ModelInfo, bool) {
	if snap := readModelCacheDoc(storageJSON); snap != nil && snap.Region == region {
		return snap.Models, true
	}
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return nil, false
	}
	var best *persistedModelCache
	for _, f := range files {
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		c := readModelCacheDoc(phys.JSON)
		if c == nil || c.Region != region {
			continue
		}
		if best == nil || persistedFetchedAt(c) > persistedFetchedAt(best) {
			best = c
		}
	}
	if best == nil {
		return nil, false
	}
	return best.Models, true
}

// persistedFetchedAt renders the snapshot's discovery time for comparison;
// unparsable timestamps sort oldest ("").
func persistedFetchedAt(c *persistedModelCache) string {
	if c == nil {
		return ""
	}
	return c.FetchedAt
}

// persistModelSnapshot locates the asking credential's file and merges the
// fresh snapshot into it. Best-effort: failures are logged, never propagated
// — a persist miss only means the next restart re-discovers from scratch.
func persistModelSnapshot(storageJSON []byte, region string, models []pluginapi.ModelInfo) {
	name, doc, ok := locateCredentialFile(storageJSON)
	if !ok {
		log.Printf("models: region=%s snapshot persist skipped — credential file not located", region)
		return
	}
	if existing := readModelCacheDoc(doc); existing != nil && existing.Region == region && samePersistedModels(existing.Models, models) {
		// Unchanged — a save here would re-fire the watcher → model.for_auth
		// for zero information. This guard is what makes the write loop
		// converge.
		return
	}
	merged, err := mergeModelCacheIntoDoc(doc, &persistedModelCache{
		Region:    region,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Models:    models,
	})
	if err != nil {
		log.Printf("models: region=%s snapshot merge failed: %v", region, err)
		return
	}
	if err := hostAuthSaveJSONFn(name, merged); err != nil {
		log.Printf("models: region=%s snapshot persist failed: %v", region, err)
		return
	}
	log.Printf("models: region=%s snapshot persisted into %s (%d model(s))", region, name, len(models))
}

// locateCredentialFile matches the asking credential to its file by access
// token — the only stable identifier that survives both storage shapes
// (nested plugin OAuth docs and legacy flat imports) and naming schemes
// (canonical qoder-<region>-<uid> files and legacy qoder-cn- names).
func locateCredentialFile(storageJSON []byte) (name string, doc []byte, ok bool) {
	ask, err := parseStored(storageJSON)
	if err != nil || ask == nil || strings.TrimSpace(ask.Auth.AccessToken) == "" {
		return "", nil, false
	}
	tok := ask.Auth.AccessToken
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return "", nil, false
	}
	for _, f := range files {
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		if peer, err := parseStored(phys.JSON); err == nil && peer != nil && peer.Auth.AccessToken == tok {
			return f.Name, phys.JSON, true
		}
	}
	return "", nil, false
}

// mergeModelCacheIntoDoc stamps the snapshot into a credential document,
// preserving every other top-level key byte-for-byte (note, proxy_url,
// panel-managed fields — same ownership model as normalizeQoderAuthDoc).
func mergeModelCacheIntoDoc(doc []byte, cache *persistedModelCache) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return nil, err
	}
	m[modelCacheDocKey] = raw
	return json.Marshal(m)
}

// samePersistedModels compares two snapshots field-completely (not just by
// model id): a renamed display name or changed context limit is a real
// catalog change worth one save.
func samePersistedModels(a, b []pluginapi.ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// preservePluginDocKeys re-injects whitelisted plugin-stamped keys that the
// incoming document is missing but the current physical file carries. Called
// from the hostAuthSaveJSON funnel so typed buildAuthFileJSON rebuilds
// (lifecycle notes, adopt, import) keep the model_cache snapshot alive —
// without it, every credits-note churn would silently wipe the persisted
// catalog and the next discovery outage would advertise the static guess.
// Keys the caller explicitly set always win over the persisted copy.
func preservePluginDocKeys(name string, doc []byte) []byte {
	if len(doc) == 0 || name == "" {
		return doc
	}
	var have map[string]json.RawMessage
	if err := json.Unmarshal(doc, &have); err != nil {
		return doc
	}
	needed := make([]string, 0, len(preservedPluginDocKeys))
	for _, k := range preservedPluginDocKeys {
		if _, ok := have[k]; !ok {
			needed = append(needed, k)
		}
	}
	if len(needed) == 0 {
		return doc
	}
	physical := physicalDocByName(name)
	if len(physical) == 0 {
		return doc
	}
	var prev map[string]json.RawMessage
	if err := json.Unmarshal(physical, &prev); err != nil {
		return doc
	}
	restored := false
	for _, k := range needed {
		raw, ok := prev[k]
		if !ok || len(raw) == 0 {
			continue
		}
		have[k] = raw
		restored = true
	}
	if !restored {
		return doc
	}
	merged, err := json.Marshal(have)
	if err != nil {
		return doc
	}
	return merged
}

// physicalDocByName resolves one credential document by exact file name
// (best-effort; empty when the name is unknown — e.g. a first save).
func physicalDocByName(name string) []byte {
	files, err := hostAuthListFn()
	if err != nil {
		return nil
	}
	for _, f := range files {
		if !strings.EqualFold(strings.TrimSpace(f.Name), name) {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil {
			return nil
		}
		return phys.JSON
	}
	return nil
}
