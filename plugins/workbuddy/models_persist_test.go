package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// persistTestStorage builds a nested-shape credential storage blob the same
// way the host passes it to model.for_auth, with an optional raw extra
// top-level member (e.g. a persisted model_cache).
func persistTestStorage(token, region, extraJSON string) []byte {
	doc := `{"auth":{"accessToken":"` + token + `","region":"` + region + `"},"account":{"uid":"u1","nickname":"n1"}` + extraJSON + `}`
	return []byte(doc)
}

// stubPersistSeams swaps the host-RPC seams for in-memory fakes and returns
// a restore func. files maps AuthIndex → physical JSON.
func stubPersistSeams(t *testing.T, files map[string][]byte) func() {
	t.Helper()
	origList, origGet, origSave := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthSaveJSONFn
	capturedSaves = make([]string, 0, 8)
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		out := make([]pluginapi.HostAuthFileEntry, 0, len(files))
		for idx := range files {
			out = append(out, pluginapi.HostAuthFileEntry{AuthIndex: idx, Name: "workbuddy-" + idx + ".json"})
		}
		return out, nil
	}
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		raw, ok := files[authIndex]
		if !ok {
			return nil, errors.New("no such credential")
		}
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "workbuddy-" + authIndex + ".json", JSON: raw}, nil
	}
	hostAuthSaveJSONFn = func(name string, raw []byte) error {
		capturedSaves = append(capturedSaves, name+"="+string(raw))
		// Mirror the real host.auth.save: the stored record is updated, so a
		// subsequent locateCredentialFile reads the SAVED doc (with the
		// model_cache present) — that's what production's change-guard relies
		// on to converge.
		for idx := range files {
			if "workbuddy-"+idx+".json" == name {
				files[idx] = raw
				break
			}
		}
		return nil
	}
	return func() {
		hostAuthListFn, hostAuthGetPhysicalFn, hostAuthSaveJSONFn = origList, origGet, origSave
	}
}

// TestPersistedSnapshotServedOnDiscoveryFailure pins the v0.9.38 restart
// case: the realm's in-memory cache is empty (fresh process), discovery
// fails, and the credential's own persisted model_cache is served instead
// of nothing — so the host keeps the credential's models registered and the
// panel's per-credential excluded-models editor stays non-empty.
func TestPersistedSnapshotServedOnDiscoveryFailure(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	snap := &persistedModelCache{Realm: "cn", FetchedAt: "2026-09-27T00:00:00Z", Models: realmTestModels("m-cn-1", "m-cn-2")}
	doc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""), snap)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	orig := discoverModelsFn
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("upstream models endpoint down")
	}
	defer func() { discoverModelsFn = orig }()
	restore := stubPersistSeams(t, map[string][]byte{"a1": doc})
	defer restore()

	got := fetchDynamicModelsFromStorage(doc)
	if len(got) != 2 || got[0].ID != "m-cn-1" || got[1].ID != "m-cn-2" {
		t.Fatalf("snapshot not served on discovery failure: %+v", got)
	}
	if st := realmModelStateFor("cn"); st == nil || st.Source != "persisted snapshot" {
		t.Fatalf("diagnostics source = %+v, want 'persisted snapshot'", st)
	}
}

