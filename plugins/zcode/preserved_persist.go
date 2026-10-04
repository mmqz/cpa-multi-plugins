// preserved_persist.go re-injects whitelisted top-level keys that the
// incoming document is missing but the current physical file carries
// (issue #25: host-owned operator fields the CPA auth manager writes —
// proxy_url / weight / priority / prefix / label / request_retry / headers —
// must survive every plugin-side rewrite; keys the caller explicitly set
// always win over the persisted copy).
package main

import (
	"encoding/json"
	"strings"
)

var preservedPluginDocKeys = []string{
	"proxy_url",
	"weight",
	"priority",
	"prefix",
	"label",
	"request_retry",
	"headers",
}

// hostAuthListFn is the injectable seam for physicalDocByName (tests stub it
// instead of touching the host bridge).
var hostAuthListFn = hostAuthList

func preservePluginDocKeys(name string, doc []byte) []byte {
	if len(doc) == 0 || name == "" {
		return doc
	}
	var have map[string]json.RawMessage
	if err := json.Unmarshal(doc, &have); err != nil {
		return doc
	}
	needed := make([]string, 0, len(preservedPluginDocKeys))
	for _, k := range preservedPluginDocKeys {
		if _, ok := have[k]; !ok {
			needed = append(needed, k)
		}
	}
	if len(needed) == 0 {
		return doc
	}
	physical := physicalDocByName(name)
	if len(physical) == 0 {
		return doc
	}
	var prev map[string]json.RawMessage
	if err := json.Unmarshal(physical, &prev); err != nil {
		return doc
	}
	restored := false
	for _, k := range needed {
		raw, ok := prev[k]
		if !ok || len(raw) == 0 {
			continue
		}
		have[k] = raw
		restored = true
	}
	if !restored {
		return doc
	}
	merged, err := json.Marshal(have)
	if err != nil {
		return doc
	}
	return merged
}

// physicalDocByName resolves one credential document by exact file name
// (best-effort; empty when the name is unknown — e.g. a first save).
func physicalDocByName(name string) []byte {
	files, err := hostAuthListFn()
	if err != nil {
		return nil
	}
	for _, f := range files {
		if !strings.EqualFold(strings.TrimSpace(f.Name), name) {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil {
			return nil
		}
		return phys.JSON
	}
	return nil
}
