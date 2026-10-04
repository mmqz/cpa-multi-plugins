package main

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// groupsTestRequest builds a ManagementRequest for the models/groups endpoint.
func groupsTestRequest(query url.Values) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/workbuddy/models/groups", Query: query}
}

// groupsByRealm indexes the response groups by realm key.
func groupsByRealm(t *testing.T, payload any) map[string]modelGroup {
	t.Helper()
	resp, ok := payload.(modelGroupsResponse)
	if !ok {
		t.Fatalf("payload type %T is not modelGroupsResponse", payload)
	}
	out := map[string]modelGroup{}
	for _, g := range resp.Groups {
		out[g.Realm] = g
	}
	for _, realm := range modelGroupRealmOrder {
		if _, ok := out[realm]; !ok {
			t.Fatalf("group for realm %q missing", realm)
		}
	}
	return out
}

// TestModelGroupsSnapshotAggregation pins the read-only path: per realm the
// NEWEST same-realm snapshot wins, realms without credentials render empty
// with needs_refresh, and the group carries credential counts.
func TestModelGroupsSnapshotAggregation(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	oldCN := &persistedModelCache{Realm: "cn", FetchedAt: "2026-09-26T00:00:00Z", Models: realmTestModels("m-cn-old")}
	newCN := &persistedModelCache{Realm: "cn", FetchedAt: "2026-09-27T00:00:00Z", Models: realmTestModels("m-cn-a", "m-cn-b")}
	gl := &persistedModelCache{Realm: "global", FetchedAt: "2026-09-25T00:00:00Z", Models: realmTestModels("m-gl-1")}

	cnOldDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn-old", "cn", ""), oldCN)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	cnNewDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn-new", "cn", ""), newCN)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	glDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-gl", "global", ""), gl)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	restore := stubPersistSeams(t, map[string][]byte{"cn1": cnOldDoc, "cn2": cnNewDoc, "gl1": glDoc})
	defer restore()

	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	groups := groupsByRealm(t, payload)

	gCN := groups["cn"]
	if gCN.Count != 2 || len(gCN.Models) != 2 {
		t.Fatalf("cn group = %d models, want 2 (newest snapshot wins)", gCN.Count)
	}
	if gCN.Models[0].ID != "m-cn-a" || gCN.Models[1].ID != "m-cn-b" {
		t.Fatalf("cn models = %v,%v — stale snapshot leaked", gCN.Models[0].ID, gCN.Models[1].ID)
	}
	if gCN.Source != "snapshot" || gCN.FetchedAt != "2026-09-27T00:00:00Z" {
		t.Fatalf("cn source/fetched_at = %q/%q, want snapshot/2026-09-27", gCN.Source, gCN.FetchedAt)
	}
	if gCN.NeedsRefresh {
		t.Fatal("cn should not need refresh")
	}
	if gCN.Credentials != 2 {
		t.Fatalf("cn credentials = %d, want 2", gCN.Credentials)
	}

	gGL := groups["global"]
	if gGL.Count != 1 || gGL.Models[0].ID != "m-gl-1" {
		t.Fatalf("global group wrong: %+v", gGL.Models)
	}

	gIN := groups["intl"]
	if gIN.Count != 0 || !gIN.NeedsRefresh {
		t.Fatalf("intl should be empty + needs_refresh, got count=%d needs=%v", gIN.Count, gIN.NeedsRefresh)
	}
	if !strings.Contains(gIN.Note, "刷新") {
		t.Fatalf("intl note should hint refresh, got %q", gIN.Note)
	}
}

