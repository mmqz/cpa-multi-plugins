// models_persist.go — v0.12.63: persist each variant's last successful model
// discovery INTO the credential files themselves (top-level "model_cache"
// key), so the last-known-good catalog survives host restarts and upstream
// outages. Port of the workbuddy v0.9.38 design onto trae's cn/solo/intl
// variant namespaces.
//
// Why: modelsForVariant / intlhandleModelForAuth have exactly one fallback —
// the compiled-in static catalogs. Those are guesses frozen at release time;
// a discovery outage (or a restart before the first successful discovery)
// advertised them as if they were upstream truth. Persisting the snapshot
// means the failure ladder serves REAL upstream data (change-guarded) before
// touching the static guess.
//
// Snapshot semantics: the persisted list is the ADVERTISED catalog — ids
// already carry the variant suffix ("-solo"/"-intl") and the Intl virtuals
// (auto/work) — because exclusions and host routing both key on the
// advertised id. The list is PRE-exclusion, so excluded models stay visible
// in the picker and un-excluding never needs an upstream round-trip.
//
// Scope guard: snapshots are per-VARIANT and only ever served back to
// credentials of the SAME variant — a cn catalog must never satisfy a solo
// or intl credential (different namespaces, suffixed ids).
//
// Write hygiene: every host.auth.save re-fires the watcher → model.for_auth.
// persistModelSnapshot therefore writes ONLY when the discovered list
// actually changed (full ModelInfo compare), so the loop converges after one
// save per catalog change. Known gap (accepted): the scheduler's
// auth.Auth.SaveAtomic rebuilds the file in its minimal legacy shape and
// drops the snapshot; it re-stamps on the next successful discovery, and
// same-variant peers still serve theirs.
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
// rides along every save because the merge-based write paths
// (mergeAuthStorage, heal, adopt) preserve unknown top-level keys as raw JSON.
const modelCacheDocKey = "model_cache"

// preservedPluginDocKeys lists plugin-stamped top-level credential keys that
// must survive EVERY hostAuthSave funnel call — including the typed rebuilds
// (intlpersistRefreshedAuthTo, import) which construct a fresh document and
// would otherwise silently drop them. The whitelist (not a blanket merge) is
// deliberate: a rebuild must not smuggle arbitrary stale keys from a
// same-named older file.
// v0.12.68 (issue #25): beyond the plugin-stamped model_cache, the list now
// carries host-owned operator fields CPA writes into auth files via the auth
// manager (proxy_url / weight / priority / prefix / label / request_retry /
// headers) — every plugin-side rewrite used to reset them to defaults.
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
	// Realm is the variant namespace the catalog was discovered under
	// ("cn" | "solo" | "intl").
	Realm string `json:"realm"`
	// FetchedAt is the discovery time (RFC3339 UTC), used to pick the
	// freshest peer snapshot when the asking credential has none of its own.
	FetchedAt string `json:"fetched_at"`
	// Models is the advertised list (post-suffix, pre exclusion).
	Models []pluginapi.ModelInfo `json:"models"`
}

// Test seams — the real host RPC bridge is unavailable in unit tests.
var (
	hostAuthListFn = hostAuthList
	hostAuthRawFn  = hostAuthGetRaw
	hostAuthSaveFn = hostAuthSave
	intlSaveFn     = intlhostAuthSave
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
	if strings.TrimSpace(c.Realm) == "" || len(c.Models) == 0 {
		return nil
	}
	return c
}

// persistedSnapshotForStorage resolves the last persisted snapshot for the
// asking credential's variant: its own document first, then the freshest
// same-variant peer document. Cross-variant snapshots are never served.
func persistedSnapshotForStorage(storageJSON []byte, variant string) ([]pluginapi.ModelInfo, bool) {
	if snap := readModelCacheDoc(storageJSON); snap != nil && snap.Realm == variant {
		return snap.Models, true
	}
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return nil, false
	}
	var best *persistedModelCache
	for _, f := range files {
		phys, err := hostAuthRawFn(f.AuthIndex)
		if err != nil || len(phys) == 0 {
			continue
		}
		c := readModelCacheDoc(phys)
		if c == nil || c.Realm != variant {
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
func persistModelSnapshot(storageJSON []byte, variant string, models []pluginapi.ModelInfo) {
	name, doc, ok := locateCredentialFile(storageJSON)
	if !ok {
		log.Printf("models: variant=%s snapshot persist skipped — credential file not located", variant)
		return
	}
	if existing := readModelCacheDoc(doc); existing != nil && existing.Realm == variant && samePersistedModels(existing.Models, models) {
		// Unchanged — a save here would re-fire the watcher → model.for_auth
		// for zero information. This guard is what makes the write loop
		// converge.
		return
	}
	merged, err := mergeModelCacheIntoDoc(doc, &persistedModelCache{
		Realm:     variant,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Models:    models,
	})
	if err != nil {
		log.Printf("models: variant=%s snapshot merge failed: %v", variant, err)
		return
	}
	if err := hostAuthSaveFn(name, merged); err != nil {
		log.Printf("models: variant=%s snapshot persist failed: %v", variant, err)
		return
	}
	log.Printf("models: variant=%s snapshot persisted into %s (%d model(s))", variant, name, len(models))
}

// locateCredentialFile matches the asking credential to its file by access
// token — the only stable identifier that survives both storage shapes
// (nested trae-*.json and flat CPA-panel imports) and every naming scheme
// (trae-<uid>, trae-solo-cn-<uid>, trae-intl-<uid>).
func locateCredentialFile(storageJSON []byte) (name string, doc []byte, ok bool) {
	tok := extractTraeAccessToken(storageJSON)
	if strings.TrimSpace(tok) == "" {
		return "", nil, false
	}
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return "", nil, false
	}
	for _, f := range files {
		phys, err := hostAuthRawFn(f.AuthIndex)
		if err != nil || len(phys) == 0 {
			continue
		}
		if extractTraeAccessToken(phys) == tok {
			return f.Name, phys, true
		}
	}
	return "", nil, false
}

// extractTraeAccessToken reads the access token from a credential document,
// nested (auth.accessToken) or flat (accessToken).
func extractTraeAccessToken(raw []byte) string {
	var probe struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(raw, &probe)
	if strings.TrimSpace(probe.Auth.AccessToken) != "" {
		return probe.Auth.AccessToken
	}
	return probe.AccessToken
}

// mergeModelCacheIntoDoc stamps the snapshot into a credential document,
// preserving every other top-level key byte-for-byte (note, parity extras,
// panel-managed fields).
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
// incoming document is missing but the current physical file carries. Wired
// into the hostAuthSave / intlhostAuthSave funnels so typed rebuilds
// (intlpersistRefreshedAuthTo, import) keep the model_cache snapshot alive —
// without it, every intl token refresh would silently wipe the persisted
// catalog. Keys the caller explicitly set always win over the persisted copy.
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
		phys, err := hostAuthRawFn(f.AuthIndex)
		if err != nil {
			return nil
		}
		return phys
	}
	return nil
}
