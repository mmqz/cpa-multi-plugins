// models.go implements the ModelProvider capability: the pinned GLM coding
// plan catalog (TriDefender/zcode-api src/provider/models.ts — hardcoded to
// the exact models available on the Z.AI / Bigmodel coding-plan tier), the
// oauth-excluded-models filter, and the per-(auth, model) cooling filter.
package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// zcodeModels is the pinned GLM catalog (specs synced to the ZCode 3.11.2
// catalog by the reference project; glm-5.2 kept for forwarding
// compatibility, glm-5.3-flash serves claimed trial plans).
func zcodeModels() []pluginapi.ModelInfo {
	specs := []struct {
		id      string
		name    string
		context int64
		maxOut  int64
		reason  bool
	}{
		{"glm-4.5-air", "GLM 4.5 Air", 131072, 98304, true},
		{"glm-4.6", "GLM 4.6", 200000, 131072, true},
		{"glm-4.6v", "GLM 4.6V", 131072, 32768, false},
		{"glm-4.7", "GLM 4.7", 200000, 131072, true},
		{"glm-5", "GLM 5", 200000, 64000, true},
		{"glm-5-turbo", "GLM 5 Turbo", 200000, 64000, true},
		{"glm-5v-turbo", "GLM 5V Turbo", 200000, 131072, false},
		{"glm-5.1", "GLM 5.1", 200000, 64000, true},
		{"glm-5.2", "GLM 5.2", 1000000, 128000, true},
		{"glm-5.3", "GLM 5.3", 1000000, 128000, true},
		{"glm-5.3-flash", "GLM 5.3 Flash", 1000000, 128000, true},
	}
	out := make([]pluginapi.ModelInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, pluginapi.ModelInfo{
			ID:                         s.id,
			Name:                       s.name,
			ContextLength:              s.context,
			MaxCompletionTokens:        s.maxOut,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out
}

// filterCoolingModels removes models currently cooling for THIS credential
// (per-model cooldown) so the host's built-in scheduler is never offered a
// degraded (auth, model) pair.
func filterCoolingModels(sa *storedAuth, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	authID := cooldownAuthIDFor(sa)
	if authID == "" || len(models) == 0 || len(cooldownSnapshotFor(authID)) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if modelIsCooling(authID, m.ID) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for the
// zcode provider. The host passes this config via HostConfigSummary.
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

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	models := filterExcludedModels(zcodeModels(), req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// filterPlanModels narrows the catalog to the models the credential's plan
// tier actually serves. The start-plan (trial) gateway officially serves
// GLM-5.3-Flash / GLM-5.2 / GLM-5-Turbo (open-source zcode-builtin catalog:
// "start-plan 仅 GLM-5.3-Flash/5.2/5-Turbo — 按 plan 过滤有官方依据");
// coding-plan accounts keep the full pinned catalog.
func filterPlanModels(sa *storedAuth, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if sa == nil || sa.Auth.Plan != planStart {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, ok := startPlanModelIDs[strings.ToLower(m.ID)]; ok {
			out = append(out, m)
		}
	}
	return out
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key — the host skips any
	// response whose Provider doesn't match the auth's provider.
	models := zcodeModels()
	if sa, err := parseStored(req.StorageJSON); err == nil {
		models = filterPlanModels(sa, models)
		// Off-peak lane serves its own (much narrower) model set — narrow the
		// catalog so the host only offers models the ticketed gateway accepts.
		if offPeakEligible(sa) {
			models = filterOffPeakModels(models)
		}
		models = filterCoolingModels(sa, models)
	}
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
