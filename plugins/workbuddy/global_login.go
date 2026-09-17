// global_login.go — post-login auto-claims for Global (workbuddy.ai) accounts,
// mirroring workbuddy2api login.sh --realm=global:
//
//  1. overseas "register" activation (GET /auth/realms/copilot/overseas/user/
//     register?userId=<uid>) — idempotent; code 200 = activated, code 500 /
//     "region required" = the account still needs its registration region
//     completed (script scripts/global_region.py activate_region);
//  2. one-time trial pack claim (POST /billing/ide/trial) — idempotent,
//     upstream code 14051 means already claimed (not an error).
//
// Both are best-effort and never block the login response: they run in a
// background goroutine launched by handlePollLogin and only log their outcome
// (plus a diagnostic for region-required accounts).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// registerActivateGlobal GETs the overseas register activation endpoint.
// Returns (activated, needsRegion, msg) with the same semantics as
// workbuddy2api's scripts/global_region.py::activate_region:
//
//	code 200               → activated, needs=false
//	code 500 / region hint → activated=true but needs_region (must complete)
//	other                  → error message only
func registerActivateGlobal(sa *storedAuth) (activated bool, needsRegion bool, msg string) {
	if sa == nil || strings.TrimSpace(sa.Auth.AccessToken) == "" || strings.TrimSpace(sa.Account.UID) == "" {
		return false, false, "missing token/uid"
	}
	url := upstreamBaseGlobal + "/auth/realms/copilot/overseas/user/register?userId=" + sa.Account.UID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, false, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", sa.Account.UID)
	req.Header.Set("Origin", originRefererGlobal)
	req.Header.Set("Referer", originRefererGlobal+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return false, false, err.Error()
	}
	if resp.StatusCode >= 400 {
		return false, false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return false, false, err.Error()
	}
	switch {
	case env.Code == 200:
		return true, false, "register success"
	case env.Code == 500 || strings.Contains(strings.ToLower(env.Msg), "region required"):
		return false, true, env.Msg
	default:
		return false, false, fmt.Sprintf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
}

// performGlobalPostLogin runs right after a successful Global login. It
// activates the overseas register (idempotent) and then attempts the one-time
// trial pack claim (idempotent). Failures never propagate to the caller —
// everything is logged for the operator.
func performGlobalPostLogin(sa storedAuth) {
	label := sa.Account.Nickname
	if strings.TrimSpace(label) == "" {
		label = sa.Account.UID
	}
	activated, needs, msg := registerActivateGlobal(&sa)
	switch {
	case activated:
		log.Printf("global login %s: register activation ok", label)
	case needs:
		log.Printf("global login %s: register needs region completion (%s); trial may be skipped, complete the registration region to enable the trial pack", label, msg)
	default:
		if msg != "" {
			log.Printf("global login %s: register activation note: %s", label, msg)
		}
	}
	res, err := performTrialCall(&sa)
	if err != nil {
		log.Printf("global login %s: trial claim error: %v", label, err)
		return
	}
	log.Printf("global login %s: trial claim result: success=%v already=%v message=%s",
		label, res["success"], res["already_claimed"], res["message"])
}