// TestPersistedSnapshotPeerFallbackSameRealmOnly proves the peer scan never
// crosses the realm boundary: a CN credential may fall back to a CN peer's
// snapshot, never to an Intl/Global one, and vice versa (v0.12.18 boundary
// — different gateways advertise different model ids).
func TestPersistedSnapshotPeerFallbackSameRealmOnly(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	cnDoc := mustPersistCache(t, persistTestStorage("tok-peer-cn", "cn", ""), "cn", "2026-09-27T01:00:00Z", realmTestModels("m-cn-peer"))
	intlDoc := mustPersistCache(t, persistTestStorage("tok-peer-intl", "intl", ""), "intl", "2026-09-27T01:00:00Z", realmTestModels("m-intl-peer"))

	orig := discoverModelsFn
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("down")
	}
	defer func() { discoverModelsFn = orig }()
	restore := stubPersistSeams(t, map[string][]byte{"cn": cnDoc, "intl": intlDoc})
	defer restore()

	cnAsk := persistTestStorage("tok-ask-cn", "cn", "")
	gotCN := fetchDynamicModelsFromStorage(cnAsk)
	if len(gotCN) != 1 || gotCN[0].ID != "m-cn-peer" {
		t.Fatalf("cn ask: want cn peer snapshot, got %+v", gotCN)
	}
	intlAsk := persistTestStorage("tok-ask-intl", "intl", "")
	gotIntl := fetchDynamicModelsFromStorage(intlAsk)
	if len(gotIntl) != 1 || gotIntl[0].ID != "m-intl-peer" {
		t.Fatalf("intl ask: want intl peer snapshot, got %+v", gotIntl)
	}
}

// TestPersistedSnapshotFreshestPeerWins — several same-realm peers may hold
// snapshots from different discovery rounds; the freshest fetched_at wins.
func TestPersistedSnapshotFreshestPeerWins(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	oldDoc := mustPersistCache(t, persistTestStorage("tok-old", "cn", ""), "cn", "2026-09-26T00:00:00Z", realmTestModels("m-stale"))
	newDoc := mustPersistCache(t, persistTestStorage("tok-new", "cn", ""), "cn", "2026-09-27T09:00:00Z", realmTestModels("m-fresh"))

	orig := discoverModelsFn
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("down")
	}
	defer func() { discoverModelsFn = orig }()
	restore := stubPersistSeams(t, map[string][]byte{"old": oldDoc, "new": newDoc})
	defer restore()

	got := fetchDynamicModelsFromStorage(persistTestStorage("tok-ask", "cn", ""))
	if len(got) != 1 || got[0].ID != "m-fresh" {
		t.Fatalf("want freshest peer snapshot (m-fresh), got %+v", got)
	}
}

// TestPersistModelSnapshotOnlyWritesOnChange pins the convergence guard:
// every host.auth.save re-fires the watcher → model.for_auth, so the
// snapshot write must happen exactly once per real catalog change, and the
// merge must preserve unknown top-level keys (note, panel-managed fields).
func TestPersistModelSnapshotOnlyWritesOnChange(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	selfDoc := []byte(`{"auth":{"accessToken":"tok-self","region":"cn"},"account":{"uid":"u1"},"note":"keep me"}`)
	restore := stubPersistSeams(t, map[string][]byte{"self": selfDoc})
	defer restore()

	ask := persistTestStorage("tok-self", "cn", "")
	models := realmTestModels("m1", "m2")

	persistModelSnapshot(ask, "cn", models)
	persistModelSnapshot(ask, "cn", models) // unchanged → no second save
	if n := len(saveCalls()); n != 1 {
		t.Fatalf("saves after duplicate persist = %d, want 1", n)
	}

	// The single save must carry the snapshot AND the untouched note.
	var saved map[string]json.RawMessage
	parts := strings.SplitN(saveCalls()[0], "=", 2)
	if err := json.Unmarshal([]byte(parts[1]), &saved); err != nil {
		t.Fatalf("saved doc unreadable: %v", err)
	}
	if string(saved["note"]) != `"keep me"` {
		t.Fatalf("note not preserved: %s", saved["note"])
	}
	var cache persistedModelCache
	if err := json.Unmarshal(saved["model_cache"], &cache); err != nil {
		t.Fatalf("model_cache unreadable: %v", err)
	}
	if cache.Realm != "cn" || len(cache.Models) != 2 || cache.Models[0].ID != "m1" {
		t.Fatalf("persisted snapshot wrong: %+v", cache)
	}

	// A real catalog change (new model) is worth exactly one more save.
	persistModelSnapshot(ask, "cn", realmTestModels("m1", "m2", "m3"))
	if n := len(saveCalls()); n != 2 {
		t.Fatalf("saves after catalog change = %d, want 2", n)
	}
}

