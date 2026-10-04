package main

import "testing"

// TestSplitBuildPinWiring verifies the split-channel build injection chain
// end-to-end: run once WITHOUT ldflags (unified flavor: pin empty, default
// stays cn) and once WITH
//
//	go test -ldflags "-X main.pluginVariantPin=intl" -run TestSplitBuildPinWiring
//
// (split flavor: the sticky login-region default must equal the pin).
func TestSplitBuildPinWiring(t *testing.T) {
	switch pluginVariantPin {
	case "":
		if got := loadedLoginRegion(); got != regionCN {
			t.Fatalf("unified flavor: login default = %q, want %q", got, regionCN)
		}
	case regionCN, regionIntl:
		if got := loadedLoginRegion(); got != pluginVariantPin {
			t.Fatalf("split flavor pin=%s: login default = %q, want the pin", pluginVariantPin, got)
		}
	default:
		t.Fatalf("invalid pin %q leaked through", pluginVariantPin)
	}
}