// TestModelGroupsRefresh pins ?refresh=1: the live discovery answer takes
// precedence over the snapshot and the snapshot is the fallback when the
// upstream is down (the for_auth chain serves persisted last-known-good).
func TestModelGroupsRefresh(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	snap := &persistedModelCache{Realm: "cn", FetchedAt: "2026-09-01T00:00:00Z", Models: realmTestModels("m-stale")}
	cnDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""), snap)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	orig := discoverModelsFn
	defer func() { discoverModelsFn = orig }()
	restore := stubPersistSeams(t, map[string][]byte{"cn1": cnDoc})
	defer restore()

	// Fresh discovery wins.
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		if realm != "cn" {
			return nil, errors.New("wrong realm asked: " + realm)
		}
		return realmTestModels("m-fresh-1", "m-fresh-2", "m-fresh-3"), nil
	}
	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup := groupsByRealm(t, payload)["cn"]
	if cnGroup.Source != "refreshed" || cnGroup.Count != 3 || cnGroup.Models[0].ID != "m-fresh-1" {
		t.Fatalf("refresh group = source %q count %d — fresh discovery should win", cnGroup.Source, cnGroup.Count)
	}
	if cnGroup.FetchedAt == "" || strings.HasPrefix(cnGroup.FetchedAt, "2026-09-01") {
		t.Fatalf("refreshed fetched_at should be now, got %q", cnGroup.FetchedAt)
	}

	// Upstream down → the chain falls back to the persisted snapshot instead
	// of returning nothing (same contract as model.for_auth). Note the
	// FIRST refresh already stamped its fresh catalog into the credential
	// file, so the fallback serves m-fresh-*, not the pre-refresh snapshot —
	// exactly the restart-outage resilience v0.9.38 bought.
	resetDynamicModelsCache()
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("upstream down")
	}
	status, payload = handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup = groupsByRealm(t, payload)["cn"]
	if cnGroup.Count != 3 || cnGroup.Models[0].ID != "m-fresh-1" {
		t.Fatalf("refresh fallback should serve the freshly re-stamped snapshot, got %+v", cnGroup.Models)
	}
}

// TestModelGroupsCrossRealmIsolation pins the v0.12.18 realm boundary on the
// picker path: a model_cache stamped for another realm must never satisfy a
// credential's group (the doc's snapshot realm mismatches the credential's
// resolved realm and is skipped).
func TestModelGroupsCrossRealmIsolation(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	// CN credential file carrying a GLOBAL-stamped snapshot (defensive: the
	// v0.9.38 writer can't produce this, but a hand-edited or migrated file
	// could).
	foreign := &persistedModelCache{Realm: "global", FetchedAt: "2026-09-27T00:00:00Z", Models: realmTestModels("m-gl-leak")}
	doc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""), foreign)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	restore := stubPersistSeams(t, map[string][]byte{"cn1": doc})
	defer restore()

	_, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	groups := groupsByRealm(t, payload)

	if groups["cn"].Count != 0 || groups["global"].Count != 0 {
		t.Fatalf("cross-realm snapshot leaked: cn=%d global=%d, want both 0", groups["cn"].Count, groups["global"].Count)
	}
	if !groups["cn"].NeedsRefresh {
		t.Fatal("cn with no valid same-realm snapshot should need refresh")
	}
}

// TestModelGroupsNoCredentials pins the empty-store shape: every realm group
// renders, all empty and flagged for refresh, and the response carries the
// usage hint.
func TestModelGroupsNoCredentials(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

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
	if len(resp.Groups) != len(modelGroupRealmOrder) {
		t.Fatalf("groups = %d, want %d", len(resp.Groups), len(modelGroupRealmOrder))
	}
	for _, g := range resp.Groups {
		if g.Count != 0 || !g.NeedsRefresh || len(g.Models) != 0 {
			t.Fatalf("realm %q should be empty + needs_refresh", g.Realm)
		}
	}
	if resp.Hint == "" {
		t.Fatal("hint missing")
	}
}

// TestModelGroupEntriesDisplayNames pins the picker row projection: display
// name shown when distinct, dropped when equal to the id, empty ids skipped.
func TestModelGroupEntriesDisplayNames(t *testing.T) {
	models := []pluginapi.ModelInfo{
		{ID: "m-1", DisplayName: "Model One"},
		{ID: "m-2", Name: "m-2"},        // name == id → dropped
		{ID: "m-3", DisplayName: "m-3"}, // display name equal to id -> dropped (no duplicate label)
		{ID: "", DisplayName: "ghost"},  // no id → skipped
	}
	rows := modelGroupEntries(models)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (empty id skipped)", len(rows))
	}
	if rows[0].Name != "Model One" {
		t.Fatalf("row0 name = %q", rows[0].Name)
	}
	if rows[1].Name != "" {
		t.Fatalf("row2 name should be empty when equal to id, got %q", rows[1].Name)
	}
	if rows[2].Name != "" {
		t.Fatalf("row3 name should be dropped when equal to id, got %q", rows[2].Name)
	}
}
