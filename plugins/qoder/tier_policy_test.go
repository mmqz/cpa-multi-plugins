package main

import "testing"

func withQoderTierPolicy(t *testing.T, models any) func() {
	t.Helper()
	setPinnedEffortsFromModels(models)
	return func() { setPinnedEffortsFromModels(nil) }
}

func TestQoderTierPolicyPrecedence(t *testing.T) {
	restore := withQoderTierPolicy(t, []any{
		map[string]any{
			"id":                "qfmodel",
			"reasoning":         "high",
			"default_reasoning": "low",
		},
	})
	defer restore()

	cases := []struct {
		name   string
		client string
		pin    string
		want   string
	}{
		{"pin beats valid client", "max", "high", "high"},
		{"pin fills absent client", "", "high", "high"},
		{"pin beats invalid client", "ultra", "high", "high"},
		{"default fills absent client", "", "", "low"},
		{"client beats default", "max", "", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTierEffort(tc.client, tc.pin, "low")
			if got != tc.want {
				t.Fatalf("resolveTierEffort(%q) = %q, want %q", tc.client, got, tc.want)
			}
		})
	}
}

func TestQoderNoPolicyLeavesClientChoice(t *testing.T) {
	restore := withQoderTierPolicy(t, nil)
	defer restore()

	if got := resolveTierEffort("low", "", ""); got != "low" {
		t.Fatalf("client low changed to %q", got)
	}
	if got := resolveTierEffort("", "", ""); got != "" {
		t.Fatalf("empty client changed to %q", got)
	}
}
