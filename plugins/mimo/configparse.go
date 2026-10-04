// configparse.go decodes the plugin lifecycle config payload (config_yaml)
// as real YAML — block style, flow-style one-liners ({k: v}) and JSON (a
// YAML subset) all land in the same flat map. The per-plugin line-scan in
// usage_config.go stays as a fallback for payloads yaml.v3 refuses; both
// views agree on block style, so the map only ever ADDS visibility.
//
// History (issue #24): three plugins read login_variant/login_region by
// scanning YAML lines, which silently misses flow-style mappings — and the
// exact serialization style depends on how config.yaml was written. Parsing
// the document removes that ambiguity.
package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodePluginConfigYAML unmarshals the config payload into a flat
// string-keyed map. Returns ok=false when the payload is empty, not a
// mapping, or malformed YAML — callers then fall back to the line-scan.
func decodePluginConfigYAML(configYAML []byte) (map[string]any, bool) {
	trimmed := strings.TrimSpace(string(configYAML))
	if trimmed == "" {
		return nil, false
	}
	var m map[string]any
	if err := yaml.Unmarshal(configYAML, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// configScalarString coerces a decoded YAML scalar (string / int / float /
// bool) into its canonical string form. Composite nodes (maps, sequences)
// return "" — plugin config keys are all scalars.
func configScalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	default:
		s := strings.TrimSpace(fmt.Sprint(t))
		if s == "<nil>" {
			return ""
		}
		return s
	}
}

// configScalarBool interprets a decoded scalar with the same truth table as
// the line-scan ("true"/"1"/"yes"/"on" — YAML 1.1 style), so both paths agree.
func configScalarBool(v any) bool {
	switch t := configScalarString(v); strings.ToLower(t) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
