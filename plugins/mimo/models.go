// models.go implements the ModelProvider capability: the official desktop
// core catalog (index.beauty.mjs:2045-2080 — the `uw` template plus the
// mimo-auto / mimo-flash / mimo-pro trio) and the host's oauth-excluded-models
// filter. The two `mimo-x-*` names carried by the community proxy do NOT
// exist in any official bundle (docs/MIMO_AUTH.md §4 item 10) and are
// deliberately absent here; the desktop's dynamic /user/available_models
// discovery (sk lane) can be layered on later without breaking this catalog.
package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Model ids exactly as the desktop defines them (index.beauty.mjs:1876-1878).
const (
	ModelMimoAuto  = "mimo-auto"
	ModelMimoFlash = "mimo-flash"
	ModelMimoPro   = "mimo-pro"
)

// mimoModelSpecs is the official core catalog. The desktop `uw` template
// declares: attachment/reasoning/tool_call/temperature true, modalities
// text+image → text, context 1e6, output 128e3 (index.beauty.mjs:2045-2066).
func mimoModelSpecs() []struct {
	id      string
	name    string
	context int64
	maxOut  int64
} {
	return []struct {
		id      string
		name    string
		context int64
		maxOut  int64
	}{
		{ModelMimoAuto, "MiMo Auto", 1000000, 128000},
		{ModelMimoFlash, "MiMo Flash", 1000000, 128000},
		{ModelMimoPro, "MiMo Pro", 1000000, 128000},
	}
}

func mimoModels() []pluginapi.ModelInfo {
	specs := mimoModelSpecs()
	out := make([]pluginapi.ModelInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, pluginapi.ModelInfo{
			ID:                         s.id,
			Name:                       s.name,
			ContextLength:              s.context,
			MaxCompletionTokens:        s.maxOut,
			OwnedBy:                    providerName,
			SupportedInputModalities:   []string{"text", "image"},
			SupportedOutputModalities:  []string{"text"},
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out
}

// resolveAutoModel mirrors the desktop's EE()/k6() pair (index.beauty.mjs
// 1884-1892): mimo-auto lands on mimo-pro, and a "<prefix>/mimo-auto" suffix
// form resolves onto "<prefix>/<resolved>". The whitelist WS() only admits
// mimo-flash/mimo-pro as explicit targets.
func resolveAutoModel(model string) string {
	const autoSuffix = "/" + ModelMimoAuto
	if model == ModelMimoAuto {
		return ModelMimoPro
	}
	if strings.HasSuffix(model, autoSuffix) {
		return model[:len(model)-len(autoSuffix)] + "/" + ModelMimoPro
	}
	return model
}

// isAllowedModel mirrors the desktop's lp() core check (2003-2005): mimo-auto
// itself, the whitelisted flash/pro pair, or any mimo-* id behind the
// permissive prefix switch. Unknown third-party ids fall through to the
// upstream — the desktop's prefix rule is a display convenience, not a wire
// gate, and hard-failing exotic ids would break forward compatibility.
func isAllowedModel(model string) bool {
	if model == ModelMimoAuto || model == ModelMimoFlash || model == ModelMimoPro {
		return true
	}
	return strings.HasPrefix(strings.ToLower(model), "mimo-")
}

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	models := filterExcludedModels(mimoModels(), req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key — the host skips any
	// response whose Provider doesn't match the auth's provider.
	models := filterExcludedModels(mimoModels(), req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// filterExcludedModels removes models listed in oauth-excluded-models for the
// mimo provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Fresh slice — models[:0] would alias the input's backing array (the
	// static catalog) and corrupt it for subsequent callers.
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}
