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
	case "cn", "intl":
		if got := loadedLoginRegion(); got != pluginVariantPin {
			t.Fatalf("split flavor pin=%s: login default = %q, want the pin", pluginVariantPin, got)
		}
	default:
		t.Fatalf("invalid pin %q leaked through", pluginVariantPin)
	}
}

// TestSplitAuthFileName pins the split-build credential namespace: the
// registered id already encodes the realm, so no intl infix may be appended.
func TestSplitAuthFileName(t *testing.T) {
	if providerName == "workbuddy" {
		if got := authFileNameFor(&storedAuth{Account: storedAccount{UID: "u1"}, Auth: storedTokens{Domain: "www.codebuddy.ai"}}); got != "workbuddy-intl-u1.json" {
			t.Fatalf("unified intl file = %q", got)
		}
		return
	}
	if got := authFileNameFor(&storedAuth{Account: storedAccount{UID: "u1"}}); got != providerName+"-u1.json" {
		t.Fatalf("split file = %q, want %q", got, providerName+"-u1.json")
	}
}
