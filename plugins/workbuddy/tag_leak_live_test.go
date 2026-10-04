//go:build live

// tag_leak_live_test.go — issue #30 mechanism probe + release gate.
//
// Issue #30 (Misaka09982): answers begin with raw <thought>/<analysis>/
// <summary> blocks on deepseek-v4.1-flash (CN). Root cause: the reasoning
// replay folds historical reasoning into assistant content as <thought>
// blocks, and the model imitates the taught shape. This file verifies both
// halves of that claim against the real gateway, and gates the release:
//
//	CONTROL    — multi-turn history WITHOUT thought folds: logs whether the
//	             model ever emits taught tags on its own.
//	EXPERIMENT — the exact fold format injectReasoningInPlace produces in
//	             history: logs how often the model imitates it (probabilistic
//	             — evidence, never a failure).
//	GATE       — deterministic: the production pump path (pumpStreamFrames,
//	             i.e. what clients actually receive) must emit content with NO
//	             leading taught-tag run, whatever the raw upstream did. A
//	             failure here means the scrubber is not wired, not that the
//	             model misbehaved.
//
// Realm caveat: the stored credential is Intl (www.codebuddy.ai); the
// reporter's account is CN. The #31 probe already showed both gateways share
// gateway-side behavior, and the contamination is model-side (same
// deepseek-v4.1-flash), so imitation is expected on either realm.
//
// Run:
//
//	WB_JWT=... WB_UID=... go test -tags live -run TestLiveChat_TagLeak -v ./plugins/workbuddy
//
// Skipped unless -tags live AND WB_JWT are set, so normal CI stays hermetic.
package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// taughtTags mirrors scrubTagNames (kept literal here so the probe fails
// loudly if the production tag set ever drifts silently).
var taughtTags = []string{"<thought>", "<analysis>", "<summary>", "<think>"}

// rawStreamContent concats delta.content across a raw upstream SSE body.
func rawStreamContent(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

// scrubbedStreamContent pushes the raw body through the production pump and
// returns exactly what a client would receive as content.
func scrubbedStreamContent(t *testing.T, body string) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	rec := &streamRecorder{}
	if _, handled := pumpStreamFrames(sc, rec, nil, false, &sseUsageCollector{}, "m", "m", "uid-live", time.Now(), nil); handled {
		t.Fatalf("production pump aborted the stream")
	}
	chunks, _ := rec.snapshot()
	var b strings.Builder
	for _, c := range chunks {
		var chunk map[string]any
		if json.Unmarshal([]byte(c), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, ch := range choices {
			choice, _ := ch.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

func hasTaughtTag(s string) bool {
	for _, tag := range taughtTags {
		if strings.Contains(s, tag) {
			return true
		}
	}
	return false
}

func head(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func TestLiveChat_TagLeak(t *testing.T) {
	jwt := os.Getenv("WB_JWT")
	if jwt == "" {
		t.Skip("WB_JWT not set — live tag-leak probe skipped")
	}
	uid := os.Getenv("WB_UID")
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: jwt, Domain: "www.codebuddy.ai", Region: "intl", LoginPlatform: "ide"},
		Account: storedAccount{UID: uid},
	}

	// Long-task shape: the reporter sees the leak inside LONG Pi sessions, so
	// the probe replays eight folded turns (every prior answer beginning with
	// the taught block) and ends on a question that invites reasoning — the
	// highest prior for the model to open its answer with a thought block.
	qa := [][3]string{
		{"What is the capital of France?", "Paris.", "Simple factual recall: the capital of France is Paris."},
		{"What is 7 times 8?", "56.", "Arithmetic: seven times eight is 56."},
		{"Name the largest planet in the solar system.", "Jupiter.", "Recall: Jupiter is the largest planet."},
		{"What year did the Berlin Wall fall?", "1989.", "Historical recall: the wall fell in 1989."},
		{"How many continents are there?", "Seven.", "Geography recall: seven continents."},
		{"What is the chemical symbol for gold?", "Au.", "Chemistry recall: gold is Au."},
		{"Who wrote Romeo and Juliet?", "William Shakespeare.", "Literature recall: Shakespeare wrote it."},
		{"What is the square root of 144?", "12.", "Arithmetic: 12 squared is 144."},
	}
	newMessages := func(folded bool) []map[string]any {
		msgs := []map[string]any{{"role": "system", "content": "You are a helpful assistant."}}
		for _, turn := range qa {
			msgs = append(msgs, map[string]any{"role": "user", "content": turn[0]})
			answer := turn[1]
			if folded {
				answer = "<thought>\n" + turn[2] + "\n</thought>\n\n" + answer
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": answer})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": "A bat and a ball cost 1.10 in total. The bat costs 1.00 more than the ball. How much does the ball cost? Think carefully and answer briefly."})
		return msgs
	}
	newBody := func(folded bool) map[string]any {
		return map[string]any{
			"model":       "deepseek-v4.1-flash",
			"messages":    newMessages(folded),
			"agent":       "cli",
			"temperature": 1,
			"stream":      true,
		}
	}

	// CONTROL — history without folds.
	raw, _ := json.Marshal(newBody(false))
	status, respBody := liveCodexChatPost(t, sa, raw)
	if status != http.StatusOK {
		t.Fatalf("control leg: HTTP %d %.300s", status, respBody)
	}
	controlContent := rawStreamContent(t, respBody)
	t.Logf("CONTROL    raw head=%q taughtTags=%v", head(controlContent, 160), hasTaughtTag(controlContent))

	// EXPERIMENT — the taught fold format in history (mirrors
	// injectReasoningInPlace byte-for-byte). Imitation is probabilistic:
	// log every sample; the aggregate count is the evidence for the issue.
	imitated := 0
	const samples = 5
	for i := 0; i < samples; i++ {
		raw, _ := json.Marshal(newBody(true))
		status, respBody := liveCodexChatPost(t, sa, raw)
		if status != http.StatusOK {
			t.Logf("EXPERIMENT[%d] HTTP %d — skipped sample", i, status)
			continue
		}
		rawContent := rawStreamContent(t, respBody)
		clean := !hasTaughtTag(rawContent)
		if !clean {
			imitated++
		}
		t.Logf("EXPERIMENT[%d] raw head=%q imitated=%v", i, head(rawContent, 160), !clean)

		// GATE — deterministic: the production pump output must be clean
		// regardless of what the raw upstream emitted.
		clientSeen := scrubbedStreamContent(t, respBody)
		if got := scrubTagBlocksOneShot(clientSeen); got != clientSeen {
			t.Fatalf("GATE: production stream leaked a taught tag run: %q", head(clientSeen, 200))
		}
		if strings.TrimSpace(clientSeen) == "" {
			t.Fatalf("GATE: production pump emptied the answer (raw head=%q)", head(rawContent, 200))
		}
	}
	t.Logf("SUMMARY    imitation %d/%d experiment samples; control taughtTags=%v", imitated, samples, hasTaughtTag(controlContent))
}
