package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// persistTestStorage builds a nested-shape credential document the same way
// the login/persist paths write it, with an optional raw extra top-level
// member (e.g. a persisted model_cache).
func persistTestStorage(token, variant, extraJSON string) []byte {
	doc := `{"type":"trae","provider":"trae","auth":{"accessToken":"` + token + `","variant":"` + variant + `"},"account":{"uid":"u1","nickname":"n1"}` + extraJSON + `}`
	return []byte(doc)
}

// stubPersistSeams swaps the host-RPC seams for in-memory fakes and returns
// a restore func. files maps AuthIndex → physical JSON.
func stubPersistSeams(t *testing.T, files map[string][]byte) func() {
	t.Helper()
	origList, origGet, origSave, origIntlSave := hostAuthListFn, hostAuthRawFn, hostAuthSaveFn, intlSaveFn
	capturedSaves = make([]string, 0, 8)
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		out := make([]pluginapi.HostAuthFileEntry, 0, len(files))
		for idx := range files {
			out = append(out, pluginapi.HostAuthFileEntry{AuthIndex: idx, Name: "trae-" + idx + ".json"})
		}
		return out, nil
	}
	hostAuthRawFn = func(authIndex string) (json.RawMessage, error) {
		raw, ok := files[authIndex]
		if !ok {
			return nil, errors.New("no such credential")
		}
		return raw, nil
	}
	hostAuthSaveFn = func(name string, raw []byte) error {
		capturedSaves = append(capturedSaves, name+"="+string(raw))
		for idx := range files {
			if "trae-"+idx+".json" == name {
				files[idx] = raw
				break
			}
		}
		return nil
	}
	intlSaveFn = hostAuthSaveFn
	return func() {
		hostAuthListFn, hostAuthRawFn, hostAuthSaveFn, intlSaveFn = origList, origGet, origSave, origIntlSave
	}
}

// stubCNDiscovery swaps the fetchModelsFn seam and returns a restore func.
func stubCNDiscovery(t *testing.T, fn func(a *auth.Auth) ([]upstream.ModelInfo, error)) func() {
	t.Helper()
	orig := fetchModelsFn
	fetchModelsFn = fn
	return func() { fetchModelsFn = orig }
}

// variantTestModels builds distinct advertised ModelInfos from the given ids.
func variantTestModels(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: "Name " + id, OwnedBy: providerName})
	}
	return out
}

