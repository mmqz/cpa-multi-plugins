package main

import "testing"

func withTierPolicy(t *testing.T, models any) func() {
	t.Helper()
	setPinnedEffortsFromModels(models)
	return func() { setPinnedEffortsFromModels(nil) }
}

func TestWorkBuddyTierPolicyPrecedence(t *testing.T) {
	restore := withTierPolicy(t, []any{
		map[string]any{
			"id":                "deepseek-v4.1-flash",
			"reasoning":         "high",
			"default_reasoning": "low",
		},
	})
	defer restore()
	restoreLevels := withThinkingLevels(t, map[string][]string{
		"deepseek-v4.1-flash": {"low", "high", "max"},
	})
	defer restoreLevels()

	// Forced pin beats a valid client choice.
	obj := map[string]any{"model": "deepseek-v4.1-flash", "reasoning_effort": "max"}
	if !applyPinnedEffortInPlace(obj) || obj["reasoning_effort"] != "high" {
		t.Fatalf("pin did not win: %#v", obj)
	}
	if forceMaxThinking(obj) {
		t.Fatal("automatic max fallback must not override a forced pin")
	}

	// Default only fills an absent client value.
	obj = map[string]any{"model": "deepseek-v4.1-flash"}
	if !applyDefaultEffortInPlace(obj) || obj["reasoning_effort"] != "low" {
		t.Fatalf("default did not fill absent value: %#v", obj)
	}
	if forceMaxThinking(obj) {
		t.Fatal("automatic max fallback must not override a configured default")
	}

	// A valid client choice beats default.
	obj = map[string]any{"model": "deepseek-v4.1-flash", "reasoning_effort": "max"}
	if applyDefaultEffortInPlace(obj) {
		t.Fatal("default overrode a valid client choice")
	}
	if obj["reasoning_effort"] != "max" {
		t.Fatalf("client choice changed: %#v", obj)
	}
	if forceMaxThinking(obj) {
		t.Fatal("automatic max fallback overrode a valid client choice")
	}
}

func TestWorkBuddyTierPolicyInvalidLevelsDoNotApply(t *testing.T) {
	restore := withTierPolicy(t, []any{
		map[string]any{
			"id":                "glm-5.2",
			"reasoning":         "max",
			"default_reasoning": "low",
		},
	})
	defer restore()
	restoreLevels := withThinkingLevels(t, map[string][]string{"glm-5.2": {"high", "xhigh"}})
	defer restoreLevels()

	obj := map[string]any{"model": "glm-5.2", "reasoning_effort": "high"}
	if applyPinnedEffortInPlace(obj) {
		t.Fatal("invalid pin was applied")
	}
	if applyDefaultEffortInPlace(obj) {
		t.Fatal("invalid default overrode a valid client choice")
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("valid client choice changed: %#v", obj)
	}
}

func TestWorkBuddyAutomaticMaxIsFinalFallbackOnly(t *testing.T) {
	restore := withTierPolicy(t, nil)
	defer restore()
	restoreLevels := withThinkingLevels(t, map[string][]string{
		"deepseek-v4.1-flash": {"low", "high", "max"},
	})
	defer restoreLevels()

	obj := map[string]any{"model": "deepseek-v4.1-flash", "reasoning_effort": "low"}
	if forceMaxThinking(obj) {
		t.Fatal("valid client choice must beat automatic max fallback")
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("client choice changed: %#v", obj)
	}

	obj = map[string]any{"model": "deepseek-v4.1-flash"}
	if !forceMaxThinking(obj) || obj["reasoning_effort"] != "max" {
		t.Fatalf("automatic max fallback failed: %#v", obj)
	}
}
