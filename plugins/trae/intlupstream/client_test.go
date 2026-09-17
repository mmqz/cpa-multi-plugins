package upstream

import "testing"

// v0.12.37: advertised Intl ids carry the "-intl" namespace suffix;
// resolveMode must send the bare model name upstream.
func TestResolveModeStripsIntlSuffix(t *testing.T) {
	mode, strategy, name := resolveMode("gpt-5.2-intl")
	if mode != "code" || strategy != "manual" || name != "gpt-5.2" {
		t.Errorf("resolveMode(gpt-5.2-intl)=%q/%q/%q", mode, strategy, name)
	}
	if _, _, name := resolveMode("auto"); name != "" {
		t.Errorf("auto should stay virtual, got %q", name)
	}
	if _, _, name := resolveMode("work"); name != "" {
		t.Errorf("work should stay virtual, got %q", name)
	}
	if _, strategy, name := resolveMode("claude-sonnet-4-5-intl"); strategy != "manual" || name != "claude-sonnet-4-5" {
		t.Errorf("resolveMode(claude-sonnet-4-5-intl) strategy/name=%q/%q", strategy, name)
	}
}