// TestPersistedSnapshotSurvivesSaveNormalization round-trips the snapshot
// through the real write-side normalizer: unknown top-level keys ride along
// as raw JSON, so model_cache must still parse and keep its models after a
// hostAuthSaveJSON-shaped save.
func TestPersistedSnapshotSurvivesSaveNormalization(t *testing.T) {
	doc := mustPersistCache(t, persistTestStorage("tok", "global", ""), "global", "2026-09-27T02:00:00Z", realmTestModels("m-g1"))
	normalized, err := normalizeWorkbuddyAuthDoc(doc)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	snap := readModelCacheDoc(raw)
	if snap == nil || snap.Realm != "global" || len(snap.Models) != 1 || snap.Models[0].ID != "m-g1" {
		t.Fatalf("snapshot lost through normalization: %+v", snap)
	}
}

// TestNoTokenStillAdvertisesNothing — v0.9.33 semantics hold: a credential
// with no token cannot chat, so it must not advertise models even when a
// persisted snapshot exists.
func TestNoTokenStillAdvertisesNothing(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	doc := mustPersistCache(t, []byte(`{"auth":{"region":"cn"},"account":{}}`), "cn", "2026-09-27T00:00:00Z", realmTestModels("m1"))
	if got := fetchDynamicModelsFromStorage(doc); len(got) != 0 {
		t.Fatalf("no-token credential advertised %d model(s), want 0", len(got))
	}
}

// --- helpers ---

func mustPersistCache(t *testing.T, doc []byte, realm, fetchedAt string, models []pluginapi.ModelInfo) []byte {
	t.Helper()
	out, err := mergeModelCacheIntoDoc(doc, &persistedModelCache{Realm: realm, FetchedAt: fetchedAt, Models: models})
	if err != nil {
		t.Fatalf("mergeModelCacheIntoDoc: %v", err)
	}
	return out
}

// capturedSaves collects every hostAuthSaveJSONFn payload issued while a
// stubPersistSeams fake is installed (reset on each install).
var capturedSaves []string

// saveCalls returns the captured hostAuthSaveJSONFn payloads from the most
// recent stubPersistSeams install.
func saveCalls() []string { return capturedSaves }

// TestPreservePluginDocKeysRestoresModelCache pins the v0.9.41 save-funnel
// guard: a typed buildAuthFileJSON rebuild (the lifecycle/notes path)
// produces a document WITHOUT model_cache; the funnel must re-inject the
// persisted snapshot from the physical file, and a caller-supplied key must
// win. Before this fix every credits-note churn wiped the v0.9.38 snapshot.
func TestPreservePluginDocKeysRestoresModelCache(t *testing.T) {
	snap := &persistedModelCache{Realm: "cn", FetchedAt: "2026-09-27T00:00:00Z", Models: realmTestModels("m-keep")}
	physDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-live", "cn", ""), snap)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	restoreSeams := stubPersistSeams(t, map[string][]byte{"live": physDoc})
	defer restoreSeams()

	// A typed rebuild: only the builder's keys, no model_cache.
	rebuilt := []byte(`{"type":"workbuddy","provider":"workbuddy","disabled":false,"note":"CN · test","auth":{"accessToken":"tok-live","region":"cn"},"account":{"uid":"u1"},"auth_kind":"oauth"}`)
	merged := preservePluginDocKeys("workbuddy-live.json", rebuilt)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatalf("merged doc unreadable: %v", err)
	}
	if _, ok := m["model_cache"]; !ok {
		t.Fatalf("model_cache not restored by the save funnel")
	}
	if string(m["note"]) != `"CN · test"` {
		t.Fatalf("rebuild's own note must win: %s", m["note"])
	}

	// Fresh save (no physical file under that name) → nothing to restore.
	bare := preservePluginDocKeys("workbuddy-brand-new.json", rebuilt)
	var b map[string]json.RawMessage
	if err := json.Unmarshal(bare, &b); err != nil {
		t.Fatalf("bare doc unreadable: %v", err)
	}
	if _, ok := b["model_cache"]; ok {
		t.Fatalf("model_cache must not appear without a physical source")
	}
}
