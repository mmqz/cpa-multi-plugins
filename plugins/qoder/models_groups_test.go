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
	return pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/qoder/models/groups", Query: query}
}

// groupsByRegion indexes the response groups by region key.
func groupsByRegion(t *testing.T, payload any) map[string]modelGroup {
	t.Helper()
	resp, ok := payload.(modelGroupsResponse)
	if !ok {
		t.Fatalf("payload type %T is not modelGroupsResponse", payload)
	}
	out := map[string]modelGroup{}
	for _, g := range resp.Groups {
		out[g.Realm] = g
	}
	for _, region := range modelGroupRegionOrder {
		if _, ok := out[region]; !ok {
			t.Fatalf("group for region %q missing", region)
		}
	}
	return out
}

// TestModelGroupsSnapshotAggregation pins the read-only path: per region the
// NEWEST same-region snapshot wins, regions without credentials render empty
// with needs_refresh, and the group carries credential counts.
func TestModelGroupsSnapshotAggregation(t *testing.T) {
	resetQoderModelCacheForTest()
	defer resetQoderModelCacheForTest()

	oldCN := &persistedModelCache{Region: "cn", FetchedAt: "2026-09-26T00:00:00Z", Models: regionTestModels("m-cn-old")}
	newCN := &persistedModelCache{Region: "cn", FetchedAt: "2026-09-27T00:00:00Z", Models: regionTestModels("m-cn-a", "m-cn-b")}
	in := &persistedModelCache{Region: "intl", FetchedAt: "2026-09-25T00:00:00Z", Models: regionTestModels("m-in-1")}

	cnOldDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn-old", "cn", ""), oldCN)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	cnNewDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn-new", "cn", ""), newCN)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	inDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-in", "intl", ""), in)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	restore := stubPersistSeams(t, map[string][]byte{"cn1": cnOldDoc, "cn2": cnNewDoc, "in1": inDoc})
	defer restore()

	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	groups := groupsByRegion(t, payload)

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

	gIN := groups["intl"]
	if gIN.Count != 1 || gIN.Models[0].ID != "m-in-1" {
		t.Fatalf("intl group wrong: %+v", gIN.Models)
	}
}

// TestModelGroupsRefresh pins ?refresh=1: the live discovery answer takes
// precedence over the snapshot and the snapshot is the fallback when the
// upstream is down (the for_auth chain serves persisted last-known-good).
func TestModelGroupsRefresh(t *testing.T) {
	resetQoderModelCacheForTest()
	defer resetQoderModelCacheForTest()

	snap := &persistedModelCache{Region: "cn", FetchedAt: "2026-09-01T00:00:00Z", Models: regionTestModels("m-stale")}
	cnDoc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""), snap)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	restore := stubDiscovery(t, func(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
		if authRegion(sa) != "cn" {
			return nil, errors.New("wrong region asked: " + authRegion(sa))
		}
		return regionTestModels("m-fresh-1", "m-fresh-2", "m-fresh-3"), nil
	})
	defer restore()
	restoreSeams := stubPersistSeams(t, map[string][]byte{"cn1": cnDoc})
	defer restoreSeams()

	// Fresh discovery wins.
	status, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup := groupsByRegion(t, payload)["cn"]
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
	// exactly the restart-outage resilience the snapshot buys.
	resetQoderModelCacheForTest()
	restore2 := stubDiscovery(t, func(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("upstream down")
	})
	defer restore2()
	status, payload = handleModelGroupsQuery(groupsTestRequest(url.Values{"refresh": []string{"1"}}))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cnGroup = groupsByRegion(t, payload)["cn"]
	if cnGroup.Count != 3 || cnGroup.Models[0].ID != "m-fresh-1" {
		t.Fatalf("refresh fallback should serve the freshly re-stamped snapshot, got %+v", cnGroup.Models)
	}
}

// TestModelGroupsCrossRegionIsolation pins the region boundary on the picker
// path: a model_cache stamped for another region must never satisfy a
// credential's group (the doc's snapshot region mismatches the credential's
// resolved region and is skipped).
func TestModelGroupsCrossRegionIsolation(t *testing.T) {
	resetQoderModelCacheForTest()
	defer resetQoderModelCacheForTest()

	// CN credential file carrying an INTL-stamped snapshot (defensive: the
	// writer can't produce this, but a hand-edited or migrated file could).
	foreign := &persistedModelCache{Region: "intl", FetchedAt: "2026-09-27T00:00:00Z", Models: regionTestModels("m-in-leak")}
	doc, err := mergeModelCacheIntoDoc(persistTestStorage("tok-cn", "cn", ""), foreign)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	restore := stubPersistSeams(t, map[string][]byte{"cn1": doc})
	defer restore()

	_, payload := handleModelGroupsQuery(groupsTestRequest(url.Values{}))
	groups := groupsByRegion(t, payload)

	if groups["cn"].Count != 0 || groups["intl"].Count != 0 {
		t.Fatalf("cross-region snapshot leaked: cn=%d intl=%d, want both 0", groups["cn"].Count, groups["intl"].Count)
	}
	if !groups["cn"].NeedsRefresh {
		t.Fatal("cn with no valid same-region snapshot should need refresh")
	}
}

// TestModelGroupsNoCredentials pins the empty-store shape: every region group
// renders, all empty and flagged for refresh, and the response carries the
// usage hint.
func TestModelGroupsNoCredentials(t *testing.T) {
	resetQoderModelCacheForTest()
	defer resetQoderModelCacheForTest()

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
	if len(resp.Groups) != len(modelGroupRegionOrder) {
		t.Fatalf("groups = %d, want %d", len(resp.Groups), len(modelGroupRegionOrder))
	}
	for _, g := range resp.Groups {
		if g.Count != 0 || !g.NeedsRefresh || len(g.Models) != 0 {
			t.Fatalf("region %q should be empty + needs_refresh", g.Realm)
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
