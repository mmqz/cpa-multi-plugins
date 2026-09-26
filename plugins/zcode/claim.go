// claim.go implements the claim POST half of the manual-claim ("weekend
// plan") subsystem — the panel-hosted captcha route. Since 0.1.6 the Aliyun
// captcha challenge runs inside the management panel page itself (the user's
// real browser): AliyunCaptcha.js initializes against the zcode captcha
// instance (SceneId 11xygtvd / region sgp / prefix no8xfe — the reference
// solver's solveTraceless defaults, TriDefender/zcode-api captcha-happy.ts),
// startTracelessVerification attempts an invisible pass and falls back to an
// interactive slider, and the resulting verify param arrives here as the
// captcha argument of the management claim call. Origin feasibility was
// verified live 2026-09-26 from a foreign origin (http://127.0.0.1): the
// instance config call, the pe/FeiLin dynamic bundles, the fingerprint
// upload and the no8xfe-verify round-trip all answer HTTP 200 — the captcha
// instance is not domain-bound to zcode.z.ai, so the user's own browser
// mints genuine verify params without the official client. The plugin never
// sees the captcha session; it only forwards the one-shot param.
//
// Wire contract (src/claim/client.ts, ZCode 3.12.3 desktop bundle + the 0918
// campaign deviation): POST /api/v1/zcode-plan/billing/claim body
// {"plan_id": "..."} with the MINIMAL header set — Authorization,
// Content-Type, X-Aliyun-Captcha-Verify-Param,
// [X-Aliyun-Captcha-Verify-Region], X-ZCode-App-Version, X-Platform,
// X-Device-Mid (campaign-gated; the TV identity set is deliberately NOT
// sent). Success = 2xx + biz code 0 + data.plan. Failure biz codes mirror
// the desktop classifyClaimCode: 1001 not_found, 1002 unavailable, 1003
// already_claimed, 1004 ineligible, 1005 quota_exhausted, 3001
// invalid_request, 3007 captcha (param rejected — retry with a fresh one),
// 401 login_required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// claimRequest is the panel's management claim body.
type claimRequest struct {
	AuthIndex string `json:"auth_index"`
	PlanID    string `json:"plan_id"`
	Captcha   string `json:"captcha"`
	Region    string `json:"captcha_region,omitempty"`
}

