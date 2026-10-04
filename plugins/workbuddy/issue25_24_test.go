package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Issue #24: login_region / login_platform must be sticky across bare
// reconfigures and must also be seen when the host sends the config as a
// flow-style mapping or JSON — neither of which the line-scan can match.

func wbConfigureWire(t *testing.T, cfg string) {
	t.Helper()
	raw, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(cfg)})
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	configure(raw)
}

func TestWorkbuddyConfigureStickyLoginRegion(t *testing.T) {
	oldRegion, oldPlatform := loginRegion, loginPlatform
	t.Cleanup(func() {
		loginRegionMu.Lock()
		loginRegion = oldRegion
		loginRegionMu.Unlock()
		loginPlatformMu.Lock()
		loginPlatform = oldPlatform
		loginPlatformMu.Unlock()
	})

	wbConfigureWire(t, "enabled: true\nlogin_region: intl\nlogin_platform: ide\npriority: 0\n")
	if loginRegion != regionIntl {
		t.Fatalf("block-style intl not applied: %q", loginRegion)
	}
	if loginPlatform != "ide" {
		t.Fatalf("login_platform ide not applied: %q", loginPlatform)
	}

	// Bare reconfigure (auth-store churn) must NOT reset to cn/CLI.
	wbConfigureWire(t, "enabled: true\npriority: 0\n")
	if loginRegion != regionIntl || loginPlatform != "ide" {
		t.Fatalf("bare reconfigure reset sticky values: region=%q platform=%q", loginRegion, loginPlatform)
	}

	// Explicit switch back still works.
	wbConfigureWire(t, "enabled: true\nlogin_region: cn\nlogin_platform: CLI\n")
	if loginRegion != regionCN || loginPlatform != "CLI" {
		t.Fatalf("explicit reset not applied: region=%q platform=%q", loginRegion, loginPlatform)
	}
}

func TestWorkbuddyConfigureFlowStyleAndJSON(t *testing.T) {
	oldRegion := loginRegion
	t.Cleanup(func() {
		loginRegionMu.Lock()
		loginRegion = oldRegion
		loginRegionMu.Unlock()
	})

	// Flow-style mapping: the line-scan sees one line, the YAML decode must.
	wbConfigureWire(t, "{enabled: true, login_region: intl, priority: 0}\n")
	if loginRegion != regionIntl {
		t.Fatalf("flow-style intl not applied: %q", loginRegion)
	}

	// JSON payload (YAML subset).
	wbConfigureWire(t, `{"enabled":true,"login_region":"intl"}`)
	if loginRegion != regionIntl {
		t.Fatalf("JSON intl not applied: %q", loginRegion)
	}
}

// Issue #25: the save funnel must carry host-owned operator fields across
// typed rebuilds.

func TestWorkbuddyPreservePluginDocKeysCarriesHostOwnedFields(t *testing.T) {
	physical := `{"type":"workbuddy","note":"old note","model_cache":{"x":1},"proxy_url":"http://127.0.0.1:7890","weight":3,"priority":9,"prefix":"teamA","label":"主号","request_retry":2,"headers":{"X-Extra":"1"}}`
	origList, origGet := hostAuthListFn, hostAuthGetPhysicalFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{AuthIndex: "i1", Name: "workbuddy-u1.json"}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "i1", Name: "workbuddy-u1.json", JSON: []byte(physical)}, nil
	}
	t.Cleanup(func() { hostAuthListFn, hostAuthGetPhysicalFn = origList, origGet })

	sa := &storedAuth{Account: storedAccount{UID: "u1"}}
	fresh, err := buildAuthFileJSON(sa, false, "new note", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	merged := preservePluginDocKeys("workbuddy-u1.json", fresh)

	var out map[string]json.RawMessage
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	for _, key := range []string{"proxy_url", "weight", "priority", "prefix", "label", "request_retry", "headers", "model_cache"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("merged doc lost host-owned key %q", key)
		}
	}
	// Plugin-owned fields keep the caller's fresh values.
	if string(out["note"]) != `"new note"` {
		t.Fatalf("note = %s, want fresh caller value", out["note"])
	}
}
