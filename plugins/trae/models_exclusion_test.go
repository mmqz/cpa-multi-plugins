package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func traeExclusionModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "auto", Name: "auto", OwnedBy: providerName},
		{ID: "auto-solo", Name: "auto-solo", OwnedBy: providerName},
		{ID: "auto-intl", Name: "auto-intl", OwnedBy: providerName},
		{ID: "free", Name: "free", OwnedBy: providerName},
	}
}

func assertTraeModelIDs(t *testing.T, got []pluginapi.ModelInfo, wantIDs []string) {
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

// TestFilterExcludedVariantSubKeys pins the v0.12.62 granularity contract:
// the bare "trae" provider key excludes across every variant namespace,
// while the "trae-cn" / "trae-solo" / "trae-intl" sub-keys exclude one
// namespace each. Because trae namespaces are model-ID suffixes (cn bare,
// -solo, -intl), exclusion patterns always match the advertised id.
func TestFilterExcludedVariantSubKeys(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"trae":      {"free"},
			"trae-cn":   {"auto"},
			"trae-solo": {"auto-solo"},
			"trae-intl": {"auto-intl"},
		},
	}

	// Provider key alone (static path): every namespace loses "free".
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host),
		[]string{"auto", "auto-solo", "auto-intl"})

	// cn credential: provider key ∪ trae-cn.
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host, "trae-cn"),
		[]string{"auto-solo", "auto-intl"})

	// solo credential.
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host, "trae-solo"),
		[]string{"auto", "auto-intl"})

	// intl credential.
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host, "trae-intl"),
		[]string{"auto", "auto-solo"})

	// Hand-written key case drift is tolerated by the scan fallback.
	hostDrift := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"Trae":      {"free"},
			"Trae-INTL": {"auto-intl"},
		},
	}
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), hostDrift, "trae-intl"),
		[]string{"auto", "auto-solo"})
}

// TestFilterExcludedSubKeyIsolation proves the cn/solo/intl sub-keys never
// bleed into each other: excluding "auto" under trae-cn must not remove
// intl's virtual bare "auto" from an intl credential's catalog when only
// the intl sub-key is consulted, and vice versa.
func TestFilterExcludedSubKeyIsolation(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"trae-cn": {"auto"},
		},
	}
	// intl credential consults ONLY trae-intl (+ provider key): the cn
	// sub-key's "auto" entry must not touch it.
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host, "trae-intl"),
		[]string{"auto", "auto-solo", "auto-intl", "free"})
	// cn credential consults trae-cn: bare "auto" goes.
	assertTraeModelIDs(t, filterExcludedModels(traeExclusionModels(), host, "trae-cn"),
		[]string{"auto-solo", "auto-intl", "free"})
}
