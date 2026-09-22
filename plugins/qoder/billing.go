// billing.go owns the upstream billing API surface: check-in status, user
// resource (credits / packages), payment type, and the perform-* call wrappers
// for daily check-in. Includes the shared JSON helpers used to tolerate the
// upstream's loosely-typed response shapes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// billingBaseOverride is nil in production; tests set it to redirect every
// sash/quota billing call (check-in, campaigns, quota) to an httptest server.
// The upstream base consts are compile-time, so this indirection is the only
// seam an HTTP-level test has.
var billingBaseOverride func(region string) string

// billingBaseFor routes billing calls by the account's region, honoring the
// test override. All sash/api check-in paths must go through this helper —
// a direct upstreamBaseFor(sa) call would bypass the test seam.
func billingBaseFor(sa *storedAuth) string {
	if billingBaseOverride != nil {
		return billingBaseOverride(authRegion(sa))
	}
	return upstreamBaseFor(sa)
}

// billingClientType/Version are the desktop client's Cosy identity headers.
// v0.8.22 (adapted from bfSan/qoder-cpa-plugin 30d6c16, live-verified 2026-09-21
// against openapi.qoder.com.cn and openapi.qoder.sh): the billing surface gates
// the campaigns response on Cosy-ClientType — the same credential that answers
// showCampaign:false to a bare request returns the live daily "100 Credits"
// campaign once the header is present. Without it the CN campaigns list can
// come back flag-less AND row-less, which the v0.12.80 CLAIMABLE-row inference
// cannot recover from: the panel shows "今日暂无可领取权益" and the day's
// benefit is silently skipped. Sending a desktop identity on every billing
// call is idempotent — where upstream does not gate, the response is unchanged.
const (
	billingClientType = "10"
	billingClientVer  = "0.3.4"
)

func billingHeaders(req *http.Request, sa *storedAuth) {
	// QoderWork billing endpoints authenticate with the active token as a
	// plain Bearer — jobToken (jt-) or device token (dt-), both accepted
	// upstream (verified live 2026-07-27). No COSY signing (KNOWLEDGE §2).
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Qoder")
	req.Header.Set("Cosy-ClientType", billingClientType)
	req.Header.Set("Cosy-Version", billingClientVer)
}

// checkinStatusResponse mirrors GET /sash/api/v1/me/daily-check-in/status
// (plain JSON, no envelope).
type checkinStatusResponse struct {
	Status             string `json:"status"` // CLAIMABLE | CLAIMED
	RewardCredits      int64  `json:"rewardCredits"`
	NextClaimAt        int64  `json:"nextClaimAt"` // s epoch
	CurrentStreakDays  int64  `json:"currentStreakDays"`
	TotalClaimDays     int64  `json:"totalClaimDays"`
	TotalRewardCredits int64  `json:"totalRewardCredits"`
	LastClaimedAt      int64  `json:"lastClaimedAt"`   // s epoch
	RewardExpiresAt    int64  `json:"rewardExpiresAt"` // s epoch
}

// fetchCheckinStatus builds the panel's check-in summary. v0.12.80: BOTH
// regions now claim through the campaigns system — upstream DISABLED the
// legacy CN daily-check-in globally (status reports DISABLED with zero
// streak; claim answers 409 on unclaimed days and grants no credits;
// verified upstream 2026-09-21, cross-checked against the qoder2api
// project's packet capture). Calling the legacy claim only produced false
// "今日已签"/failure toasts on healthy CN accounts — the field report behind
// this change.
//
// The legacy CN status endpoint stays readable, so for CN credentials we
// merge its streak stats into the campaign summary (strictly read-only,
// non-zero values only) — the panel keeps 连续/累计 display while claims ride
// campaigns. Intl has no legacy endpoint; nothing to merge there.
func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	sum, err := fetchCampaignCheckinSummary(sa)
	if err != nil {
		return nil, err
	}
	mergeLegacyCheckinStats(sa, sum)
	return sum, nil
}

// fetchLegacyCheckinStatus queries the legacy CN daily-check-in status
// endpoint. READ-ONLY since v0.12.80: its claim sibling is DISABLED upstream
// and must never be called (409 + no grant). Kept as a stats supplement —
// if upstream restores the legacy system, non-zero stats flow back in
// automatically.
func fetchLegacyCheckinStatus(sa *storedAuth) (*checkinStatusResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, billingBaseFor(sa)+"/sash/api/v1/me/daily-check-in/status", nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkin status http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var q checkinStatusResponse
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		return nil, fmt.Errorf("checkin status parse: %w", err)
	}
	return &q, nil
}

// mergeLegacyCheckinStats folds legacy CN daily-check-in stats into a
// campaign-derived summary. Only strictly greater non-zero values overwrite:
// the DISABLED legacy system reports zeros, which must never clobber the
// campaign-derived state. No-op for Intl (no legacy endpoint — probing it
// would only 404) and for nil summaries.
func mergeLegacyCheckinStats(sa *storedAuth, sum *checkinSummary) {
	if sum == nil || authRegion(sa) != regionCN {
		return
	}
	q, err := fetchLegacyCheckinStatus(sa)
	if err != nil || q == nil {
		return // best-effort: campaign summary stays authoritative
	}
	if q.CurrentStreakDays > sum.StreakDays {
		sum.StreakDays = q.CurrentStreakDays
	}
	if q.TotalClaimDays > sum.WeekCheckinDays {
		sum.WeekCheckinDays = q.TotalClaimDays
	}
	if q.TotalRewardCredits > sum.TotalCredits {
		sum.TotalCredits = q.TotalRewardCredits
	}
	if sum.DailyCredit == 0 && q.RewardCredits > 0 {
		sum.DailyCredit = q.RewardCredits
	}
}

