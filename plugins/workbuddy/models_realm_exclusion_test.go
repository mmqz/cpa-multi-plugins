package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// realmExclusionHost builds a HostConfigSummary the way the host delivers
// oauth-excluded-models to a plugin: keys lowercased, one provider-wide
// entry plus per-realm sub-entries.
func realmExclusionHost() pluginapi.HostConfigSummary {
	return pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"workbuddy":        {"shared-1"},
			"workbuddy-cn":     {"cn-only", "shared-1"},
			"workbuddy-intl":   {"intl-only"},
			"workbuddy-global": nil,
		},
	}
}

func realmExclusionModels() []pluginapi.ModelInfo {
	return realmTestModels("shared-1", "cn-only", "intl-only", "free-1")
}

// TestFilterExcludedRealmSubKeys pins the v0.9.39 granularity contract of
// oauth-excluded-models for plugin channels: the bare provider key excludes
// across every realm, the "<provider>-<realm>" sub-key excludes one realm
// only, and the effective list is the deduplicated union of both.
func TestFilterExcludedRealmSubKeys(t *testing.T) {
	host := realmExclusionHost()

	// Provider key alone: applies to every realm.
	got := filterExcludedModels(realmExclusionModels(), host)
	want := []string{"cn-only", "intl-only", "free-1"}
	assertModelIDs(t, got, want)

	// Sub-key cn: provider ∪ workbuddy-cn, deduped.
	got = filterExcludedModelsForRealm(realmExclusionModels(), host, "cn")
	want = []string{"intl-only", "free-1"}
	assertModelIDs(t, got, want)

	// Sub-key intl: only intl-only joins the provider entry.
	got = filterExcludedModelsForRealm(realmExclusionModels(), host, "intl")
	want = []string{"cn-only", "free-1"}
	assertModelIDs(t, got, want)

	// Sub-key global: entry exists but is empty → provider key only.
	got = filterExcludedModelsForRealm(realmExclusionModels(), host, "global")
	want = []string{"cn-only", "intl-only", "free-1"}
	assertModelIDs(t, got, want)

	// No realm context (model.static): provider key only.
	got = filterExcludedModelsForRealm(realmExclusionModels(), host, "")
	want = []string{"cn-only", "intl-only", "free-1"}
	assertModelIDs(t, got, want)

	// Hand-written key case drift is tolerated by the scan fallback.
	hostCaseDrift := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"Workbuddy":    {"shared-1"},
			"Workbuddy-CN": {"cn-only"},
		},
	}
	got = filterExcludedModelsForRealm(realmExclusionModels(), hostCaseDrift, "cn")
	want = []string{"intl-only", "free-1"}
	assertModelIDs(t, got, want)
}

// TestFilterExcludedSubKeyNeverCrossesRealms proves a cn sub-key cannot
// touch a global-realm credential's catalog even when the model IDs
// overlap between realms.
func TestFilterExcludedSubKeyNeverCrossesRealms(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"workbuddy-cn": {"auto"},
		},
	}
	models := realmTestModels("auto")
	got := filterExcludedModelsForRealm(models, host, "global")
	if len(got) != 1 || got[0].ID != "auto" {
		t.Fatalf("cn sub-key leaked into global realm: %+v", got)
	}
	got = filterExcludedModelsForRealm(models, host, "cn")
	if len(got) != 0 {
		t.Fatalf("cn sub-key did not apply to cn realm: %+v", got)
	}
}

func assertModelIDs(t *testing.T, got []pluginapi.ModelInfo, wantIDs []string) {
	t.Helper()
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if len(ids) != len(wantIDs) {
		t.Fatalf("model ids = %v, want %v", ids, wantIDs)
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Fatalf("model ids = %v, want %v", ids, wantIDs)
		}
	}
}
