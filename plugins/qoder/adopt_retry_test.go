package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Issue #22: during plugin registration the host auth manager may not be
// wired yet — host.auth.list succeeds via its directory fallback but
// host.auth.get answers "bad envelope" for entries the manager has not
// indexed. Adoption must (a) fall back to reading the physical file by the
// list entry's path and (b) report waitingForHost so the caller retries,
// instead of silently giving up after one pass.

func stubAdoptHost(t *testing.T, entries []pluginapi.HostAuthFileEntry, getFails bool, files map[string]string) {
	t.Helper()
	savedAdopt = nil
	origList, origGet, origSave := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthSaveJSONFn
	hostAuthSaveJSONFn = func(name string, raw []byte) error {
		savedAdopt = append([]byte(nil), raw...)
		return nil
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) { return entries, nil }
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		if getFails {
			return nil, errAdoptManagerNotReady
		}
		for _, e := range entries {
			if e.AuthIndex == authIndex {
				return &hostAuthPhysical{AuthIndex: e.AuthIndex, Name: e.Name, Path: e.Path, JSON: []byte(files[e.Name])}, nil
			}
		}
		return nil, errAdoptManagerNotReady
	}
	t.Cleanup(func() { hostAuthListFn, hostAuthGetPhysicalFn, hostAuthSaveJSONFn = origList, origGet, origSave })
}

var savedAdopt []byte

var errAdoptManagerNotReady = &adoptTestError{"host.auth.get: bad envelope"}

type adoptTestError struct{ s string }

func (e *adoptTestError) Error() string { return e.s }

const legacyQoderCN = `{"type":"qoder-cn","auth":{"accessToken":"tok-cn"},"account":{"uid":"u1"}}`

