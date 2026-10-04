package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// normalizeQoderAuthDoc validates one credential document and stamps the
// OAuth attribution the host routes and classifies on (type/provider/
// auth_kind) without disturbing anything else. Every physical save funnels
// through here so attribution survives no matter which code path produced
// the payload, and operator metadata (priority, prefix, custom, proxy_url,
// ...) rides along as raw JSON — the plugin's structs deliberately do not
// model panel-managed fields, and re-marshaling them through a typed struct
// would silently drop them.
//
// Re-authored clean-room from libo0118's qoder-custom (ee5057a, 2026-09-23);
// the write-side ownership rule mirrors what handleParseAuth already
// enforces on the read side.
func normalizeQoderAuthDoc(raw []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("invalid Qoder credential object")
	}
	// Never claim a foreign credential: a declared type/provider must name
	// our family (pre-merge names included). A non-string declaration is
	// malformed rather than foreign — rejected for the same fail-closed
	// reason. Blank declarations are tolerated and overwritten below.
	for _, key := range []string{"type", "provider"} {
		declared, exists := doc[key]
		if !exists {
			continue
		}
		var name string
		if err := json.Unmarshal(declared, &name); err != nil {
			return nil, fmt.Errorf("credential %s is not a string", key)
		}
		if strings.TrimSpace(name) != "" && !isOurDeclaredType(name) {
			return nil, fmt.Errorf("credential %s does not belong to Qoder", key)
		}
	}
	doc["type"] = json.RawMessage(`"qoder"`)
	doc["provider"] = json.RawMessage(`"qoder"`)
	doc["auth_kind"] = json.RawMessage(`"oauth"`)
	return doc, nil
}

// mergeQoderRefreshedTokens rewrites the rotating token pair plus expiry in
// the on-disk document and touches nothing else. A refresh never changes the
// account object, the PAT fallback, realm/region markers, or operator
// fields, so updating exactly three keys inside the auth object (nested
// shape) or at the top level (legacy flat shape, still parsed by
// parseStored) is strictly safer than rebuilding: a rebuild projects the
// credential through the plugin's structs and drops every field the structs
// do not model. An empty physical document (first save before the host has
// materialized the file) starts a fresh classified document; an unreadable
// one is an error — silently discarding it would reintroduce the wipeout
// this merge exists to prevent.
func mergeQoderRefreshedTokens(physJSON []byte, tokens storedTokens) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if trimmed := strings.TrimSpace(string(physJSON)); trimmed != "" {
		var err error
		doc, err = normalizeQoderAuthDoc(physJSON)
		if err != nil {
			return nil, err
		}
	} else {
		doc["type"] = json.RawMessage(`"qoder"`)
		doc["provider"] = json.RawMessage(`"qoder"`)
		doc["auth_kind"] = json.RawMessage(`"oauth"`)
	}
	auth := doc
	if nested, ok := doc["auth"]; ok {
		auth = nil
		if err := json.Unmarshal(nested, &auth); err != nil || auth == nil {
			return nil, fmt.Errorf("invalid Qoder auth object")
		}
	}
	access, err := json.Marshal(tokens.AccessToken)
	if err != nil {
		return nil, err
	}
	refresh, err := json.Marshal(tokens.RefreshToken)
	if err != nil {
		return nil, err
	}
	expiry, err := json.Marshal(tokens.ExpiresAt)
	if err != nil {
		return nil, err
	}
	auth["accessToken"] = access
	auth["refreshToken"] = refresh
	auth["expiresAt"] = expiry
	if _, ok := doc["auth"]; ok {
		merged, err := json.Marshal(auth)
		if err != nil {
			return nil, err
		}
		doc["auth"] = merged
	}
	return json.Marshal(doc)
}
