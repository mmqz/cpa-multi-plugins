package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// regionExclusionModels builds the advertised catalog shape for filter tests.
func regionExclusionModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "shared-1", Name: "shared-1", OwnedBy: providerName},
		{ID: "cn-only", Name: "cn-only", OwnedBy: providerName},
		{ID: "intl-only", Name: "intl-only", OwnedBy: providerName},
		{ID: "free-1", Name: "free-1", OwnedBy: providerName},
	}
}

func assertQoderModelIDs(t *testing.T, got []pluginapi.ModelInfo, wantIDs []string) {
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

// TestFilterExcludedRegionSubKeys pins the v0.8.26 granularity contract of
// oauth-excluded-models for the qoder plugin: the bare provider key excludes
// across both regions, the "qoder-cn" / "qoder-intl" sub-keys exclude one
// region each, and the effective list is the deduplicated union of the
// provider key and the credential's region sub-key.
func TestFilterExcludedRegionSubKeys(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"qoder":      {"shared-1"},
			"qoder-cn":   {"cn-only", "shared-1"},
			"qoder-intl": {"intl-only"},
		},
	}

	// Provider key alone: applies to every region.
	assertQoderModelIDs(t, filterExcludedModels(regionExclusionModels(), host),
		[]string{"cn-only", "intl-only", "free-1"})

	// Sub-key cn: provider ∪ qoder-cn, deduped.
	assertQoderModelIDs(t, filterExcludedModelsForRegion(regionExclusionModels(), host, "cn"),
		[]string{"intl-only", "free-1"})

	// Sub-key intl.
	assertQoderModelIDs(t, filterExcludedModelsForRegion(regionExclusionModels(), host, "intl"),
		[]string{"cn-only", "free-1"})

	// No region context (model.static): provider key only.
	assertQoderModelIDs(t, filterExcludedModelsForRegion(regionExclusionModels(), host, ""),
		[]string{"cn-only", "intl-only", "free-1"})

	// Hand-written key case drift is tolerated by the scan fallback.
	hostDrift := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"Qoder":    {"shared-1"},
			"Qoder-CN": {"cn-only"},
		},
	}
	assertQoderModelIDs(t, filterExcludedModelsForRegion(regionExclusionModels(), hostDrift, "cn"),
		[]string{"intl-only", "free-1"})
}

// TestFilterExcludedSubKeyNeverCrossesRegions proves a cn sub-key cannot
// touch an intl credential's catalog even when model IDs overlap.
func TestFilterExcludedSubKeyNeverCrossesRegions(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"qoder-cn": {"auto"},
		},
	}
	models := []pluginapi.ModelInfo{{ID: "auto", Name: "auto", OwnedBy: providerName}}
	if got := filterExcludedModelsForRegion(models, host, "intl"); len(got) != 1 {
		t.Fatalf("cn sub-key leaked into intl region: %+v", got)
	}
	if got := filterExcludedModelsForRegion(models, host, "cn"); len(got) != 0 {
		t.Fatalf("cn sub-key did not apply to cn region: %+v", got)
	}
}
