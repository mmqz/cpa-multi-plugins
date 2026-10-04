// captcha_pool.go — v0.2.0: panel-minted Aliyun captcha verify params pooled
// for the two consumers that need one-shot tokens:
//   - chat on the zcode-plan anthropic gateway: the risk-control layer
//     challenges otherwise-valid calls (3007 family, measured live
//     2026-09-27 — plane.go header) and the granted weekend/activity buckets
//     live behind exactly that gate (issue #21). A challenged request is
//     retried ONCE with a pool token attached.
//   - the auto-claim scheduler (issue #23): a pool token makes a claim fully
//     automatic; without one the scheduler probes captcha-less and surfaces
//     a panel badge when the campaign demands a token.
//
// Tokens are minted by the USER'S OWN BROWSER in the management panel
// (claim.go header: the Aliyun instance is not domain-bound, verified live
// 2026-09-26) — real fingerprints, no solver, no headless DOM. The pool is
// in-memory by design: verify params are one-shot server-side and short-
// lived, so persisting them would only stockpile dead weight. The panel's
// refill widget mints into the pool while it is open; a token drained by a
// challenge/claim is consumed once and gone.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	captchaPoolCapacity = 8
	// Verify params are minted minutes before use; anything older than two
	// hours is treated as stale rather than spent on a live request.
	captchaPoolTokenTTL = 2 * time.Hour
)

type captchaPoolEntry struct {
	param    string
	region   string
	mintedAt time.Time
}

var captchaPoolState struct {
	sync.Mutex
	entries []captchaPoolEntry // FIFO — oldest first, taken first
}

// captchaPoolPut adds one minted verify param. Duplicate params (double
// submits of the same mint) and empties are ignored. Returns false when the
// entry was not stored.
func captchaPoolPut(param, region string) bool {
	param = strings.TrimSpace(param)
	if param == "" {
		return false
	}
	captchaPoolState.Lock()
	defer captchaPoolState.Unlock()
	for _, e := range captchaPoolState.entries {
		if e.param == param {
			return false
		}
	}
	if len(captchaPoolState.entries) >= captchaPoolCapacity {
		captchaPoolState.entries = captchaPoolState.entries[1:]
	}
	captchaPoolState.entries = append(captchaPoolState.entries, captchaPoolEntry{
		param:    param,
		region:   strings.TrimSpace(region),
		mintedAt: time.Now(),
	})
	return true
}

// captchaPoolTake pops the oldest non-stale token (FIFO). Expired entries
// are dropped on the way. Returns empty strings when the pool is dry.
func captchaPoolTake() (param, region string) {
	captchaPoolState.Lock()
	defer captchaPoolState.Unlock()
	for len(captchaPoolState.entries) > 0 {
		e := captchaPoolState.entries[0]
		captchaPoolState.entries = captchaPoolState.entries[1:]
		if time.Since(e.mintedAt) <= captchaPoolTokenTTL {
			return e.param, e.region
		}
	}
	return "", ""
}

// captchaPoolSnapshot reports (fresh, capacity, newestMint) for the panel.
func captchaPoolSnapshot() (int, int, time.Time) {
	captchaPoolState.Lock()
	defer captchaPoolState.Unlock()
	fresh := 0
	var newest time.Time
	for _, e := range captchaPoolState.entries {
		if time.Since(e.mintedAt) <= captchaPoolTokenTTL {
			fresh++
		}
		if e.mintedAt.After(newest) {
			newest = e.mintedAt
		}
	}
	return fresh, captchaPoolCapacity, newest
}

// captchaRetryRoute builds the one-shot captcha retry for a challenged
// zcode-plan anthropic call: same endpoint, same (already-translated) body,
// headers re-applied with a pool token attached on top. ok=false when the
// failure is not a captcha challenge or the pool is dry — the caller then
// keeps its existing failure handling.
func captchaRetryRoute(status int, failBody string, hdrs http.Header) (chatRoute, bool) {
	hdrValue := ""
	if hdrs != nil {
		hdrValue = strings.TrimSpace(hdrs.Get("x-aliyun-captcha-verify-param"))
	}
	if !startPlanCaptchaChallenge(status, hdrValue, failBody) {
		return chatRoute{}, false
	}
	param, region := captchaPoolTake()
	if param == "" {
		return chatRoute{}, false
	}
	base := startPlaneFallbackRoute()
	return chatRoute{
		endpoint:  base.endpoint,
		anthropic: true,
		applyHeaders: func(req *http.Request, sa *storedAuth, body string) {
			base.applyHeaders(req, sa, body)
			req.Header.Set("X-Aliyun-Captcha-Verify-Param", param)
			if region != "" {
				req.Header.Set("X-Aliyun-Captcha-Verify-Region", region)
			}
		},
	}, true
}

// handleCaptchaPoolGet reports the pool state to the panel widget.
func handleCaptchaPoolGet() map[string]any {
	fresh, capacity, newest := captchaPoolSnapshot()
	out := map[string]any{
		"ok":       true,
		"size":     fresh,
		"capacity": capacity,
	}
	if !newest.IsZero() {
		out["newest_mint"] = newest.UTC().Format(time.RFC3339)
	}
	return out
}

// handleCaptchaPoolPut stores one panel-minted verify param.
func handleCaptchaPoolPut(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Captcha string `json:"captcha"`
		Region  string `json:"region"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"ok": false, "error": "payload parse: " + err.Error()}
	}
	if !captchaPoolPut(body.Captcha, body.Region) {
		return map[string]any{"ok": false, "error": "empty or duplicate verify param"}
	}
	fresh, capacity, _ := captchaPoolSnapshot()
	return map[string]any{"ok": true, "size": fresh, "capacity": capacity}
}
