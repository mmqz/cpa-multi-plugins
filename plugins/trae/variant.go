// variant.go — per-account variant plumbing for the merged trae plugin
// (v0.12.0). trae-cn, trae-solo-cn and trae-intl were separate plugins;
// they are merged here. Each auth file carries its variant explicitly
// (auth.variant), derived from the filename for legacy files by adopt.go.
// NEW logins target the variant configured via login_variant.
package main

import (
	"encoding/json"
	"strings"
	"sync"
)

const (
	variantCN   = "cn"
	variantSolo = "solo"
	variantIntl = "intl"
)

var (
	loginVariantMu sync.RWMutex
	loginVariant   = variantCN
)

// normalizeVariant maps any stored hint onto cn/solo/intl (default cn).
func normalizeVariant(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case variantSolo:
		return variantSolo
	case variantIntl:
		return variantIntl
	default:
		return variantCN
	}
}

// oauthAuthFor returns the OAuth auth_from value for a login variant.
func oauthAuthFor(variant string) string {
	if variant == variantSolo {
		return "solo"
	}
	return "trae"
}

// oauthPlatformCodeFor returns the device platform code for a variant.
func oauthPlatformCodeFor(variant string) string {
	if variant == variantSolo {
		return "SOLO_PC"
	}
	return "IDE_PC"
}

// oauthHideSaasLoginFor reports whether the verification URI hides the
// SaaS login entry (SOLO only).
func oauthHideSaasLoginFor(variant string) bool {
	return variant == variantSolo
}

// variantLabel returns a human label for auth card fallbacks.
func variantLabel(variant string) string {
	switch variant {
	case variantSolo:
		return "Trae SOLO CN"
	case variantIntl:
		return "Trae Intl"
	default:
		return "Trae CN"
	}
}

// loadedLoginVariant returns the configured variant for NEW logins.
func loadedLoginVariant() string {
	loginVariantMu.RLock()
	defer loginVariantMu.RUnlock()
	return loginVariant
}

// OAuth callback listener knobs (v0.12.2): callback_bind controls the local
// bind address (0.0.0.0 for Docker/remote), callback_public_host controls
// the host advertised in the redirect URL (server IP/hostname for remote).
var (
	callbackBindMu      sync.RWMutex
	callbackBindValue   = "127.0.0.1"
	callbackPublicMu    sync.RWMutex
	callbackPublicValue = "127.0.0.1"
)

// loadedCallbackBind returns the local bind address for callback listeners.
func loadedCallbackBind() string {
	callbackBindMu.RLock()
	defer callbackBindMu.RUnlock()
	if callbackBindValue == "" {
		return "127.0.0.1"
	}
	return callbackBindValue
}

// loadedCallbackPublicHost returns the host used in the redirect URL.
func loadedCallbackPublicHost() string {
	callbackPublicMu.RLock()
	defer callbackPublicMu.RUnlock()
	if callbackPublicValue == "" {
		return "127.0.0.1"
	}
	return callbackPublicValue
}

// configureCallback parses callback_bind / callback_public_host from the
// plugin config block (same YAML line format as login_variant).
func configureCallback(lines []string) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "callback_bind:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "callback_bind:")), "\"'")
			if v != "" {
				callbackBindMu.Lock()
				callbackBindValue = v
				callbackBindMu.Unlock()
			}
		} else if strings.HasPrefix(line, "callback_public_host:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "callback_public_host:")), "\"'")
			if v != "" {
				callbackPublicMu.Lock()
				callbackPublicValue = v
				callbackPublicMu.Unlock()
			}
		}
	}
}

// configureVariant parses login_variant (and callback knobs) from the
// plugin config block. STICKY semantics (v0.12.3): login_variant only
// changes when the incoming config explicitly carries a login_variant
// line — the host may resend Register/Reconfigure with a bare or foreign
// config block (e.g. during auth-store churn), and resetting to the cn
// default mid-flight broke INTL logins by rerouting their polls.
func configureVariant(raw []byte) {
	next := ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			lines := strings.Split(string(req.ConfigYAML), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "login_variant:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "login_variant:")), "\"'")
					next = normalizeVariant(v)
				}
			}
			configureCallback(lines)
		}
	}
	if next == "" {
		return
	}
	loginVariantMu.Lock()
	loginVariant = next
	loginVariantMu.Unlock()
}

// sniffVariantFromJSON derives the variant of a legacy auth file: explicit
// auth.variant wins; otherwise Intl files are recognized by their
// Cloud-IDE / Web-IDE specific fields.
func sniffVariantFromJSON(raw []byte) string {
	var probe struct {
		Auth struct {
			Variant string `json:"variant"`
		} `json:"auth"`
		Variant      string `json:"variant"`
		WebID        string `json:"webId"`
		Tenant       string `json:"tenant"`
		UserIdentity string `json:"userIdentity"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.Auth.Variant != "" {
		return normalizeVariant(probe.Auth.Variant)
	}
	if probe.Variant != "" {
		return normalizeVariant(probe.Variant)
	}
	// Intl files carry webId / tenant / userIdentity (marscode.com realm).
	if probe.WebID != "" || probe.Tenant != "" || probe.UserIdentity != "" {
		return variantIntl
	}
	return variantCN
}

// requestVariantIsIntl reports whether an RPC request targets an Intl account
// (merged trae-intl variant). The host sends the credential in different
// shapes per method, all base64-encoded ([]byte JSON encoding):
//   - AuthModelRequest / ExecutorRequest / refresh: top-level "StorageJSON"
//   - AuthParseRequest: top-level "RawJSON"
//   - legacy plugin-side shape: snake_case "storage_json" raw JSON object
//
// v0.12.0-0.12.1 only probed the legacy shape, so Intl accounts silently
// routed through the CN/SOLO handlers (wrong protocol, wrong model list).
func requestVariantIsIntl(request []byte) bool {
	var withStorage struct {
		StorageJSON []byte `json:"StorageJSON"`
	}
	if err := json.Unmarshal(request, &withStorage); err == nil && len(withStorage.StorageJSON) > 0 {
		return sniffVariantFromJSON(withStorage.StorageJSON) == variantIntl
	}
	var withRaw struct {
		RawJSON []byte `json:"RawJSON"`
	}
	if err := json.Unmarshal(request, &withRaw); err == nil && len(withRaw.RawJSON) > 0 {
		return sniffVariantFromJSON(withRaw.RawJSON) == variantIntl
	}
	var legacy struct {
		StorageJSON json.RawMessage `json:"storage_json"`
	}
	if err := json.Unmarshal(request, &legacy); err == nil && len(legacy.StorageJSON) > 0 && legacy.StorageJSON[0] == '{' {
		return sniffVariantFromJSON(legacy.StorageJSON) == variantIntl
	}
	return false
}

// loginVariantIsIntl reports whether NEW logins target the Intl realm.
func loginVariantIsIntl() bool {
	return loadedLoginVariant() == variantIntl
}

// pollStateIsIntl reports whether a login-poll request's state was created
// by the Intl login flow (i.e. lives in intlloginStates). Routing polls by
// state location instead of the mutable login_variant global keeps
// in-flight logins immune to mid-flight variant flips (v0.12.3).
func pollStateIsIntl(request []byte) bool {
	var req struct {
		State string `json:"State"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return false
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return false
	}
	_, ok := intlloginStates.Load(state)
	return ok
}
