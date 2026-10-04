package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// normalizeWorkbuddyAuthDoc validates one credential document and stamps the
// OAuth attribution the host routes and classifies on (type/provider/
// auth_kind) without disturbing anything else. Every physical save funnels
// through here so attribution survives no matter which code path produced
// the payload, and panel-managed fields (proxy_url, logo, future keys) ride
// along as raw JSON — re-marshaling them through a typed struct would
// silently drop them.
//
// Re-authored clean-room from libo0118's qoder-custom (8b77d18, plus the
// enrichAuthMetadata slice of 8be80ff; reviewed 2026-09-25). The write-side
// ownership rule mirrors what handleParseAuth already enforces on the read
// side. Deliberately NOT absorbed from the same proposal: its token-level
// merge inside the auth object. This plugin's merge boundary is a
// documented decision — mergeStoredAuthIntoDoc preserves unknown TOP-LEVEL
// fields but replaces the owned auth/account objects wholesale, so stale
// sub-fields cannot contradict the refreshed tokens
// (TestMergeStoredAuthIntoDocOwnedObjectReplace) — and the wipeout the
// proposal was fixing (a bare json.Marshal(sa) refresh) has not existed
// here since v0.9.27.
func normalizeWorkbuddyAuthDoc(raw []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("invalid WorkBuddy credential object")
	}
	// Never claim a foreign credential: a declared type/provider must name
	// our family (workbuddy/codebuddy pre-merge names included). A
	// non-string declaration is malformed rather than foreign — rejected
	// for the same fail-closed reason. Blank declarations are tolerated
	// and overwritten below.
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
			return nil, fmt.Errorf("credential %s does not belong to WorkBuddy", key)
		}
	}
	doc["type"] = json.RawMessage(`"workbuddy"`)
	doc["provider"] = json.RawMessage(`"workbuddy"`)
	doc["auth_kind"] = json.RawMessage(`"oauth"`)
	return doc, nil
}
