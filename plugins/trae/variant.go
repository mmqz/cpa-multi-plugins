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

// configureVariant parses login_variant from the plugin config block.
func configureVariant(raw []byte) {
	next := variantCN
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "login_variant:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "login_variant:")), "\"'")
					next = normalizeVariant(v)
				}
			}
		}
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

// requestVariantIsIntl reports whether an RPC request carrying storage_json
// targets an Intl account (merged trae-intl variant).
func requestVariantIsIntl(request []byte) bool {
	var req struct {
		StorageJSON json.RawMessage `json:"storage_json"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return false
	}
	return sniffVariantFromJSON(req.StorageJSON) == variantIntl
}

// loginVariantIsIntl reports whether NEW logins target the Intl realm.
func loginVariantIsIntl() bool {
	return loadedLoginVariant() == variantIntl
}
