package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The 2026-09-20 voucher/task outage chain, pinned as regression guards.
//
// History: 0.9.13 declared /tasks + /tasks/run twice (host dedupes silently)
// and 0.9.14 swapped their handler cases for /school/vouchers without adding
// a declaration. The host dispatches plugin management routes by EXACT key
// lookup over the declared table (pluginhost.ServeManagementHTTP), so:
//
//   - an undeclared but handled path (GET /school/vouchers) never reaches the
//     plugin — the host gin NoRoute answers 404 with an EMPTY body, which the
//     panel surfaced as "JSON.parse: unexpected end of data at line 1
//     column 1" and later as "管理桥接响应异常（HTTP 404）：响应体为空";
//   - a declared but unhandled path (POST /tasks/run during 0.9.14..0.9.17)
//     reaches the plugin and falls through to the "not found" 404.
//
// Both drift directions are silent and user-visible, so both are tested:
// table integrity (no dups, handler surface fully declared) plus a dispatch
// smoke proving every declared route lands on a real handler.
func TestManagementRoutesTableIntegrity(t *testing.T) {
	reg := managementRegistration()
	if len(reg.Routes) == 0 {
		t.Fatal("no management routes declared")
	}
	declared := map[string]int{}
	for _, r := range reg.Routes {
		declared[r.Method+" "+r.Path]++
	}
	for key, n := range declared {
		if n > 1 {
			t.Errorf("management route %s declared %d times (host dedupes silently; keep the table 1:1)", key, n)
		}
	}
	base := "/plugins/" + providerName
	want := []struct{ method, suffix string }{
		{http.MethodGet, "/accounts"},
		{http.MethodPost, "/refresh"},
		{http.MethodPost, "/checkin"},
		{http.MethodPost, "/checkin/config"},
		{http.MethodGet, "/tasks"},
		{http.MethodPost, "/tasks/run"},
		// v0.12.67: the voucher dialog lived undeclared since 0.9.14 — the
		// host answered 404 with an empty body before the plugin ever ran.
		{http.MethodGet, "/school/vouchers"},
		{http.MethodGet, "/credits"},
		{http.MethodPost, "/import"},
		{http.MethodPost, "/trial"},
		{http.MethodPost, "/select"},
		{http.MethodPost, "/keepalive"},
		{http.MethodGet, "/keepalive/status"},
	}
	for _, w := range want {
		key := w.method + " " + base + w.suffix
		if declared[key] != 1 {
			t.Errorf("handled route %s missing from declared Routes table (host 404s it with an empty body)", key)
		}
	}
}

func TestManagementRoutesDispatchSmoke(t *testing.T) {
	// Reset the plugin-layer rate limiter: the smoke dispatches several POST
	// routes back-to-back and the shared "_global" bucket would otherwise
	// return 429 for the tail (an acceptable non-404, but noisy).
	mgmtRateLimitMu.Lock()
	mgmtRateLimit = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu.Unlock()
	t.Cleanup(func() {
		mgmtRateLimitMu.Lock()
		mgmtRateLimit = map[string]*mgmtRateEntry{}
		mgmtRateLimitMu.Unlock()
	})

	reg := managementRegistration()
	for _, r := range reg.Routes {
		full := "/v0/management" + r.Path
		req, err := json.Marshal(pluginapi.ManagementRequest{
			Method:  r.Method,
			Path:    full,
			Headers: http.Header{},
			Body:    []byte("{}"),
		})
		if err != nil {
			t.Fatalf("marshal %s: %v", full, err)
		}
		out, err := handleManagement(req)
		if err != nil {
			t.Fatalf("%s %s: handleManagement error: %v", r.Method, full, err)
		}
		var env envelope
		if err := json.Unmarshal(out, &env); err != nil || !env.OK {
			t.Fatalf("%s %s: bad envelope (err=%v)", r.Method, full, err)
		}
		var resp pluginapi.ManagementResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatalf("%s %s: bad response payload: %v", r.Method, full, err)
		}
		if resp.StatusCode == 0 {
			resp.StatusCode = http.StatusOK
		}
		// The plugin's own fallthrough 404 is the declared-but-unhandled drift.
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(resp.Body), "not found: ") {
			t.Errorf("%s %s: declared route fell through to the not-found handler (body=%s)", r.Method, full, string(resp.Body))
		}
	}
}

// Guard the shared limiter contract the smoke test relies on: POST requests
// consume tokens, GET-only paths do not.
func TestManagementRateLimitPostOnly(t *testing.T) {
	mgmtRateLimitMu.Lock()
	mgmtRateLimit = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu.Unlock()
	t.Cleanup(func() {
		mgmtRateLimitMu.Lock()
		mgmtRateLimit = map[string]*mgmtRateEntry{}
		mgmtRateLimitMu.Unlock()
	})

	base := loadedManagementBasePath() + "/plugins/" + providerName
	if !allowManagementRequest("t1") {
		t.Fatal("first POST should pass the fresh bucket")
	}
	// GET /accounts is not a mutating path and must not consume tokens even
	// when the method check is bypassed by an explicit GET.
	if mutatingManagementPath(base + "/accounts") {
		t.Error("GET /accounts must not be treated as mutating")
	}
	if !mutatingManagementPath(base + "/checkin") {
		t.Error("POST /checkin must be treated as mutating")
	}
	_ = time.Now() // keep time import aligned with future assertions
}