// TestPersistedSnapshotServedOnDiscoveryFailure pins the restart case: the
// live fetch fails and the credential's own persisted model_cache is served
// instead of the compiled-in static guess — real upstream data beats guesses.
func TestPersistedSnapshotServedOnDiscoveryFailure(t *testing.T) {
	restoreSeams := stubPersistSeams(t, map[string][]byte{
		"a1": mustPersistCache(t, persistTestStorage("tok-cn", "cn", ""), "cn", "2026-09-27T00:00:00Z", variantTestModels("m-cn-1", "m-cn-2")),
	})
	defer restoreSeams()
	restore := stubCNDiscovery(t, func(a *auth.Auth) ([]upstream.ModelInfo, error) {
		return nil, errors.New("upstream down")
	})
	defer restore()

	a, err := parseStoredAuth(persistTestStorage("tok-cn", "cn", ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := modelsForVariant(a, persistTestStorage("tok-cn", "cn", ""))
	if len(got) != 2 || got[0].ID != "m-cn-1" || got[1].ID != "m-cn-2" {
		t.Fatalf("snapshot not served on discovery failure: %+v", got)
	}
}

// TestPersistedSnapshotSameVariantOnly proves the peer scan never crosses the
// variant namespace: a cn credential falls back to a cn peer's snapshot, and
// a solo credential (whose advertised ids carry the -solo suffix) only to a
// solo peer — a cross-variant snapshot would hand the host unroutable ids.
func TestPersistedSnapshotSameVariantOnly(t *testing.T) {
	restoreSeams := stubPersistSeams(t, map[string][]byte{
		"cn":   mustPersistCache(t, persistTestStorage("tok-peer-cn", "cn", ""), "cn", "2026-09-27T01:00:00Z", variantTestModels("m-cn-peer")),
		"solo": mustPersistCache(t, persistTestStorage("tok-peer-solo", "solo", ""), "solo", "2026-09-27T01:00:00Z", variantTestModels("m-solo-peer-solo")),
	})
	defer restoreSeams()
	restore := stubCNDiscovery(t, func(a *auth.Auth) ([]upstream.ModelInfo, error) {
		return nil, errors.New("down")
	})
	defer restore()

	aCN, err := parseStoredAuth(persistTestStorage("tok-ask-cn", "cn", ""))
	if err != nil {
		t.Fatalf("parse cn: %v", err)
	}
	if got := modelsForVariant(aCN, persistTestStorage("tok-ask-cn", "cn", "")); len(got) != 1 || got[0].ID != "m-cn-peer" {
		t.Fatalf("cn ask: want cn peer snapshot, got %+v", got)
	}
	aSolo, err := parseStoredAuth(persistTestStorage("tok-ask-solo", "solo", ""))
	if err != nil {
		t.Fatalf("parse solo: %v", err)
	}
	if got := modelsForVariant(aSolo, persistTestStorage("tok-ask-solo", "solo", "")); len(got) != 1 || got[0].ID != "m-solo-peer-solo" {
		t.Fatalf("solo ask: want solo peer snapshot, got %+v", got)
	}
}

// TestPersistModelSnapshotOnlyWritesOnChange pins the convergence guard:
// every host.auth.save re-fires the watcher → model.for_auth, so the
// snapshot write must happen exactly once per real catalog change, and the
// merge must preserve unknown top-level keys.
func TestPersistModelSnapshotOnlyWritesOnChange(t *testing.T) {
	selfDoc := []byte(`{"type":"trae","provider":"trae","auth":{"accessToken":"tok-self","variant":"cn"},"account":{"uid":"u1"},"note":"keep me"}`)
	restoreSeams := stubPersistSeams(t, map[string][]byte{"self": selfDoc})
	defer restoreSeams()

	ask := persistTestStorage("tok-self", "cn", "")
	models := variantTestModels("m1", "m2")

	persistModelSnapshot(ask, "cn", models)
	persistModelSnapshot(ask, "cn", models) // unchanged → no second save
	if n := len(saveCalls()); n != 1 {
		t.Fatalf("saves after duplicate persist = %d, want 1", n)
	}

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

	persistModelSnapshot(ask, "cn", variantTestModels("m1", "m2", "m3"))
	if n := len(saveCalls()); n != 2 {
		t.Fatalf("saves after catalog change = %d, want 2", n)
	}
}

// TestPreservePluginDocKeysRestoresModelCache pins the save-funnel guard:
// a typed rebuild (intlpersistRefreshedAuthTo / import) produces a document
// WITHOUT model_cache; the funnel must re-inject the persisted snapshot from
// the physical file, and a caller-supplied key must win.
func TestPreservePluginDocKeysRestoresModelCache(t *testing.T) {
	snap := &persistedModelCache{Realm: "intl", FetchedAt: "2026-09-27T00:00:00Z", Models: variantTestModels("m-keep")}
	physDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-live", "intl", ""), snap)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	restoreSeams := stubPersistSeams(t, map[string][]byte{"live": physDoc})
	defer restoreSeams()

	rebuilt := []byte(`{"type":"trae","provider":"trae","auth":{"accessToken":"tok-live","variant":"intl"},"account":{"uid":"u1"},"disabled":false}`)
	merged := preservePluginDocKeys("trae-live.json", rebuilt)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatalf("merged doc unreadable: %v", err)
	}
	if _, ok := m["model_cache"]; !ok {
		t.Fatalf("model_cache not restored by the save funnel")
	}

	// Fresh save (no physical file under that name) → nothing to restore.
	bare := preservePluginDocKeys("trae-brand-new.json", rebuilt)
	var b map[string]json.RawMessage
	if err := json.Unmarshal(bare, &b); err != nil {
		t.Fatalf("bare doc unreadable: %v", err)
	}
	if _, ok := b["model_cache"]; ok {
		t.Fatalf("model_cache must not appear without a physical source")
	}
}

// --- groups endpoint ---

// groupsTestRequest builds a ManagementRequest for the models/groups endpoint.
func groupsTestRequest(query url.Values) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/trae/models/groups", Query: query}
}

// groupsByVariant indexes the response groups by variant key.
func groupsByVariant(t *testing.T, payload any) map[string]modelGroup {
	t.Helper()
	resp, ok := payload.(modelGroupsResponse)
	if !ok {
		t.Fatalf("payload type %T is not modelGroupsResponse", payload)
	}
	out := map[string]modelGroup{}
	for _, g := range resp.Groups {
		out[g.Realm] = g
	}
	for _, v := range modelGroupVariantOrder {
		if _, ok := out[v]; !ok {
			t.Fatalf("group for variant %q missing", v)
		}
	}
	return out
}

// TestModelGroupsSnapshotAggregation pins the read-only path: per variant the
// NEWEST same-variant snapshot wins, variants without credentials render
// empty with needs_refresh, and the group carries credential counts.
func TestModelGroupsSnapshotAggregation(t *testing.T) {
	cnDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""),
		&persistedModelCache{Realm: "cn", FetchedAt: "2026-09-27T00:00:00Z", Models: variantTestModels("m-cn-a", "m-cn-b")})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	soloDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-solo", "solo", ""),
		&persistedModelCache{Realm: "solo", FetchedAt: "2026-09-26T00:00:00Z", Models: variantTestModels("m-solo-1-solo")})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	intlDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-intl", "intl", ""),
		&persistedModelCache{Realm: "intl", FetchedAt: "2026-09-25T00:00:00Z", Models: variantTestModels("auto", "gpt-5.2-intl")})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	restore := stubPersistSeams(t, map[string][]byte{"cn1": cnDoc, "so1": soloDoc, "in1": intlDoc})
	defer restore()

	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	groups := groupsByVariant(t, payload)

	gCN := groups["cn"]
	if gCN.Count != 2 || gCN.Source != "snapshot" || gCN.Credentials != 1 {
		t.Fatalf("cn group = source %q count %d creds %d", gCN.Source, gCN.Count, gCN.Credentials)
	}
	if gCN.Models[0].ID != "m-cn-a" {
		t.Fatalf("cn model = %q", gCN.Models[0].ID)
	}
	if groups["solo"].Count != 1 || groups["solo"].Models[0].ID != "m-solo-1-solo" {
		t.Fatalf("solo group wrong: %+v", groups["solo"].Models)
	}
	if groups["intl"].Count != 2 {
		t.Fatalf("intl group = %d, want 2 (suffix + virtuals ride the snapshot)", groups["intl"].Count)
	}
}

