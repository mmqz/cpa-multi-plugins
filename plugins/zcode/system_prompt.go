// system_prompt.go assembles the start-plan gateway's mandatory ZCode system
// blocks — a faithful mirror of the desktop client's ContextBuilder
// (ZCode 3.11.2 via TriDefender/zcode-api src/proxy/system-prompt.ts).
//
// The gateway does content inspection: a request whose `system` field lacks
// the ZCode identity blocks is rejected with biz 3012 "method not allowed".
// The real client's assembly has a precise shape (all symbols verified in the
// 3.11.2 bundle by the reference project):
//
//   - assembleSystemMessages groups the built sections into EXACTLY 3 wire
//     blocks, each with cache_control {type:"ephemeral"}:
//     1. cli_prefix alone ("You are ZCode, …")
//     2. every other STABLE section, "\n\n"-joined
//     3. every DYNAMIC section, "\n\n"-joined, with an explicit "\n\n"
//     prefix on the block text
//   - block 2 = Agent Identity + ZCode Desktop Context (the default desktop
//     section set; config-dependent sections like Session Guidance/Memory/
//     Output Style/skills are absent in this common client state)
//   - block 3 = Dynamic Behavior → Environment Info → Context Management;
//     the Environment lines carry REAL runtime values (cwd/platform/shell/
//     osVersion) plus the conditional
//     "- You are powered by the model named {providerId}/{modelId}." line
//   - meta_user attachments: the client ALWAYS attaches a context_prefix to
//     the first user turn — the currentDate section wrapped in
//     <system-reminder>…</system-reminder>
//
// The env values ride the same source as the identity headers (platformArch /
// osRelease) so the prompt's Platform/OS Version lines can never contradict
// X-Platform/X-Os-Version — a mixed combination is a distinguisher no real
// client produces.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

// zcodeSystemTexts holds the static section texts (sidecar asset, embedded at
// build time). Parsed once at init; a malformed asset is a build-time bug so
// failure surfaces as a panic before any traffic is served.
type zcodeSystemTexts struct {
	CLIPrefix string   `json:"cliPrefix"`
	Stable    []string `json:"stableSections"`
	Dynamic   struct {
		BeforeEnvironment string `json:"beforeEnvironment"`
		AfterEnvironment  string `json:"afterEnvironment"`
	} `json:"dynamicSections"`
	Environment struct {
		Heading        string `json:"heading"`
		InvokedLine    string `json:"invokedLine"`
		CwdLabel       string `json:"cwdLabel"`
		GitLabel       string `json:"gitLabel"`
		GitNo          string `json:"gitNo"`
		PlatformLabel  string `json:"platformLabel"`
		ShellLabel     string `json:"shellLabel"`
		OsVersionLabel string `json:"osVersionLabel"`
		PoweredByLine  string `json:"poweredByLine"`
	} `json:"environment"`
	ContextPrefix struct {
		Intro              string `json:"intro"`
		Outro              string `json:"outro"`
		CurrentDateHeading string `json:"currentDateHeading"`
		CurrentDateLine    string `json:"currentDateLine"`
	} `json:"contextPrefix"`
	SystemReminder struct {
		Open  string `json:"open"`
		Close string `json:"close"`
	} `json:"systemReminder"`
}

var zcodeSystem zcodeSystemTexts

func init() {
	if err := json.Unmarshal(zcodeSystemJSON, &zcodeSystem); err != nil {
		panic(fmt.Sprintf("zcode_system.json: %v", err))
	}
}

// systemBlock is one Anthropic text system block with its optional cache
// breakpoint marker.
type systemBlock struct {
	Type         string    `json:"type"`
	Text         string    `json:"text"`
	CacheControl *cacheCtl `json:"cache_control,omitempty"`
}

type cacheCtl struct {
	Type string `json:"type"`
}

// ephemeralCacheCtl returns a fresh {type:"ephemeral"} — always a new value so
// sharing a pointer across blocks can never alias a caller's mutation.
func ephemeralCacheCtl() *cacheCtl {
	return &cacheCtl{Type: "ephemeral"}
}

// providerModelID maps the OAuth provider onto the built-in model-provider id
// the powered-by line renders (bundle `p2`: zai→"zai-api", bigmodel→"bigmodel-api").
func providerModelID(provider string) string {
	if provider == providerBigmodel {
		return "bigmodel-api"
	}
	return "zai-api"
}

// startPlanEnvInfo carries the Environment section's runtime values
// (bundle `createNodeContextSourceAdapter` input feeding `T9o`).
type startPlanEnvInfo struct {
	Cwd       string
	Platform  string
	Shell     string
	OsVersion string
}

