//go:build live

// codex_intro_live_test.go — issue #31 pre-release gate: prove on the REAL
// gateway that (a) the Codex CLI opening signature in a system message trips
// the Tencent 11128 channel risk-control (control leg), and (b) the same
// request passed through the plugin's production sanitize pipeline
// (rewriteSystemMessagesInPlace → sanitizeBlockedTemplates) completes with
// HTTP 200 (treatment leg). This re-runs the reporter's single-variable
// isolation against the shipped code path before release.
//
// Run:
//
//	WB_JWT=... WB_UID=... go test -tags live -run TestLiveChat_CodexIntro -v ./plugins/workbuddy
//
// Skipped unless -tags live AND WB_JWT are set, so normal CI stays hermetic.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// codexIntroTrigger is the verified trigger sentence from issue #31 (Codex
// CLI's default opening system instruction, verbatim).
const codexIntroTrigger = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."

func liveCodexChatPost(t *testing.T, sa *storedAuth, raw []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	backendHeaders(req, sa)
	stream, status, _, err := hostHTTPDoStream(req)
	if err != nil {
		t.Fatalf("stream post: %v", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	payload, _ := io.ReadAll(io.LimitReader(reader, 1<<20))
	_ = reader.Close()
	return status, string(payload)
}

func TestLiveChat_CodexIntroSanitized(t *testing.T) {
	jwt := os.Getenv("WB_JWT")
	if jwt == "" {
		t.Skip("WB_JWT not set — live chat verification skipped")
	}
	uid := os.Getenv("WB_UID")
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: jwt, Domain: "www.codebuddy.ai", Region: "intl", LoginPlatform: "ide"},
		Account: storedAccount{UID: uid},
	}

	newBody := func() map[string]any {
		return map[string]any{
			"model": "deepseek-v4.1-flash",
			"messages": []map[string]any{
				{"role": "system", "content": codexIntroTrigger},
				{"role": "user", "content": "Reply with exactly: OK"},
			},
			"agent":       "cli",
			"temperature": 1,
			"stream":      true,
		}
	}

	// CONTROL — raw signature, verbatim. Issue #31 reports 100% reproduction on
	// the Tencent gateway; a 200 here would only mean this realm's gateway
	// doesn't enforce the rule today (evidence, not failure — the reported
	// channel is the CN-facing one).
	raw, _ := json.Marshal(newBody())
	status, respBody := liveCodexChatPost(t, sa, raw)
	blocked := strings.Contains(respBody, "11128")
	t.Logf("CONTROL   (raw Codex intro): HTTP %d blocked11128=%v head=%q", status, blocked, briefStreamHead(respBody))

	// TREATMENT — the exact production outbound pipeline. The body is
	// round-tripped through JSON first because production receives it as
	// decoded JSON ([]any messages), not typed Go slices.
	raw2, _ := json.Marshal(newBody())
	var obj map[string]any
	if err := json.Unmarshal(raw2, &obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !rewriteSystemMessagesInPlace(obj) {
		t.Fatal("production sanitize made no changes — pipeline not wired for system role")
	}
	san, _ := json.Marshal(obj)
	if strings.Contains(string(san), "open source project led by OpenAI") {
		t.Fatal("sanitize left the trigger phrase in the payload")
	}
	status2, respBody2 := liveCodexChatPost(t, sa, san)
	t.Logf("TREATMENT (plugin pipeline): HTTP %d head=%q", status2, briefStreamHead(respBody2))
	if status2 != http.StatusOK {
		t.Fatalf("sanitized request failed: HTTP %d %.400s", status2, respBody2)
	}
	if strings.Contains(respBody2, "11128") {
		t.Fatalf("sanitized request still risk-blocked: %.400s", respBody2)
	}
	if !strings.Contains(respBody2, `"choices"`) {
		t.Fatalf("no completion in stream: %.400s", respBody2)
	}
	t.Logf("E2E OK — sanitized Codex-intro request completed against the real gateway")
}

// briefStreamHead compresses an SSE/error body to its first meaningful line
// for compact test logs.
func briefStreamHead(s string) string {
	s = strings.TrimSpace(s)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
