package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetLearnedAliases clears the package-global learned-mapping store so
// tests stay order-independent.
func resetLearnedAliases() {
	intlLearnedReal.Range(func(k, _ any) bool {
		intlLearnedReal.Delete(k)
		return true
	})
}

// TestIntlAliasDisplayNames_FieldReportCoverage — every tier alias observed
// in the 2026-09-20 field reports (initial: 4 ids; user panel later the
// same day: +primary-model / deep-model / enhance-1.0) must carry an
// annotation. Genuine model ids stay out of the map.
func TestIntlAliasDisplayNames_FieldReportCoverage(t *testing.T) {
	for _, id := range []string{
		"fast-model", "auto-chat", "balanced-model", "default-model",
		"primary-model", "deep-model", "enhance-1.0",
	} {
		if _, ok := intlAliasDisplayNames[id]; !ok {
			t.Errorf("tier alias %q missing from intlAliasDisplayNames", id)
		}
	}
	for _, real := range []string{"o4-mini", "hy4-preview", "glm-4.6"} {
		if _, ok := intlAliasDisplayNames[real]; ok {
			t.Errorf("genuine model id %q must not be annotated as a tier alias", real)
		}
	}
}

// noteLearnedRealModel records only informative echoes: alias→real, never
// self/alias→alias/unknown-model inputs, matched case-insensitively.
func TestNoteLearnedRealModel(t *testing.T) {
	resetLearnedAliases()
	t.Cleanup(resetLearnedAliases)

	noteLearnedRealModel("fast-model", "glm-4.6-air")
	if got := learnedRealModel("FAST-MODEL"); got != "glm-4.6-air" {
		t.Fatalf("learned lookup must be case-insensitive, got %q", got)
	}

	// Self echo and alias→alias echoes carry no information.
	noteLearnedRealModel("fast-model", "fast-model")
	if got := learnedRealModel("fast-model"); got != "glm-4.6-air" {
		t.Fatalf("self echo must not overwrite, got %q", got)
	}
	noteLearnedRealModel("fast-model", "auto-chat")
	if got := learnedRealModel("fast-model"); got != "glm-4.6-air" {
		t.Fatalf("alias→alias echo must not be recorded, got %q", got)
	}
	noteLearnedRealModel("fast-model", "")
	if got := learnedRealModel("fast-model"); got != "glm-4.6-air" {
		t.Fatalf("empty echo must be ignored, got %q", got)
	}

	// Non-alias ids never participate.
	noteLearnedRealModel("o4-mini", "glm-4.6-air")
	if got := learnedRealModel("o4-mini"); got != "" {
		t.Fatalf("genuine model id must not gain a mapping, got %q", got)
	}
	noteLearnedRealModel("glm-4.6", "something-else")
	if got := learnedRealModel("glm-4.6"); got != "" {
		t.Fatalf("unknown model must not gain a mapping, got %q", got)
	}

	// A later DIFFERENT real id updates the mapping (upstream re-balances
	// tiers; latest evidence wins).
	noteLearnedRealModel("fast-model", "glm-4.7-flash")
	if got := learnedRealModel("fast-model"); got != "glm-4.7-flash" {
		t.Fatalf("newer evidence must win, got %q", got)
	}
}

// applyLearnedAliasNames annotates only aliases with learned mappings,
// never mutates the input slice, and is idempotent (no double suffix).
func TestApplyLearnedAliasNames(t *testing.T) {
	resetLearnedAliases()
	t.Cleanup(resetLearnedAliases)
	noteLearnedRealModel("fast-model", "glm-4.6-air")

	in := []pluginapi.ModelInfo{
		{ID: "fast-model", Name: "Fast Model（上游别名）"},
		{ID: "o4-mini", Name: "o4-mini"},
		{ID: "default-model", Name: "Default Model（上游别名）"},
	}
	out := applyLearnedAliasNames(in)
	if out[0].Name != "Fast Model（上游别名）·实测 glm-4.6-air" {
		t.Fatalf("learned alias must be annotated, got %q", out[0].Name)
	}
	if out[1].Name != "o4-mini" || out[2].Name != "Default Model（上游别名）" {
		t.Fatalf("unlearned entries must stay untouched, got %q / %q", out[1].Name, out[2].Name)
	}
	if in[0].Name != "Fast Model（上游别名）" {
		t.Fatalf("input slice must not be mutated, got %q", in[0].Name)
	}

	again := applyLearnedAliasNames(out)
	if again[0].Name != out[0].Name {
		t.Fatalf("annotation must be idempotent, got %q want %q", again[0].Name, out[0].Name)
	}

	// Empty / nil lists pass through.
	if got := applyLearnedAliasNames(nil); got != nil {
		t.Fatalf("nil list must pass through, got %v", got)
	}
}

// learnedRealSnapshot must copy (diagnostics consumers own their map).
func TestLearnedRealSnapshot(t *testing.T) {
	resetLearnedAliases()
	t.Cleanup(resetLearnedAliases)
	noteLearnedRealModel("deep-model", "glm-4.6")
	snap := learnedRealSnapshot()
	if snap["deep-model"] != "glm-4.6" {
		t.Fatalf("snapshot missing entry: %v", snap)
	}
	snap["deep-model"] = "tampered"
	if got := learnedRealModel("deep-model"); got != "glm-4.6" {
		t.Fatalf("snapshot must be a copy, store now has %q", got)
	}
}