// resolveEnvPromptInfo mirrors resolveEnvPromptInfo (identity.ts): the values
// come from the SAME chain as the identity headers, so the prompt can never
// contradict X-Platform/X-Os-Version. osVersion keeps the
// `${platform} ${release} ${arch}` composition. shell follows the bundle
// algorithm verbatim (SHELL ?? ComSpec ?? "" → basename, else "unknown").
// cwd is the process working directory — never "unknown" in real traffic.
func resolveEnvPromptInfo() startPlanEnvInfo {
	platform := runtime.GOOS
	arch := runtime.GOARCH
	release := osRelease()
	parts := make([]string, 0, 3)
	for _, p := range []string{platform, release, arch} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	shellRaw := os.Getenv("SHELL")
	if shellRaw == "" {
		shellRaw = firstNonEmpty(os.Getenv("ComSpec"), os.Getenv("COMSPEC"))
	}
	shell := "unknown"
	if shellRaw != "" {
		if base := filepath.Base(shellRaw); base != "" && base != "." && base != string(filepath.Separator) {
			shell = base
		}
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "unknown"
	}
	return startPlanEnvInfo{Cwd: cwd, Platform: platform, Shell: shell, OsVersion: strings.Join(parts, " ")}
}

// buildEnvironmentSection renders the Environment Info section (`T9o`/`eMi`):
// lines joined with "\n", the powered-by line last when both model and
// provider are known.
func buildEnvironmentSection(env startPlanEnvInfo, currentModel, provider string) string {
	e := zcodeSystem.Environment
	lines := []string{
		e.Heading,
		e.InvokedLine,
		"- " + e.CwdLabel + ": " + env.Cwd,
		"- " + e.GitLabel + ": " + e.GitNo,
		"- " + e.PlatformLabel + ": " + env.Platform,
		"- " + e.ShellLabel + ": " + env.Shell,
		"- " + e.OsVersionLabel + ": " + env.OsVersion,
	}
	modelID := strings.TrimSpace(currentModel)
	if modelID != "" && provider != "" {
		line := strings.Replace(e.PoweredByLine, "{provider}", providerModelID(provider), 1)
		line = strings.Replace(line, "{model}", modelID, 1)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// formatLocalIsoDate renders the local YYYY-MM-DD the currentDate section
// uses (bundle `pK`).
func formatLocalIsoDate(t time.Time) string {
	return t.Format("2006-01-02")
}

// buildStartPlanSystem prepends the official ZCode gateway blocks to the
// request's system blocks, mirroring assembleSystemMessages: 3 official
// blocks (ephemeral breakpoints; the dynamic block text carries an explicit
// "\n\n" prefix), then the client's own system blocks AFTER them with their
// cache_control stripped — the official blocks already consume 3 of
// Anthropic's 4 cache breakpoints and the last one belongs to the
// last-message marker, so foreign markers would push the request over the cap.
func buildStartPlanSystem(existing []systemBlock, currentModel string, env startPlanEnvInfo, provider string) []systemBlock {
	stable := strings.Join(zcodeSystem.Stable, "\n\n")
	dynamic := strings.Join([]string{
		zcodeSystem.Dynamic.BeforeEnvironment,
		buildEnvironmentSection(env, currentModel, provider),
		zcodeSystem.Dynamic.AfterEnvironment,
	}, "\n\n")
	official := []systemBlock{
		{Type: "text", Text: zcodeSystem.CLIPrefix, CacheControl: ephemeralCacheCtl()},
		{Type: "text", Text: stable, CacheControl: ephemeralCacheCtl()},
		{Type: "text", Text: "\n\n" + dynamic, CacheControl: ephemeralCacheCtl()},
	}
	return append(official, normalizeUserSystem(existing)...)
}

// buildContextPrefixMessage renders the meta_user context_prefix (`tct` +
// `Vre` + `blt`): a leading user turn whose single text block is the
// currentDate section wrapped in <system-reminder>…</system-reminder> (no
// inner padding newlines).
func buildContextPrefixMessage(now time.Time) map[string]any {
	cp := zcodeSystem.ContextPrefix
	currentDate := cp.CurrentDateHeading + "\n" + strings.Replace(cp.CurrentDateLine, "{date}", formatLocalIsoDate(now), 1)
	body := strings.Join([]string{cp.Intro, currentDate, "", cp.Outro}, "\n")
	text := zcodeSystem.SystemReminder.Open + body + zcodeSystem.SystemReminder.Close
	return map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

// normalizeUserSystem tolerates the client's own system blocks: strings and
// {type:"text"} blocks pass through, cache_control is intentionally dropped
// (see buildStartPlanSystem), anything else is filtered out.
func normalizeUserSystem(existing []systemBlock) []systemBlock {
	out := make([]systemBlock, 0, len(existing))
	for _, b := range existing {
		text := strings.TrimSpace(b.Text)
		if b.Type == "text" && text != "" {
			out = append(out, systemBlock{Type: "text", Text: b.Text})
		}
	}
	return out
}
