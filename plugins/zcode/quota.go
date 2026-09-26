// quota.go owns the zcode.z.ai billing plane: the live free-quota snapshot
// (GET /api/v1/zcode-plan/billing/balance) mapped onto the panel's
// creditsSummary shape. The claimable-plan preview reader (the panel's
// claim view) lives in preview.go — reading billing/preview is all it
// does; the claim POST itself is captcha-gated and stays official-client
// only.
//
// Header contract (TriDefender/zcode-api routes-quota.ts / claim/client.ts):
// the billing gateway authenticates with the plan JWT (`Authorization: Bearer
// {jwt}`) plus the full context identity set (`TV` — no X-ZCode-Agent) and a
// stable per-account `X-Device-Mid`; a missing device header makes preview
// fail with biz 3001 "parameter error" on campaign gateways.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// zcodeBalance is one balance row in the billing/balance response. Both
// snake_case and camelCase aliases are observed upstream — accept both so
// neither casing drops the field.
type zcodeBalance struct {
	ShowName       string  `json:"show_name"`
	RemainingUnits float64 `json:"remaining_units"`
	RemainingAlt   float64 `json:"remainingUnits"`
	TotalUnits     float64 `json:"total_units"`
	TotalAlt       float64 `json:"totalUnits"`
	UsedUnits      float64 `json:"used_units"`
	UsedAlt        float64 `json:"usedUnits"`
	UnitType       string  `json:"unit_type"`
	UnitTypeAlt    string  `json:"unitType"`
	ExpiresAt      float64 `json:"expires_at"`
}

func (b zcodeBalance) remaining() float64 {
	if b.RemainingUnits != 0 {
		return b.RemainingUnits
	}
	return b.RemainingAlt
}
func (b zcodeBalance) total() float64 {
	if b.TotalUnits != 0 {
		return b.TotalUnits
	}
	return b.TotalAlt
}
func (b zcodeBalance) used() float64 {
	if b.UsedUnits != 0 {
		return b.UsedUnits
	}
	return b.UsedAlt
}
func (b zcodeBalance) unitType() string {
	if b.UnitType != "" {
		return b.UnitType
	}
	return b.UnitTypeAlt
}

type balanceResponse struct {
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
	ServerTime int64  `json:"server_time"`
	Data       struct {
		Balances []zcodeBalance `json:"balances"`
	} `json:"data"`
}

// fetchQuotaSnapshot queries billing/balance with the stored JWT and folds
// the balances into a creditsSummary (one package per balance row).
func fetchQuotaSnapshot(sa *storedAuth) (*creditsSummary, error) {
	if strings.TrimSpace(sa.Auth.JWT) == "" {
		return nil, fmt.Errorf("not logged in — no plan JWT (re-run login)")
	}
	platform, arch := platformArch()
	q := url.Values{}
	q.Set("app_version", zcodeIdentity().appVersion)
	q.Set("platform", platform+"-"+arch)
	endpoint := zcodeAPIBase + "/zcode-plan/billing/balance?" + q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.JWT)
	req.Header.Set("Accept", "application/json")
	for k, v := range buildContextIdentityHeaders(zcodeIdentity(), sa.Auth.DeviceMid) {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("billing balance http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var out balanceResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("billing balance parse: %w", err)
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("billing balance: code=%d msg=%s", out.Code, out.Msg)
	}
	return summarizeBalances(out.Data.Balances), nil
}

// summarizeBalances folds balance rows into the panel's creditsSummary.
func summarizeBalances(rows []zcodeBalance) *creditsSummary {
	sum := &creditsSummary{PackCount: len(rows)}
	for i, b := range rows {
		remain, used, total := int64(b.remaining()), int64(b.used()), int64(b.total())
		sum.TotalRemain += remain
		sum.TotalUsed += used
		sum.TotalSize += total
		name := strings.TrimSpace(b.ShowName)
		if name == "" {
			name = fmt.Sprintf("额度池 %d", i+1)
		}
		if ut := strings.TrimSpace(b.unitType()); ut != "" {
			name += " (" + ut + ")"
		}
		sum.Packages = append(sum.Packages, packageSummary{
			Name: name, Remain: remain, Used: used, Size: total,
		})
	}
	return sum
}
