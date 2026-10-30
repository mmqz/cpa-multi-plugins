//go:build live

// dsh_file_ref_family_live_test.go — verification suite for the fourth 11-128
// family member (the DSH file-reference segment), 2026-10-29.
//
// Field report behind it: after 0.9.79 a real DSH session (50 messages, 67
// tools, ~322 KB) still answered 400 + 11128, with the two sentences
//
//	A: Tokens prefixed with @ are paths the user explicitly referenced.
//	B: Relative paths resolve from the workspace root; absolute paths …
//
// present in the system prompt. Bisecting the scrubbed system text pinned the
// trigger exactly: A and B block only when they share one field, and only in
// system/assistant. 0.9.80/0.9.81 rewrite A; this file pins that behaviour and
// records the realm asymmetry (intl does not enforce this family yet).
//
// Env switches (every test skips without them):
//
//	WB_CRED_FILE  credential json (CN region=cn or global region=global)
//	WB_BODY_FILE  captured request body, e.g. the 322 KB field-report body
//	WB_AUTH_DIR   directory of workbuddy-bridge-*.json for the realm census
//	WB_SLEEP      seconds between probes (default 6; the upstream cools down
//	              per credential for about a minute after a risk-control hit)
//
// Run: $env:CGO_ENABLED='1'; go test -tags live -timeout 30m -run DSHFileRef -v .
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	dfrSentenceA = "Tokens prefixed with @ are paths the user explicitly referenced."
	dfrSentenceB = "Relative paths resolve from the workspace root;"
	// dfrRewrittenA is what the shipped pipeline must leave behind.
	dfrRewrittenA = "Tokens prefixed with @ are paths that the user explicitly referenced."
)