// TestModelGroupsRefresh pins ?refresh=1 for the cn lane: the live discovery
// answer takes precedence over the snapshot, and the snapshot is the
// fallback when the upstream is down — the exact for_auth contract.
func TestModelGroupsRefresh(t *testing.T) {
	cnDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""),
		&persistedModelCache{Realm: "cn", FetchedAt: "2026-09-01T00:00:00Z", Models: variantTestModels("m-stale")})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	restoreSeams := stubPersistSeams(t, map[string][]byte{"cn1": cnDoc})
	defer restoreSeams()

	// Fresh discovery wins.
	restore := stubCNDiscovery(t, func(a *auth.Auth) ([]upstream.ModelInfo, error) {
		if a.Variant != variantCN {
			return nil, errors.New("wrong variant asked: " + a.Variant)
		}
		return []upstream.ModelInfo{{ID: "m-fresh-1", Name: "Fresh One"}, {ID: "m-fresh-2", Name: "Fresh Two"}}, nil
	})
	defer restore()

	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup := groupsByVariant(t, payload)["cn"]
	if cnGroup.Source != "refreshed" || cnGroup.Count != 2 || cnGroup.Models[0].ID != "m-fresh-1" {
		t.Fatalf("refresh group = source %q count %d — fresh discovery should win", cnGroup.Source, cnGroup.Count)
	}

	// Upstream down → the chain falls back to the persisted snapshot instead
	// of returning nothing. The FIRST refresh already stamped its fresh
	// catalog into the credential file, so the fallback serves m-fresh-*.
	restore2 := stubCNDiscovery(t, func(a *auth.Auth) ([]upstream.ModelInfo, error) {
		return nil, errors.New("upstream down")
	})
	defer restore2()
	status, payload = handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup = groupsByVariant(t, payload)["cn"]
	if cnGroup.Count != 2 || cnGroup.Models[0].ID != "m-fresh-1" {
		t.Fatalf("refresh fallback should serve the freshly re-stamped snapshot, got %+v", cnGroup.Models)
	}
}

// TestModelGroupsNoCredentials pins the empty-store shape: every variant
// group renders, all empty and flagged for refresh, and the response carries
// the usage hint.
func TestModelGroupsNoCredentials(t *testing.T) {
	restore := stubPersistSeams(t, map[string][]byte{})
	defer restore()

	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	resp, ok := payload.(modelGroupsResponse)
	if !ok {
		t.Fatalf("payload type %T", payload)
	}
	if len(resp.Groups) != len(modelGroupVariantOrder) {
		t.Fatalf("groups = %d, want %d", len(resp.Groups), len(modelGroupVariantOrder))
	}
	for _, g := range resp.Groups {
		if g.Count != 0 || !g.NeedsRefresh || len(g.Models) != 0 {
			t.Fatalf("variant %q should be empty + needs_refresh", g.Realm)
		}
	}
	if resp.Hint == "" {
		t.Fatal("hint missing")
	}
}

// TestExtractTraeAccessToken covers both storage shapes the plugin writes
// and accepts.
func TestExtractTraeAccessToken(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"nested-tok"},"account":{}}`)
	flat := []byte(`{"accessToken":"flat-tok","uid":"u1"}`)
	if got := extractTraeAccessToken(nested); got != "nested-tok" {
		t.Fatalf("nested = %q", got)
	}
	if got := extractTraeAccessToken(flat); got != "flat-tok" {
		t.Fatalf("flat = %q", got)
	}
	if got := extractTraeAccessToken([]byte(`{}`)); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// --- helpers ---

func mustPersistCache(t *testing.T, doc []byte, variant, fetchedAt string, models []pluginapi.ModelInfo) []byte {
	t.Helper()
	out, err := mergeModelCacheIntoDoc(doc, &persistedModelCache{Realm: variant, FetchedAt: fetchedAt, Models: models})
	if err != nil {
		t.Fatalf("mergeModelCacheIntoDoc: %v", err)
	}
	return out
}

// capturedSaves collects every hostAuthSaveFn payload issued while a
// stubPersistSeams fake is installed (reset on each install).
var capturedSaves []string

// saveCalls returns the captured hostAuthSaveFn payloads from the most
// recent stubPersistSeams install.
func saveCalls() []string { return capturedSaves }
