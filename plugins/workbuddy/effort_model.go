package main

import "strings"

// reasoningRank is used only to choose the highest level a model actually
// advertises. It must not be used to invent a level the model did not return.
var reasoningRank = map[string]int{
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// thinkingLevelsForModel returns the exact Thinking.Levels discovered for a
// model, across the current per-realm catalogs. It never falls back to a
// hard-coded family table: an empty result means "do not guess".
func thinkingLevelsForModel(model string) []string {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return nil
	}
	prefix := strings.ToLower(providerName) + "/"
	if strings.HasPrefix(key, prefix) {
		key = strings.TrimPrefix(key, prefix)
	}
	resolved := strings.ToLower(strings.TrimSpace(resolveUpstreamModel(key, nil)))

	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	for _, entry := range dynamicModelsCache.realms {
		for _, info := range entry.models {
			id := strings.ToLower(strings.TrimSpace(info.ID))
			name := strings.ToLower(strings.TrimSpace(info.Name))
			if id != key && name != key && id != resolved && name != resolved {
				continue
			}
			if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
				return nil
			}
			return append([]string(nil), info.Thinking.Levels...)
		}
	}
	return nil
}

// highestEffortLevel returns the highest level advertised by the model.
// Unknown values are ignored; no advertised level means no override.
func highestEffortLevel(levels []string) string {
	best := ""
	bestRank := -1
	for _, level := range levels {
		rank, ok := reasoningRank[strings.ToLower(strings.TrimSpace(level))]
		if !ok || rank <= bestRank {
			continue
		}
		best = strings.ToLower(strings.TrimSpace(level))
		bestRank = rank
	}
	return best
}