func dfrSleep() time.Duration {
	if v := os.Getenv("WB_SLEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 6 * time.Second
}

// dfrErrCode extracts the upstream error code from a response body.
func dfrErrCode(s string) string {
	if m := regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?`).FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if strings.Contains(s, "11128") {
		return "11128"
	}
	return "-"
}

// dfrRestriction reports the human-readable block reason (11140 = account
// restricted, 11128 = channel risk control) so a probe result is never a bare
// status number.
func dfrRestriction(resp string) string {
	switch {
	case strings.Contains(resp, "11140"):
		return "11140 account restricted"
	case strings.Contains(resp, "11128"):
		return "11128 channel risk control"
	default:
		return "-"
	}
}

// dfrAuth builds a storedAuth from a credential file written by CPA.
func dfrAuth(t *testing.T, path string) *storedAuth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cred %s: %v", path, err)
	}
	var cred wbCredFile
	if err := json.Unmarshal(raw, &cred); err != nil {
		t.Fatalf("parse cred %s: %v", path, err)
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:   cred.Auth.AccessToken,
			RefreshToken:  cred.Auth.RefreshToken,
			Domain:        cred.Auth.Domain,
			Region:        cred.Auth.Region,
			LoginPlatform: "ide",
		},
		Account: storedAccount{
			UID:          cred.Account.UID,
			EnterpriseID: cred.Account.EnterpriseID,
			Nickname:     cred.Account.Nickname,
		},
	}
}

func dfrCredFromEnv(t *testing.T) (*storedAuth, string) {
	t.Helper()
	path := os.Getenv("WB_CRED_FILE")
	if path == "" {
		t.Skip("WB_CRED_FILE not set — live probe skipped")
	}
	sa := dfrAuth(t, path)
	return sa, path
}

// dfrPost sends a minimal chat body and returns status plus the response text.
func dfrPost(t *testing.T, sa *storedAuth, messages []map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash", "agent": "cli", "temperature": 1,
		"max_tokens": 16, "stream": true, "messages": messages,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	backendHeaders(req, sa)
	stream, status, _, err := hostHTTPDoStream(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer stream.Close()
	payload, _ := io.ReadAll(io.LimitReader(newHostStreamReader(stream), 1<<20))
	return status, string(payload)
}

// dfrUserTurn is the closing turn of every probe: cheap and deterministic.
func dfrUserTurn() map[string]any {
	return map[string]any{"role": "user", "content": "Reply with exactly: OK"}
}

func dfrSystem(text string) map[string]any {
	return map[string]any{"role": "system", "content": text}
}

// dfrCanary verifies the credential still answers before spending probes on it.
func dfrCanary(t *testing.T, sa *storedAuth, label string) bool {
	t.Helper()
	status, resp := dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."), dfrUserTurn(),
	})
	t.Logf("%s canary status=%d code=%s %s", label, status, dfrErrCode(resp), dfrRestriction(resp))
	return status == 200
}

func dfrReport(t *testing.T, label, name string, status int, resp string) {
	t.Helper()
	t.Logf("%s %-22s status=%-4d code=%-6s %s", label, name, status, dfrErrCode(resp), dfrRestriction(resp))
}

// TestLiveDSHFileRefPairRoleScope pins the role scope: the pair blocks from
// system and assistant, and must keep passing from user and tool.
func TestLiveDSHFileRefPairRoleScope(t *testing.T) {
	sa, path := dfrCredFromEnv(t)
	if !dfrCanary(t, sa, t.Name()) {
		t.Skip("credential not healthy (see canary line) — realm probe impossible")
	}
	t.Logf("cred=%s region=%s", filepath.Base(path), sa.Auth.Region)
	pair := dfrSentenceA + " " + dfrSentenceB

	status, resp := dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant. " + pair), dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_system", status, resp)
	time.Sleep(dfrSleep())

	status, resp = dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."),
		{"role": "user", "content": "Summarize the notes you were given."},
		{"role": "assistant", "content": "noted. " + pair},
		dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_assistant", status, resp)
	time.Sleep(dfrSleep())

	status, resp = dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."),
		{"role": "user", "content": "context: " + pair},
		dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_user", status, resp)
	if status != 200 {
		t.Errorf("pair in a user message must pass (measured 200 on both realms): got %d code=%s", status, dfrErrCode(resp))
	}
	time.Sleep(dfrSleep())

	status, resp = dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."),
		{"role": "user", "content": "look this up"},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": "call_dfr_1", "type": "function",
			"function": map[string]any{"name": "read", "arguments": `{"path":"README.md"}`},
		}}},
		{"role": "tool", "tool_call_id": "call_dfr_1", "content": pair},
		dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_tool", status, resp)
}

// TestLiveDSHFileRefCrossFieldNoAccumulation pins the geometry: the gate is
// per field, so A in one field plus B in another must pass.
func TestLiveDSHFileRefCrossFieldNoAccumulation(t *testing.T) {
	sa, _ := dfrCredFromEnv(t)
	if !dfrCanary(t, sa, t.Name()) {
		t.Skip("credential not healthy (see canary line) — realm probe impossible")
	}

	cases := []struct {
		name string
		msgs []map[string]any
	}{
		{"A_only_in_system", []map[string]any{dfrSystem("You are a helpful assistant. " + dfrSentenceA), dfrUserTurn()}},
		{"B_only_in_system", []map[string]any{dfrSystem("You are a helpful assistant. " + dfrSentenceB), dfrUserTurn()}},
		{"A_system_plus_B_user", []map[string]any{
			dfrSystem("You are a helpful assistant. " + dfrSentenceA),
			{"role": "user", "content": dfrSentenceB}, dfrUserTurn(),
		}},
	}
	for _, c := range cases {
		status, resp := dfrPost(t, sa, c.msgs)
		dfrReport(t, t.Name(), c.name, status, resp)
		if status != 200 {
			t.Errorf("%s must pass (per-field gate): got %d code=%s", c.name, status, dfrErrCode(resp))
		}
		time.Sleep(dfrSleep())
	}
}

// TestLiveDSHFileRefProductionRewriteWholeBody runs the shipped pipeline over a
// captured body and proves the historical text is rewritten in place, then (if
// a credential is supplied) that the body now answers 200.
func TestLiveDSHFileRefProductionRewriteWholeBody(t *testing.T) {
	bodyPath := os.Getenv("WB_BODY_FILE")
	if bodyPath == "" {
		t.Skip("WB_BODY_FILE not set — whole-body rewrite check skipped")
	}
	raw, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read body %s: %v", bodyPath, err)
	}
	sa := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "probe"}}
	out := prepareUpstreamBody(raw, nil, sa, "deepseek-v4.1-flash")
	text := string(out)
	t.Logf("body %s: %d B in → %d B out", filepath.Base(bodyPath), len(raw), len(out))

	if strings.Contains(text, dfrSentenceA) {
		t.Errorf("shipped pipeline left the blocking sentence A untouched")
	}
	if !strings.Contains(text, dfrRewrittenA) {
		t.Errorf("shipped pipeline did not produce the rewritten sentence A")
	}
	// B is deliberately untouched: a lone B never triggers.
	if !strings.Contains(text, dfrSentenceB) {
		t.Logf("note: sentence B was also rewritten in this build")
	}

	credPath := os.Getenv("WB_CRED_FILE")
	if credPath == "" {
		t.Log("WB_CRED_FILE not set — live replay of the rewritten body skipped")
		return
	}
	liveAuth := dfrAuth(t, credPath)
	if !dfrCanary(t, liveAuth, t.Name()) {
		t.Skip("credential not healthy (see canary line) — live replay impossible")
	}
	time.Sleep(dfrSleep())
	req, err := http.NewRequest(http.MethodPost, endpointChatFor(liveAuth), strings.NewReader(text))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	backendHeaders(req, liveAuth)
	stream, status, _, err := hostHTTPDoStream(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer stream.Close()
	payload, _ := io.ReadAll(io.LimitReader(newHostStreamReader(stream), 1<<20))
	t.Logf("%s replay status=%d code=%s %s", t.Name(), status, dfrErrCode(string(payload)), dfrRestriction(string(payload)))
	if status != 200 {
		t.Errorf("rewritten field-report body must answer 200: got %d code=%s", status, dfrErrCode(string(payload)))
	}
}

// TestLiveIntlDSHFileRefPair records the realm asymmetry: as of 2026-10-29 the
// international upstream does not enforce this family at all.
func TestLiveIntlDSHFileRefPair(t *testing.T) {
	sa, path := dfrCredFromEnv(t)
	if !dfrCanary(t, sa, t.Name()) {
		t.Skip("credential not healthy (see canary line) — realm probe impossible")
	}
	t.Logf("cred=%s region=%s domain=%s", filepath.Base(path), sa.Auth.Region, sa.Auth.Domain)
	pair := dfrSentenceA + " " + dfrSentenceB

	status, resp := dfrPost(t, sa, []map[string]any{dfrSystem("You are a helpful assistant. " + pair), dfrUserTurn()})
	dfrReport(t, t.Name(), "pair_in_system", status, resp)
	time.Sleep(dfrSleep())

	status, resp = dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."),
		{"role": "user", "content": "Summarize the notes you were given."},
		{"role": "assistant", "content": "noted. " + pair},
		dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_assistant", status, resp)
	time.Sleep(dfrSleep())

	status, resp = dfrPost(t, sa, []map[string]any{
		dfrSystem("You are a helpful assistant."),
		{"role": "user", "content": "context: " + pair},
		dfrUserTurn(),
	})
	dfrReport(t, t.Name(), "pair_in_user", status, resp)
	if status != 200 {
		t.Errorf("pair in a user message must pass: got %d code=%s", status, dfrErrCode(resp))
	}
}

// TestLiveIntlCredentialClassify censuses a credential directory with a canary
// request only, which separates "phrase gate rejected this" from "this account
// cannot talk to the upstream at all".
func TestLiveIntlCredentialClassify(t *testing.T) {
	dir := os.Getenv("WB_AUTH_DIR")
	if dir == "" {
		t.Skip("WB_AUTH_DIR not set — credential census skipped")
	}
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy-bridge-*.json"))
	if err != nil || len(files) == 0 {
		t.Skipf("no workbuddy-bridge-*.json under %s", dir)
	}
	var usable, restricted, unauthorized, other int
	for _, f := range files {
		sa := dfrAuth(t, f)
		status, resp := dfrPost(t, sa, []map[string]any{dfrSystem("You are a helpful assistant."), dfrUserTurn()})
		switch {
		case status == 200:
			usable++
		case status == 403 && strings.Contains(resp, "11140"):
			restricted++
		case status == 401:
			unauthorized++
		default:
			other++
		}
		t.Logf("%-12s region=%-7s status=%-4d code=%-6s %s", filepath.Base(f)[18:26], sa.Auth.Region, status, dfrErrCode(resp), dfrRestriction(resp))
		time.Sleep(dfrSleep())
	}
	t.Logf("%s total=%d usable=%d restricted11140=%d unauthorized401=%d other=%d", t.Name(), len(files), usable, restricted, unauthorized, other)
	if usable == 0 {
		t.Errorf("no usable credential in %s — the realm cannot be probed at all", dir)
	}
}