// claimOutcome is the normalized upstream result surfaced to the panel.
type claimOutcome struct {
	OK       bool   `json:"ok"`
	PlanID   string `json:"plan_id,omitempty"`
	StartsAt int64  `json:"starts_at,omitempty"`
	EndsAt   int64  `json:"ends_at,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// classifyClaimCode mirrors the desktop mapper (src/claim/types.ts).
func classifyClaimCode(code int64) string {
	switch code {
	case 1001:
		return "not_found"
	case 1002:
		return "unavailable"
	case 1003:
		return "already_claimed"
	case 1004:
		return "ineligible"
	case 1005:
		return "quota_exhausted"
	case 3001:
		return "invalid_request"
	case 3007:
		return "captcha"
	case 401:
		return "login_required"
	default:
		return "unknown"
	}
}

// claimBizCode tolerantly extracts the upstream biz code: top-level number,
// numeric string, or numeric json.Number. Returns ok=false when absent.
func claimBizCode(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// rawClaimResponse mirrors the upstream envelope (biz code + optional plan).
type rawClaimResponse struct {
	Code    any    `json:"code"`
	Msg     string `json:"msg"`
	Message string `json:"message"`
	Data    struct {
		Plan *struct {
			PlanID   string   `json:"plan_id"`
			StartsAt *float64 `json:"starts_at"`
			EndsAt   *float64 `json:"ends_at"`
		} `json:"plan"`
	} `json:"data"`
}

// fetchClaimPlan submits one claim for one account with the user-minted
// captcha verify param. The header set is the claim plane's minimal bundle,
// NOT the TV identity set (see the file header).
func fetchClaimPlan(sa *storedAuth, planID, captchaParam, region string) claimOutcome {
	out := claimOutcome{PlanID: planID}
	if sa == nil || strings.TrimSpace(sa.Auth.JWT) == "" {
		out.Kind = "login_required"
		out.Message = "account has no plan JWT — re-run login"
		return out
	}
	if strings.TrimSpace(captchaParam) == "" {
		out.Kind = "captcha"
		out.Message = "missing captcha verify param"
		return out
	}
	platform, arch := platformArch()
	headers := map[string]string{
		"Authorization":                 "Bearer " + sa.Auth.JWT,
		"Content-Type":                  "application/json",
		"X-Aliyun-Captcha-Verify-Param": captchaParam,
		"X-ZCode-App-Version":           zcodeIdentity().appVersion,
		"X-Platform":                    platform + "-" + arch,
	}
	if strings.TrimSpace(region) != "" {
		headers["X-Aliyun-Captcha-Verify-Region"] = region
	}
	if mid := strings.TrimSpace(sa.Auth.DeviceMid); mid != "" {
		headers["X-Device-Mid"] = mid // campaign-gated deviation (0828 + 0918)
	}
	payload, _ := json.Marshal(map[string]string{"plan_id": planID})
	endpoint := zcodeAPIBase + "/zcode-plan/billing/claim"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		out.Kind = "http_error"
		out.Message = err.Error()
		return out
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		out.Kind = "http_error"
		out.Message = err.Error()
		return out
	}
	var body rawClaimResponse
	_ = json.Unmarshal(resp.Body, &body) // tolerant: HTTP status decides the fallback
	code, hasCode := claimBizCode(body.Code)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && hasCode && code == 0 && body.Data.Plan != nil {
		out.OK = true
		if body.Data.Plan.StartsAt != nil && *body.Data.Plan.StartsAt == *body.Data.Plan.StartsAt {
			out.StartsAt = int64(*body.Data.Plan.StartsAt)
		}
		if body.Data.Plan.EndsAt != nil && *body.Data.Plan.EndsAt == *body.Data.Plan.EndsAt {
			out.EndsAt = int64(*body.Data.Plan.EndsAt)
		}
		out.Message = firstNonEmpty(strings.TrimSpace(body.Msg), "claimed")
		return out
	}
	if hasCode {
		out.Code = strconv.FormatInt(code, 10)
	}
	if resp.StatusCode >= 400 && !hasCode {
		out.Kind = "http_error"
		if resp.StatusCode == http.StatusUnauthorized {
			out.Kind = "login_required"
		}
		out.Message = fmt.Sprintf("http %d %s", resp.StatusCode, truncateRedacted(string(resp.Body), 160))
		return out
	}
	out.Kind = "unknown"
	if hasCode {
		out.Kind = classifyClaimCode(code)
	}
	out.Message = firstNonEmpty(strings.TrimSpace(body.Msg), strings.TrimSpace(body.Message), fmt.Sprintf("biz code %v", body.Code))
	return out
}

// handleClaim serves POST {mgmt}/claim: resolve the requested account, then
// submit the claim with the panel-minted captcha param.
func handleClaim(req pluginapi.ManagementRequest) map[string]any {
	var body claimRequest
	_ = json.Unmarshal(req.Body, &body)
	body.PlanID = strings.TrimSpace(body.PlanID)
	body.Captcha = strings.TrimSpace(body.Captcha)
	body.AuthIndex = strings.TrimSpace(body.AuthIndex)
	if body.PlanID == "" || body.Captcha == "" {
		return map[string]any{"ok": false, "error": "plan_id and captcha are required"}
	}
	if body.AuthIndex == "" {
		return map[string]any{"ok": false, "error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	var found pluginapi.HostAuthFileEntry
	var target *pluginapi.HostAuthFileEntry
	for _, f := range files {
		if f.AuthIndex == body.AuthIndex {
			found = f
			target = &found
			break
		}
	}
	if target == nil {
		return map[string]any{"ok": false, "error": "auth_index " + body.AuthIndex + " not found"}
	}
	sa, _, err := hostAuthGetBundle(target.AuthIndex)
	if err != nil {
		return map[string]any{"ok": false, "error": "load auth: " + err.Error()}
	}
	outcome := fetchClaimPlan(sa, body.PlanID, body.Captcha, body.Region)
	result := map[string]any{
		"ok":      outcome.OK,
		"kind":    outcome.Kind,
		"code":    outcome.Code,
		"message": outcome.Message,
		"plan_id": outcome.PlanID,
	}
	if outcome.StartsAt != 0 {
		result["starts_at"] = outcome.StartsAt
	}
	if outcome.EndsAt != 0 {
		result["ends_at"] = outcome.EndsAt
	}
	return result
}
