// variant.go — per-variant platform constants for the merged trae plugin
// (v0.12.0). trae-cn (Trae Code CN) and trae-solo-cn (Trae SOLO CN) were
// separate plugins with identical logic except ClientID and the chat
// function; they are merged here, keyed by the account's variant.
package upstream

import "strings"

// clientIDByVariant / functionByVariant map auth variant → platform values
// (constants verified upstream; 禁止改动).
//
// v0.12.44: aligned with cockpit-tools' 2×2 lineage matrix
// (trae_account_core_platform_storage.rs:196-199) — the ClientID depends ONLY
// on solo-ness (en1oxy7wnw8j9n solo / ono9krqynydwx5 non-solo), the auth
// domain only on CN-ness (www.trae.cn / www.trae.ai). The four official
// products (TRAE, TRAE CN, TRAE SOLO, TRAE SOLO CN) are combinations of
// those two axes; cn/solo/intl/solo-intl here cover all four.
//
// v0.12.79 (issue #9): the function map deviates from the official client on
// purpose. llm_utils_chat rejects EVERY function value except solo_work_lite
// with in-stream 4001 "param is invalid" — corroborated by trae2api-web
// RESEARCH.md ("其他 function 值如 work/solo/work_lite 均无效"), trae2api-more
// IsModelConfigMismatch (4001 = model not available under current function)
// and the reporter's variant-flip experiment (same cn JWT, only the function
// changed, SOLO catalog lit up). cn/intl→inline_chat was a dead lane: every
// model 4001'd. cn now rides solo_work_lite (reporter: 2 CN accounts, 11
// models chat-verified usable). intl accounts never reach llm_utils_chat at
// all — they use the Web SOLO chat_sessions protocol (intlupstream module) —
// so their entry is unreachable from the chat path; it is aligned to
// solo_work_lite anyway so this map can never emit a dead function value
// again.
var (
	clientIDByVariant = map[string]string{
		"cn":        "ono9krqynydwx5", // non-solo (Trae Code CN = TRAE CN)
		"solo":      "en1oxy7wnw8j9n", // SOLO stable (Trae SOLO CN)
		"intl":      "ono9krqynydwx5", // TRAE intl — same non-solo client id (cockpit TRAE_AUTH_CLIENT_ID)
		"solo-intl": "en1oxy7wnw8j9n", // TRAE SOLO intl — same solo client id (cockpit TRAE_SOLO_AUTH_CLIENT_ID)
	}
	functionByVariant = map[string]string{
		"cn":        "solo_work_lite", // v0.12.79 (issue #9): llm_utils_chat's only live function
		"solo":      "solo_work_lite",
		"intl":      "solo_work_lite", // unreachable from chat (intlupstream chat_sessions); kept live
		"solo-intl": "solo_work_lite",
	}
)

// platformByVariant maps variant → official platformId / platformName
// (v0.12.44, values cross-checked against a live cockpit-tools export of a
// TRAE SOLO CN account: platformId=trae_solo_cn, platformName="TRAE SOLO CN";
// provider_key scheme: trae_account_core_product_paths.rs:654-658).
var (
	platformIDByVariant = map[string]string{
		"cn":        "trae_cn",
		"solo":      "trae_solo_cn",
		"intl":      "trae",
		"solo-intl": "trae_solo_intl",
	}
	platformNameByVariant = map[string]string{
		"cn":        "TRAE CN",
		"solo":      "TRAE SOLO CN",
		"intl":      "TRAE",
		"solo-intl": "TRAE SOLO",
	}
)

// PlatformIDFor returns the official platformId for a variant (default cn
// lineage). Used to stamp credential files with the explicit 4-way lineage
// cockpit-tools exports carry, so a credential is self-describing.
func PlatformIDFor(variant string) string {
	if v, ok := platformIDByVariant[variant]; ok {
		return v
	}
	return platformIDByVariant["cn"]
}

// PlatformNameFor returns the human platform name for a variant.
func PlatformNameFor(variant string) string {
	if v, ok := platformNameByVariant[variant]; ok {
		return v
	}
	return platformNameByVariant["cn"]
}

// IsSoloVariant reports whether the variant belongs to the SOLO lineage
// (client id en1oxy7wnw8j9n). Mirrors cockpit TraePlatformKind::is_solo.
func IsSoloVariant(variant string) bool {
	switch strings.ToLower(strings.TrimSpace(variant)) {
	case "solo", "solo-intl":
		return true
	}
	return false
}

// IsIntlVariant reports whether the variant belongs to the non-CN region
// (auth domain www.trae.ai). Mirrors cockpit TraePlatformKind::is_cn inverse.
func IsIntlVariant(variant string) bool {
	switch strings.ToLower(strings.TrimSpace(variant)) {
	case "intl", "solo-intl":
		return true
	}
	return false
}

// ClientIDFor returns the OAuth client id for a variant (default cn).
func ClientIDFor(variant string) string {
	if v, ok := clientIDByVariant[variant]; ok {
		return v
	}
	return clientIDByVariant["cn"]
}

// FunctionFor returns the llm_utils_chat function value for a variant.
func FunctionFor(variant string) string {
	if v, ok := functionByVariant[variant]; ok {
		return v
	}
	return functionByVariant["cn"]
}
