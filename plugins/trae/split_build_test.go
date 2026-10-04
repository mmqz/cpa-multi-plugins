package main

import "testing"

// TestSplitBuildPinWiring verifies the split-channel build injection chain
// end-to-end: run once WITHOUT ldflags (unified flavor: pin empty, default
// stays cn) and once WITH
//
//	go test -ldflags "-X main.pluginVariantPin=intl" -run TestSplitBuildPinWiring
//
// (split flavor: the sticky login-variant default must equal the pin).
func TestSplitBuildPinWiring(t *testing.T) {
	switch pluginVariantPin {
	case "":
		if got := loadedLoginVariant(); got != variantCN {
			t.Fatalf("unified flavor: login default = %q, want %q", got, variantCN)
		}
	case variantCN, variantSolo, variantIntl:
		if got := loadedLoginVariant(); got != pluginVariantPin {
			t.Fatalf("split flavor pin=%s: login default = %q, want the pin", pluginVariantPin, got)
		}
	default:
		t.Fatalf("invalid pin %q leaked through", pluginVariantPin)
	}
}

// TestSplitCredentialFileName pins the split-build credential namespace: the
// registered id already encodes the variant, so no variant infix may be
// appended (would produce "trae-intl-intl-<uid>.json").
func TestSplitCredentialFileName(t *testing.T) {
	if providerName == "trae" {
		if got := credentialFileName(variantIntl, "u1"); got != "trae-intl-u1.json" {
			t.Fatalf("unified intl file = %q", got)
		}
		if got := credentialFileName(variantSolo, "u1"); got != "trae-solo-cn-u1.json" {
			t.Fatalf("unified solo file = %q", got)
		}
		return
	}
	// Split build: id encodes the variant.
	if got := credentialFileName(variantIntl, "u1"); got != providerName+"-u1.json" {
		t.Fatalf("split file = %q, want %q", got, providerName+"-u1.json")
	}
}
