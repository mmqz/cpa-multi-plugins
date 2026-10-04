// models_exclusion.go — v0.12.62: oauth-excluded-models consumption for the
// trae plugin, at BOTH granularities the management panel's global page can
// express: the bare provider key ("trae" — excludes across every variant
// namespace) and the variant sub-keys ("trae-cn" / "trae-solo" /
// "trae-intl" — one namespace each). Any key present in the host config
// shows up in the panel's provider dropdown, so per-channel entries become
// first-class citizens of the global exclude page. trae's namespaces are
// model-ID-suffixed (cn = bare ids, solo = "-solo", intl = "-intl" plus the
// bare virtual auto/work), so exclusion patterns always match the
// ADVERTISED id.
package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// filterExcludedModels removes the models listed in oauth-excluded-models
// for the provider key plus any variant sub-keys the caller scopes to the
// credential at hand. The host passes the whole config map via
// HostConfigSummary; the host ALSO filters the plugin's answer with the
// provider key (applyExcludedModels in tryRegisterPluginModelsForAuth), so
// the provider-key half is idempotent — the sub-key half is plugin-only.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary, subKeys ...string) []pluginapi.ModelInfo {
	keys := append([]string{providerName}, subKeys...)
	return applyExcludedSet(models, excludedModelsForKeys(host, keys...))
}

// excludedModelsForKeys resolves the UNION of the oauth-excluded-models
// lists for keys, each via exact match then a case-insensitive scan (the
// host lowercases keys before the plugin ever sees them, but hand-written
// YAML may drift). Deduplicated, lowercased, order-preserving.
func excludedModelsForKeys(host pluginapi.HostConfigSummary, keys ...string) []string {
	if len(host.ExcludedModels) == 0 || len(keys) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		list := host.ExcludedModels[key]
		if len(list) == 0 {
			for channel, l := range host.ExcludedModels {
				if strings.EqualFold(strings.TrimSpace(channel), key) {
					list = l
					break
				}
			}
		}
		for _, m := range list {
			id := strings.ToLower(strings.TrimSpace(m))
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// applyExcludedSet filters models by an exact-id exclusion set,
// case-insensitive on the model ID, into a fresh slice (never in place —
// callers may hand us static/catalog slices they reuse).
func applyExcludedSet(models []pluginapi.ModelInfo, excluded []string) []pluginapi.ModelInfo {
	if len(excluded) == 0 || len(models) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[m] = struct{}{}
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}
