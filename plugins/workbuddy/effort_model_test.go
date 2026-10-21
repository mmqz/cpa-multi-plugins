package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// withThinkingLevels installs a small catalog for the duration of one test.
// It keeps forceMaxThinking tests deterministic without relying on a live
// upstream discovery call.
func withThinkingLevels(t *testing.T, entries map[string][]string) func() {
	t.Helper()
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.realms
	models := make([]pluginapi.ModelInfo, 0, len(entries))
	for id, levels := range entries {
		models = append(models, pluginapi.ModelInfo{
			ID:   id,
			Name: id,
			Thinking: &pluginapi.ThinkingSupport{
				Levels: append([]string(nil), levels...),
			},
		})
	}
	dynamicModelsCache.realms = map[string]realmModelsEntry{
		"test": {models: models},
	}
	dynamicModelsCache.Unlock()
	return func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.realms = prev
		dynamicModelsCache.Unlock()
	}
}

func TestForceMaxThinking_UsesHighestSupportedLevel(t *testing.T) {
	restore := withThinkingLevels(t, map[string][]string{
		"step-5-preview":      {"low", "medium", "high"},
		"glm-5.2":             {"high", "xhigh"},
		"deepseek-v4.1-flash": {"low", "high", "max"},
	})
	defer restore()

	cases := []struct {
		model string
		want  string
	}{
		{"step-5-preview", "high"},
		{"glm-5.2", "xhigh"},
		{"deepseek-v4.1-flash", "max"},
	}
	for _, tc := range cases {
		obj := map[string]any{"model": tc.model}
		if !forceMaxThinking(obj) {
			t.Fatalf("%s should be adjusted", tc.model)
		}
		if got := obj["reasoning_effort"]; got != tc.want {
			t.Errorf("%s effort = %v, want %s", tc.model, got, tc.want)
		}
	}
}

func TestForceMaxThinking_UnknownModelDoesNotGuess(t *testing.T) {
	restore := withThinkingLevels(t, map[string][]string{})
	defer restore()

	obj := map[string]any{"model": "unknown-model", "reasoning_effort": "low"}
	if forceMaxThinking(obj) {
		t.Fatal("unknown model must not get a guessed effort")
	}
	if got := obj["reasoning_effort"]; got != "low" {
		t.Fatalf("unknown model effort changed to %v", got)
	}
}
