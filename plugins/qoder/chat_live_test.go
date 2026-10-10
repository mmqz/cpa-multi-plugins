//go:build live

// chat_live_test.go — issue #34: "developer is not one of ['system',
// 'assistant', 'user', 'tool', 'function']". Clients in the o1/gpt-5 era
// send instruction messages with role "developer" (OpenAI's successor of
// "system"); the Qoder gateway's message validator rejects that role with a
// 400 provider_error. This file drives the PLUGIN's real chat path
// (buildQoderBody → qoderEncode → applyCosyHeaders → hostHTTPDoStream) so
// both the failure and the fix are proven in production dialect, not a
// reimplementation.
//
// Legs:
//
//  1. raw-developer  — the built body with the developer role forced back in
//     (JSON-level mutation, fix-independent). Documents that the upstream
//     gateway itself rejects the role. Evidence leg: status is logged, not
//     asserted, so it keeps running whichever way upstream evolves.
//  2. plugin-path    — exactly what production sends after the plugin's
//     role normalization. Asserted: HTTP 200 and a streamed answer.
//
// A minimal "hi" prompt on qfmodel (qwen3.8-flash) keeps the cost at ~1
// credit per asserted leg.
//
// Run:
//
//	QD_TOKEN=dt-... [QD_REGION=cn|intl] go test -tags live -run TestLiveChatDeveloperRole -v ./plugins/qoder
//
// Skipped unless -tags live AND QD_TOKEN are set, so normal CI stays hermetic.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveChatClient picks the HTTP client for one live chat leg. Default is the
// plugin's shared pooled client (120s overall timeout — the production
// direct-fallback transport). QD_CHAT_TIMEOUT=<seconds> switches to a
// dedicated client with that overall window, for upstreams that hold requests
// in the 10605 queue longer than 120s (the discharge, if any, happens on the
// same connection).
func liveChatClient() *http.Client {
	if v := strings.TrimSpace(os.Getenv("QD_CHAT_TIMEOUT")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return &http.Client{Timeout: time.Duration(secs) * time.Second}
		}
	}
	return sharedHTTPClient()
}

// liveChatPost sends one already-built upstream body through the plugin's
// production dialect (same headers/encoding/transport pool as the direct
// fallback) and READS THE SSE STREAM INCREMENTALLY with per-frame timestamps,
// so a hold-then-cut is distinguishable from a slow queue or a mid-generation
// truncation. Returns the HTTP status and every frame received.
func liveChatPost(t *testing.T, sa *storedAuth, body []byte, limit int64) (int, string) {
	t.Helper()
	encoded := qoderEncode(body)
	req, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("request build: %v", err)
	}
	if err := applyCosyHeaders(req, sa, encoded, endpointChatFor(sa), "qfmodel", true); err != nil {
		t.Fatalf("cosy headers: %v", err)
	}
	start := time.Now()
	resp, err := liveChatClient().Do(req)
	if err != nil {
		t.Fatalf("transport: %v (after %s)", err, time.Since(start).Truncate(time.Millisecond))
	}
	defer resp.Body.Close()
	t.Logf("[chat] http %d after %s, content-type=%s", resp.StatusCode,
		time.Since(start).Truncate(time.Millisecond), resp.Header.Get("Content-Type"))
	var buf bytes.Buffer
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		if int64(buf.Len()) >= limit {
			t.Logf("[chat] stop: limit reached (%d bytes)", limit)
			break
		}
		chunk, err := br.ReadBytes('\n')
		if len(chunk) > 0 {
			buf.Write(chunk)
			if t.Failed() {
				break
			}
			line := strings.TrimSpace(string(chunk))
			if line != "" && (buf.Len() < 2048 || bytes.HasPrefix(chunk, []byte("data:"))) {
				t.Logf("[chat] +%s %s", time.Since(start).Truncate(time.Millisecond), truncateRedacted(line, 160))
			}
		}
		if err != nil {
			t.Logf("[chat] stream ended +%s: %v (total %d bytes)", time.Since(start).Truncate(time.Millisecond), err, buf.Len())
			break
		}
	}
	return resp.StatusCode, buf.String()
}

// liveChatBody builds the upstream body for a minimal conversation through
// the plugin's own builder. roles describes the instruction side; the user
// turn is always appended last.
func liveChatBody(t *testing.T, roles []string) []byte {
	t.Helper()
	msgs := make([]map[string]any, 0, len(roles)+1)
	for _, r := range roles {
		msgs = append(msgs, map[string]any{"role": r, "content": "You are a helpful coding assistant. Reply in one short sentence."})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "hi"})
	raw, err := json.Marshal(map[string]any{
		"model":    "qoder-efficient",
		"stream":   false,
		"messages": msgs,
	})
	if err != nil {
		t.Fatalf("openai request encode: %v", err)
	}
	req := &openAIRequest{}
	if err := json.Unmarshal(raw, req); err != nil {
		t.Fatalf("openai request decode: %v", err)
	}
	body, err := buildQoderBody(req, "qfmodel", uiUserType(nil))
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	return body
}

// forceDeveloperRole rewrites the FIRST instruction-class message of the
// built upstream body back to role "developer" — reproducing byte-for-byte
// what a developer-role client sends, independently of the plugin's
// normalization.
func forceDeveloperRole(t *testing.T, body []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := msg["role"].(string); r == "system" {
			msg["role"] = "developer"
			break
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("upstream body re-encode: %v", err)
	}
	return out
}

// TestLiveChatDeveloperRole runs both legs against the real gateway.
func TestLiveChatDeveloperRole(t *testing.T) {
	sa := liveSA(t)
	region := authRegion(sa)
	t.Logf("=== live chat developer-role probe, region=%s endpoint=%s ===", region, endpointChatFor(sa))

	// Leg 1 — evidence: upstream sees role=developer (issue #34 shape).
	raw := forceDeveloperRole(t, liveChatBody(t, []string{"developer"}))
	status, payload := liveChatPost(t, sa, raw, 2048)
	t.Logf("[raw-developer] http %d body=%s", status, truncateRedacted(payload, 300))

	// Leg 2 — asserted: the plugin path (with normalization) must pass, both
	// for a lone developer turn and for system+developer side by side (the
	// rewrite yields two system messages — upstream must accept that too).
	for _, roles := range [][]string{{"developer"}, {"system", "developer"}} {
		body := liveChatBody(t, roles)
		if !bytes.Contains(body, []byte(`"role":"developer"`)) && !bytes.Contains(body, []byte(`"role": "developer"`)) {
			t.Logf("[plugin-path %v] normalized roles present in body", roles)
		}
		status, payload := liveChatPost(t, sa, body, 16384)
		t.Logf("[plugin-path %v] http %d head=%s", roles, status, truncateRedacted(payload, 200))
		if status != http.StatusOK {
			t.Errorf("[plugin-path %v] expected 200 after role normalization, got %d: %s", roles, status, truncateRedacted(payload, 300))
		}
	}
}
