// identity.go emits the ZCode desktop client's companion headers so the
// plugin is indistinguishable from the official client at the fingerprinting
// layer. TWO distinct builders are mirrored (TriDefender/zcode-api
// src/proxy/identity.ts, ZCode 3.12.3):
//
//  1. `g6n` = buildCliZCodeSourceHeaders — the single source for LLM
//     completion defaultHeaders AND the coding-plan-signature gate fetch.
//     Shape: HTTP-Referer, User-Agent, [X-ZCode-App-Version], X-Title,
//     X-Release-Channel, X-Client-Language (always, "unknown" fallback),
//     X-Client-Timezone (always, "unknown" fallback), X-ZCode-Agent ("glm",
//     inline), [X-Platform], X-Os-Category (always), [X-Os-Version].
//     NO X-Device-Mid.
//
//  2. `TV` = buildZCodeSourceHeadersFromContext — the endpoint-routing /
//     billing / claim plane. X-ZCode-Agent is GONE from this plane and
//     language/timezone are always present with the "unknown" fallback.
//     Optional X-Device-Mid last.
//
// Both gate header values through the client's printable-ASCII rule; an
// invalid appVersion drops X-ZCode-App-Version entirely and falls the
// User-Agent back to `ZCode/unknown`.
package main

import (
	"os"
	"regexp"
	"runtime"
	"strings"
)

// ASCII gate copied from the ZCode client's printable-header rule.
var asciiPrintable = regexp.MustCompile(`^[\x20-\x7e]+$`)

// normalizePrintableHeaderValue trims and validates a header value against
// the printable-ASCII rule; non-conforming values are dropped (nil).
func normalizePrintableHeaderValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || !asciiPrintable.MatchString(v) {
		return ""
	}
	return v
}

// identityConfigSnapshot carries the identity values used by both builders.
type identityConfigSnapshot struct {
	appVersion  string
	sourceTitle string
	referer     string
	language    string
	timezone    string
}

// zcodeIdentity resolves the plugin-side identity values (config overrides →
// fixed defaults). Language/timezone keep the "unknown" fallback the client
// uses when locale detection fails.
func zcodeIdentity() identityConfigSnapshot {
	identityMu.RLock()
	defer identityMu.RUnlock()
	appVersion := normalizePrintableHeaderValue(identityAppVersion)
	if appVersion == "" {
		appVersion = zcodeAppVersion
	}
	sourceTitle := normalizePrintableHeaderValue(identitySourceTitle)
	if sourceTitle == "" {
		sourceTitle = "cli"
	}
	referer := normalizePrintableHeaderValue(identityReferer)
	if referer == "" {
		referer = "https://zcode.z.ai"
	}
	lang := normalizePrintableHeaderValue(identityLanguage)
	if lang == "" {
		lang = "unknown"
	}
	tz := normalizePrintableHeaderValue(identityTimezone)
	if tz == "" {
		tz = "unknown"
	}
	return identityConfigSnapshot{appVersion: appVersion, sourceTitle: sourceTitle, referer: referer, language: lang, timezone: tz}
}

// osCategory maps the Go runtime platform onto the client's X-Os-Category.
func osCategory(platform string) string {
	switch platform {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// platformArch renders the `${platform}-${arch}` pair used by X-Platform and
// the billing plane's platform query param.
func platformArch() (string, string) {
	return runtime.GOOS, runtime.GOARCH
}

// releaseChannel mirrors the client: ZCODE_ENV==="test" ? "test" : "production".
func releaseChannel() string {
	if strings.TrimSpace(os.Getenv("ZCODE_ENV")) == "test" {
		return "test"
	}
	return "production"
}

// buildLlmIdentityHeaders mirrors `g6n` — LLM completion requests (and the
// signing gate fetch). X-ZCode-Agent sits inline, X-Os-Category is
// unconditional, X-Device-Mid is NEVER sent.
func buildLlmIdentityHeaders(id identityConfigSnapshot) map[string]string {
	platform, arch := platformArch()
	h := map[string]string{
		"HTTP-Referer":      id.referer,
		"User-Agent":        "ZCode/" + orUnknown(id.appVersion),
		"X-Title":           "Z Code@" + id.sourceTitle,
		"X-Release-Channel": releaseChannel(),
		"X-Client-Language": id.language,
		"X-Client-Timezone": id.timezone,
		"X-ZCode-Agent":     "glm",
		"X-Os-Category":     osCategory(platform),
	}
	if id.appVersion != "" {
		h["X-ZCode-App-Version"] = id.appVersion
	}
	if platform != "" && arch != "" {
		h["X-Platform"] = platform + "-" + arch
	}
	if rel := osRelease(); rel != "" {
		h["X-Os-Version"] = rel
	}
	return h
}

// buildContextIdentityHeaders mirrors `TV` — control-plane fetches
// (endpoint routing, billing). Insertion order follows the client's spread
// semantics (User-Agent, HTTP-Referer, X-Title first), X-ZCode-Agent is
// absent, X-Device-Mid is appended last when a deviceMid is provided.
func buildContextIdentityHeaders(id identityConfigSnapshot, deviceMid string) map[string]string {
	platform, arch := platformArch()
	h := map[string]string{
		"User-Agent":        "ZCode/" + orUnknown(id.appVersion),
		"HTTP-Referer":      id.referer,
		"X-Title":           "Z Code@" + id.sourceTitle,
		"X-Client-Language": id.language,
		"X-Client-Timezone": id.timezone,
	}
	if id.appVersion != "" {
		h["X-ZCode-App-Version"] = id.appVersion
	}
	if platform != "" && arch != "" {
		h["X-Platform"] = platform + "-" + arch
		h["X-Os-Category"] = osCategory(platform)
	}
	if rel := osRelease(); rel != "" {
		h["X-Os-Version"] = rel
	}
	if dm := normalizePrintableHeaderValue(deviceMid); dm != "" {
		h["X-Device-Mid"] = dm
	}
	return h
}

// llmSDKUserAgentSuffix is the SDK UA suffix the real client appends to its
// User-Agent on LLM calls (bundle `Cm` merges ai-sdk/anthropic/3.0.81 into
// the UA; the suffix rides every LLM request, control-plane fetches keep the
// bare ZCode/… UA).
const llmSDKUserAgentSuffix = "ai-sdk/anthropic/3.0.81"

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// osRelease returns a best-effort OS release string for X-Os-Version.
// Go has no direct equivalent of uname -r on every platform; on Linux the
// /proc/sys/kernel/osrelease file is authoritative and always present on the
// server deployments CPA targets. Other platforms fall back to empty (header
// omitted — the client omits it when the value is non-printable too).
func osRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return normalizePrintableHeaderValue(string(b))
}
