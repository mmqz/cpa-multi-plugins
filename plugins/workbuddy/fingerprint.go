// fingerprint.go — v0.9.69 (issue #56): client fingerprint version refresh.
//
// The upstream protocol's reference implementations moved on while this
// plugin's identity strings stood still (2026-10-18 captures from the
// workbuddy2api-hub project, as filed in issue #56):
//
//	CLI identity UA      CLI/2.108.1 CodeBuddy/2.108.1  →  CLI/2.168.2 CodeBuddy/2.168.2
//	Desktop UA (CN)      WorkBuddy/5.5.6 … CLI/2.137.1  →  WorkBuddy/5.8.3 … CLI/2.161.0
//	Desktop UA (Intl)    WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0
//	X-IDE-Version        5.5.6                          →  5.8.3
//	OpenAI SDK headers   (absent)                       →  X-Stainless-* (Node SDK 6.25.0 shape)
//
// None of this changes wire correctness — the issue itself notes it "only
// looks like an older client". But looking three minor versions behind on
// every request is free risk surface against the 11128 channel-verification
// family, so the strings ride the observed values now. Every constant that
// appears in more than one header/event field lives here so the next bump is
// a single edit.
//
// The X-Stainless family mirrors what the official CLI's bundled OpenAI
// Node SDK (6.25.0) sends on chat calls: language, package version, OS/arch
// (mapped to the SDK's process.platform / process.arch spellings — darwin
// becomes "macOS", amd64 becomes "x64"), runtime, and runtime version. OS
// and arch reflect the REAL machine the plugin runs on, because that is
// exactly what the official client would report from the same seat.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"runtime"
	"strings"
)

// desktopClientVersion is the official WorkBuddy desktop client's version
// (CN build; the Intl build's workbuddy-ai naming doesn't appear on any
// request path this plugin owns). 2026-10-18 capture: 5.8.3.
const desktopClientVersion = "5.8.3"

// desktopBundledCLIVersion is the CLI engine the desktop client embeds.
// 2026-10-18 capture: 2.161.0 (the standalone CLI package itself is 2.168.2).
const desktopBundledCLIVersion = "2.161.0"

// openaiSDKPackageVersion — the official CLI's bundled OpenAI Node SDK
// version, echoed by its X-Stainless-Package-Version header.
const openaiSDKPackageVersion = "6.25.0"

// openaiSDKRuntimeVersion — the Node runtime that desktop/CLI family ships
// with. Any believable v2x LTS reads as a current client; the captures in
// issue #56 pin the SDK version, not the patch level of node itself.
const openaiSDKRuntimeVersion = "v22.14.0"

// stainlessOS maps the plugin's runtime GOOS onto the OpenAI Node SDK's
// process.platform vocabulary (darwin → "macOS", linux → "Linux",
// windows → "Windows"); unknown platforms pass through as-is, which is also
// what the SDK does for values it doesn't special-case.
func stainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

// stainlessArch maps GOARCH onto the SDK's process.arch vocabulary
// (amd64 → "x64", arm64 → "arm64").
func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// applyStainlessHeaders stamps the OpenAI Node SDK fingerprint family onto a
// chat request when the caller is speaking with the CLI identity. The IDE
// (CodeBuddyIDE) identity keeps its own header family — mixing the two would
// be a NEW mismatch, not an alignment.
func applyStainlessHeaders(req *http.Request) {
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Package-Version", openaiSDKPackageVersion)
	req.Header.Set("X-Stainless-OS", stainlessOS())
	req.Header.Set("X-Stainless-Arch", stainlessArch())
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", openaiSDKRuntimeVersion)
}

// isIDEPlatform reports whether the credential speaks with the CodeBuddyIDE
// header family (adopted IDE logins) rather than the CLI identity.
func isIDEPlatform(platform string) bool {
	return strings.EqualFold(strings.TrimSpace(platform), "ide")
}

// deriveAccountDeviceID derives a stable, UUIDv4-shaped device identifier
// for one account from salt + account UID: the same account always derives
// the same value (a fixed virtual device), and two accounts never share a
// value. A gateway fronting N accounts must present each of them as its own
// device on every identity-bearing surface — a process-wide value would tie
// the whole pool to one machine (workbuddy2api-hub wb_fingerprint.py, its
// issue #448 correlation note).
//
// Shape matters as much as stability. The official clients' machineId is the
// OS hardware UUID and sessionId a per-process randomUUID — both hyphenated
// 8-4-4-4-12 with the v4 version/variant nibbles. A bare 36-hex string (what
// a naive derivation emits) is itself a gateway signature, so the version
// nibble is forced to 4 and the variant nibble to an RFC-4122 value here.
// The growth plane's per-uid growthDeriveID keeps its existing bare-hex
// shape; aligning that surface is a separate, behavior-changing step.
func deriveAccountDeviceID(sa *storedAuth, salt string) string {
	uid := ""
	if sa != nil {
		uid = sa.Account.UID
	}
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	h := hex.EncodeToString(sum[:16])
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// accountRequestID mints the per-request X-Request-ID: 32 bare hex chars
// (official capture shape). Deliberately NOT derived from the UID — a stable
// per-account request-id prefix would be a gateway signature; real clients
// mint a fresh one per request.
func accountRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