func TestAdoptFallsBackToPhysicalPathWhenGetFails(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "qoder-cn-u1.json")
	if err := os.WriteFile(legacyPath, []byte(legacyQoderCN), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	stubAdoptHost(t,
		[]pluginapi.HostAuthFileEntry{{AuthIndex: "", Name: "qoder-cn-u1.json", Path: legacyPath}},
		true, // host.auth.get fails exactly like an unready manager
		nil,
	)

	if !adoptForeignAuths() {
		t.Fatalf("adoptForeignAuths = false, want true (path fallback should resolve the candidate)")
	}
	if savedAdopt == nil {
		t.Fatalf("adopt did not attempt a save")
	}
	raw := savedAdopt
	var probe struct {
		Type   string `json:"type"`
		Region string `json:"region"`
	}
	// region lives under auth after buildAuthFileJSON — probe the nested shape.
	var full struct {
		Type string `json:"type"`
		Auth struct {
			Region string `json:"region"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatalf("unmarshal migrated: %v", err)
	}
	_ = probe
	if full.Type != providerName {
		t.Fatalf("type = %q, want %q", full.Type, providerName)
	}
	if full.Auth.Region != regionCN {
		t.Fatalf("region = %q, want %q", full.Auth.Region, regionCN)
	}
}

func TestAdoptReportsWaitingForHostWhenNothingResolves(t *testing.T) {
	stubAdoptHost(t,
		[]pluginapi.HostAuthFileEntry{{AuthIndex: "", Name: "qoder-cn-u1.json", Path: ""}}, // no path either
		true, nil,
	)
	if adoptForeignAuths() {
		t.Fatalf("adoptForeignAuths = true, want false (manager not ready, no path to fall back to)")
	}
}

func TestAdoptIsNoOpWhenNoLegacyFiles(t *testing.T) {
	stubAdoptHost(t, []pluginapi.HostAuthFileEntry{{AuthIndex: "i1", Name: "qoder-u1.json"}}, false,
		map[string]string{`qoder-u1.json`: `{"type":"qoder"}`})
	if !adoptForeignAuths() {
		t.Fatalf("no legacy candidates should count as ready")
	}
}

// Issue #24: login_region must be sticky across bare reconfigures and must
// also be seen when the host sends the config as a flow-style mapping or
// JSON — neither of which the line-scan can match.

func TestQoderConfigureStickyLoginRegion(t *testing.T) {
	old := loadedLoginRegion()
	t.Cleanup(func() { setLoginRegion(old) })

	wire := func(cfg string) []byte {
		b, _ := json.Marshal(struct {
			ConfigYAML []byte `json:"config_yaml"`
		}{ConfigYAML: []byte(cfg)})
		return b
	}

	configure(wire("enabled: true\nlogin_region: intl\npriority: 0\n"))
	if got := loadedLoginRegion(); got != regionIntl {
		t.Fatalf("block-style intl not applied: %q", got)
	}

	// Bare reconfigure (auth-store churn) must NOT reset to cn.
	configure(wire("enabled: true\npriority: 0\n"))
	if got := loadedLoginRegion(); got != regionIntl {
		t.Fatalf("bare reconfigure reset login_region: %q", got)
	}

	// Explicit switch back still works.
	configure(wire("enabled: true\nlogin_region: cn\n"))
	if got := loadedLoginRegion(); got != regionCN {
		t.Fatalf("explicit cn not applied: %q", got)
	}
}

func TestQoderConfigureFlowStyleAndJSON(t *testing.T) {
	old := loadedLoginRegion()
	t.Cleanup(func() { setLoginRegion(old) })

	wire := func(cfg string) []byte {
		b, _ := json.Marshal(struct {
			ConfigYAML []byte `json:"config_yaml"`
		}{ConfigYAML: []byte(cfg)})
		return b
	}

	// Flow-style mapping: the line-scan sees one line, the YAML decode must.
	configure(wire("{enabled: true, login_region: intl, priority: 0}\n"))
	if got := loadedLoginRegion(); got != regionIntl {
		t.Fatalf("flow-style intl not applied: %q", got)
	}

	// JSON payload (YAML subset).
	configure(wire(`{"enabled":true,"login_region":"intl"}`))
	if got := loadedLoginRegion(); got != regionIntl {
		t.Fatalf("JSON intl not applied: %q", got)
	}
}

// Issue #25: the save funnel must carry host-owned operator fields
// (proxy_url/weight/priority/prefix/label/...) across typed rebuilds.

func TestPreservePluginDocKeysCarriesHostOwnedFields(t *testing.T) {
	physical := `{"type":"qoder","note":"old note","model_cache":{"realm":"cn"},"proxy_url":"http://127.0.0.1:7890","weight":3,"priority":9,"prefix":"teamA","label":"主号","request_retry":2}`
	origList, origGet := hostAuthListFn, hostAuthGetPhysicalFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{AuthIndex: "i1", Name: "qoder-u1.json"}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "i1", Name: "qoder-u1.json", JSON: []byte(physical)}, nil
	}
	t.Cleanup(func() { hostAuthListFn, hostAuthGetPhysicalFn = origList, origGet })

	fresh, err := buildAuthFileJSON(&storedAuth{Auth: storedTokens{Region: regionCN}, Account: storedAccount{UID: "u1"}}, false, "new note", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	merged := preservePluginDocKeys("qoder-u1.json", fresh)

	var out map[string]json.RawMessage
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	for _, key := range []string{"proxy_url", "weight", "priority", "prefix", "label", "request_retry", "model_cache"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("merged doc lost host-owned key %q", key)
		}
	}
	// Plugin-owned fields keep the caller's fresh values.
	if string(out["note"]) != `"new note"` {
		t.Fatalf("note = %s, want fresh caller value", out["note"])
	}
	if _, hasRegion := out["region"]; hasRegion {
		t.Fatalf("unexpected top-level region key")
	}
}

func TestConfigScalarCoercion(t *testing.T) {
	m, ok := decodePluginConfigYAML([]byte("k: v\nn: 30\nb: true\nq: \"quoted\"\n"))
	if !ok {
		t.Fatalf("decodePluginConfigYAML failed")
	}
	if got := configScalarString(m["k"]); got != "v" {
		t.Fatalf("k = %q", got)
	}
	if got := configScalarString(m["n"]); got != "30" {
		t.Fatalf("n = %q", got)
	}
	if got := configScalarString(m["q"]); got != "quoted" {
		t.Fatalf("q = %q", got)
	}
	if !configScalarBool(m["b"]) {
		t.Fatalf("b = false, want true")
	}
	if _, ok := decodePluginConfigYAML([]byte("not: [a: mapping: at: all")); ok {
		// malformed YAML — line-scan fallback must take over; decode may still
		// succeed or fail depending on yaml.v3 leniency, so only sanity-check
		// the helpers survive composite nodes.
		_ = ok
	}
	if got := configScalarString(nil); got != "" {
		t.Fatalf("nil scalar = %q", got)
	}
	if !strings.EqualFold(configScalarString(true), "true") {
		t.Fatalf("bool scalar = %q", configScalarString(true))
	}
}