// quotaUsageResponse mirrors GET /api/v2/quota/usage response (plain JSON,
// no envelope). Both userQuota (base credits) and addOnQuota (one-time pro
// upgrade + checkin packs) are summed for the panel.
type quotaUsageResponse struct {
	UserID               string  `json:"userId"`
	UserType             string  `json:"userType"`
	UsageType            string  `json:"usageType"`
	TotalUsagePercentage float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded      bool    `json:"isQuotaExceeded"`
	ExpiresAt            int64   `json:"expiresAt"` // ms epoch
	UpgradeURL           string  `json:"upgradeUrl"`
	UserQuota            struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
		Unit      string  `json:"unit"`
	} `json:"userQuota"`
	AddOnQuota struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
	} `json:"addOnQuota"`
	// v0.8.17 (fork libo0118/qoder-custom review): the quota response also
	// carries dedicated resource packages (check-in / campaign grants) and an
	// organization shared pool. The old two-pool sum silently DROPPED them, so
	// the panel under-reported real credits whenever an account held such a
	// package.
	DedicatedResourcePackages []quotaPool `json:"dedicatedResourcePackages"`
	OrgResourcePackage        *quotaPool  `json:"orgResourcePackage"`
}

// quotaPool is one resource pool in the quota/usage response. Field names
// mirror userQuota's shape (verified against the fork's parsed struct).
type quotaPool struct {
	Name      string  `json:"name"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

// fetchUserResource queries QoderWork's quota endpoint and aggregates base +
// add-on credits into the panel's creditsSummary shape.
func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/api/v2/quota/usage", nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("quota/usage http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var q quotaUsageResponse
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		return nil, fmt.Errorf("quota/usage parse: %w", err)
	}
	return summarizeQuotaResponse(q), nil
}

// summarizeQuotaResponse folds the parsed quota/usage response into the
// panel's creditsSummary shape. v0.8.17 (fork libo0118/qoder-custom review):
// dedicated resource packages and the org shared pool join the totals AND
// surface as their own package rows — the old two-pool sum silently dropped
// them, under-reporting real credits for accounts holding such packages.
func summarizeQuotaResponse(q quotaUsageResponse) *creditsSummary {
	sum := &creditsSummary{
		TotalRemain: int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining),
		TotalUsed:   int64(q.UserQuota.Used + q.AddOnQuota.Used),
		TotalSize:   int64(q.UserQuota.Total + q.AddOnQuota.Total),
		PackCount:   2,
		Packages: []packageSummary{
			{Name: "基础额度", Remain: int64(q.UserQuota.Remaining), Used: int64(q.UserQuota.Used), Size: int64(q.UserQuota.Total)},
			{Name: "赠送/签到额度", Remain: int64(q.AddOnQuota.Remaining), Used: int64(q.AddOnQuota.Used), Size: int64(q.AddOnQuota.Total)},
		},
	}
	appendPool := func(pool quotaPool, fallbackName string) {
		sum.TotalRemain += int64(pool.Remaining)
		sum.TotalUsed += int64(pool.Used)
		sum.TotalSize += int64(pool.Total)
		name := strings.TrimSpace(pool.Name)
		if name == "" {
			name = fallbackName
		}
		sum.Packages = append(sum.Packages, packageSummary{Name: name, Remain: int64(pool.Remaining), Used: int64(pool.Used), Size: int64(pool.Total)})
		sum.PackCount++
	}
	for i, pool := range q.DedicatedResourcePackages {
		appendPool(pool, fmt.Sprintf("专用资源包 %d", i+1))
	}
	if q.OrgResourcePackage != nil {
		appendPool(*q.OrgResourcePackage, "组织共享池")
	}
	return sum
}

// planResponse mirrors GET /api/v2/user/plan (plain JSON, no envelope).
type planResponse struct {
	UserType       string          `json:"user_type"`
	PlanTierName   string          `json:"plan_tier_name"`
	IsPersonal     bool            `json:"is_personal_version"`
	IsPaid         bool            `json:"is_paid_plan"`
	IsHighestTier  bool            `json:"is_highest_tier"`
	FeatureAllowed map[string]bool `json:"feature_allowed"`
	StartDate      int64           `json:"start_date"` // ms epoch
	EndDate        int64           `json:"end_date"`   // ms epoch
}

func fetchPaymentType(sa *storedAuth) string {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/api/v2/user/plan", nil)
	if err != nil {
		return ""
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil || resp.StatusCode >= 400 {
		return ""
	}
	var p planResponse
	if err := json.Unmarshal(resp.Body, &p); err != nil {
		return ""
	}
	// Prefer plan_tier_name (e.g. "Pro Trial") over the raw user_type string.
	if p.PlanTierName != "" {
		return p.PlanTierName
	}
	return p.UserType
}

// performCheckinCall claims one account's daily benefit. v0.12.80: both
// regions ride the campaigns claim (the only system that actually grants
// credits). The legacy CN daily-check-in/claim endpoint is DISABLED upstream
// — it answers 409 even on unclaimed days and grants nothing (2026-09-21
// upstream verification) — so calling it can only misreport "已签/失败";
// the v0.8.8 ALREADY_CLAIMED normalization here became dead weight and was
// removed with the legacy claim path. If upstream ever restores the legacy
// system, re-add the claim beside the read-only status probe.
func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	return performCampaignCheckin(sa)
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0, size>0, or packages present). Pure zero with no packages = no data.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}
