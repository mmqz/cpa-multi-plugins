package main

// host_owned_fields.go — issue #68: the host-owned operator fields must ride
// the auth.parse response, not only the physical file.
//
// Two host-side gaps make this necessary (both verified against CLIProxyAPI
// v7.2.30):
//
//  1. The host's loader paths (internal/pluginhost buildAuthFromFileData and
//     sdk/auth readAuthFiles) rebuild the in-memory Auth record WITHOUT
//     re-reading proxy_url / prefix / priority from the file — only the
//     management panel's own update path syncs them
//     (syncAuthFileMetadataFields). So after a restart (or after any
//     host.auth.save upsert) the panel shows the proxy as "none" and the
//     per-auth outbound proxy silently stops applying, even though the file
//     still carries proxy_url.
//  2. The host's persist path for plugin auths (pluginTokenStorage ->
//     mergedStorageJSON) rebuilds the file from StorageJSON + Metadata; keys
//     absent from Metadata are dropped from the file on the next persist.
//
// auth.parse is the one hook the plugin fully controls: AuthData.ProxyURL and
// AuthData.Prefix are applied verbatim by the host (pluginAuthDataToCoreAuth),
// and every Metadata key is both visible to the panel and written back by the
// persist path. Re-injecting the whitelist here therefore restores the panel
// display and the per-auth proxy after every restart, and keeps the persist
// path from stripping the keys from the file.
//
// The whitelist mirrors the issue #25 save-funnel set (models_persist.go),
// so file-side and parse-side stay in lockstep.

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostOwnedAuthFields is the issue #25 save-funnel whitelist: operator keys
// CPA's auth manager owns at the document top level.
var hostOwnedAuthFields = []string{
	"proxy_url",
	"weight",
	"priority",
	"prefix",
	"label",
	"request_retry",
	"headers",
}

// applyHostOwnedAuthFields copies the host-owned operator fields from the
// physical document's top level into the AuthData the host rebuilds its
// in-memory record from. Callers pass the raw auth-file JSON (req.RawJSON);
// a payload that fails to unmarshal is left untouched — this helper must
// never reject a parse that would otherwise succeed.
func applyHostOwnedAuthFields(ad *pluginapi.AuthData, rawJSON []byte) {
	if ad == nil || len(rawJSON) == 0 {
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &top); err != nil {
		return
	}
	if ad.Metadata == nil {
		ad.Metadata = map[string]any{}
	}
	for _, key := range hostOwnedAuthFields {
		raw, ok := top[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		ad.Metadata[key] = v
	}
	// The dedicated AuthData fields the host applies to the in-memory record
	// (panel display + per-auth outbound proxy, issue #68).
	if s, ok := ad.Metadata["proxy_url"].(string); ok {
		ad.ProxyURL = strings.TrimSpace(s)
	}
	if s, ok := ad.Metadata["prefix"].(string); ok {
		ad.Prefix = strings.TrimSpace(s)
	}
}
