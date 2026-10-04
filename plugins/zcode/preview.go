// preview.go reads the claimable-plan preview plane (billing/preview) — the
// discovery half of the manual-claim ("weekend plan") subsystem mirrored from
// ZCode 3.10-3.12 desktop clients (TriDefender/zcode-api src/claim/*).
//
// The claim POST itself lives in claim.go (0.1.6+): the captcha challenge
// runs inside the panel page — the user's own browser mints the Aliyun verify
// param (the instance is not domain-bound; verified 2026-09-26, see the
// claim.go header) — so no official client and no headless solver are needed.
// After a successful claim the granted pack shows up as a balance row via the
// regular billing/balance plane (quota.go).
//
// Wire contract (src/claim/client.ts, empirically verified against the 0828
// and wk-0918 campaign gateways): GET /api/v1/zcode-plan/billing/preview with
// Authorization: Bearer {jwt} + the TV identity set including a stable
// X-Device-Mid — the campaign gateway rejects preview with biz 3001
// "parameter error" when the device header is missing. HTTP 404 means no
// campaign is deployed (empty list, not an error). Server biz codes mirror
// the desktop mapper: 1003 already claimed, 1004 ineligible, 1005 daily
// quota exhausted, 3007 captcha challenge.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// previewEntitlement is one grant inside a claimable plan (normalized from
// snake_case upstream; camelCase aliases tolerated like balance rows).
type previewEntitlement struct {
	Name         string   `json:"name"`
	Grant        int64    `json:"grant"`
	Unit         string   `json:"unit,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	EffectiveAt  int64    `json:"effective_at,omitempty"`
}

// previewPlan is one claimable plan entry surfaced on the panel.
type previewPlan struct {
	PlanID       string               `json:"plan_id"`
	Name         string               `json:"name"`
	Description  string               `json:"description,omitempty"`
	Priority     int                  `json:"priority,omitempty"`
	StartsAt     int64                `json:"starts_at,omitempty"`
	EndsAt       int64                `json:"ends_at,omitempty"`
	Entitlements []previewEntitlement `json:"entitlements"`
	Accounts     int                  `json:"accounts,omitempty"` // deduped view: how many accounts see it
}

// rawPreview* mirror the upstream wire shapes (snake_case primary, camelCase
// aliases observed live on the balance plane — accept both so neither casing
// drops a field).
type rawPreviewEntitlement struct {
	EntitlementID  string   `json:"entitlement_id"`
	EntitlementAlt string   `json:"entitlementId"`
	ShowName       string   `json:"show_name"`
	ShowNameAlt    string   `json:"showName"`
	GrantUnits     *float64 `json:"grant_units"`
	GrantAlt       *float64 `json:"grantUnits"`
	UnitType       string   `json:"unit_type"`
	UnitTypeAlt    string   `json:"unitType"`
	Capabilities   []string `json:"capabilities"`
	Period         string   `json:"period"`
	EffectiveAt    *float64 `json:"effective_at"`
	EffectiveAlt   *float64 `json:"effectiveAt"`
}

func (r rawPreviewEntitlement) id() string {
	return strings.TrimSpace(r.EntitlementID)
}

func (r rawPreviewEntitlement) name() string {
	if s := strings.TrimSpace(r.ShowName); s != "" {
		return s
	}
	return strings.TrimSpace(r.ShowNameAlt)
}

func (r rawPreviewEntitlement) unit() string {
	if s := strings.TrimSpace(r.UnitType); s != "" {
		return s
	}
	return strings.TrimSpace(r.UnitTypeAlt)
}

type rawPreviewPlan struct {
	PlanID      string                  `json:"plan_id"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Priority    *float64                `json:"priority"`
	Entitlement []rawPreviewEntitlement `json:"entitlements"`
	StartsAt    *float64                `json:"starts_at"`
	EndsAt      *float64                `json:"ends_at"`
}

func (r rawPreviewPlan) normalize() *previewPlan {
	id := strings.TrimSpace(r.PlanID)
	if id == "" {
		return nil
	}
	plan := &previewPlan{
		PlanID:       id,
		Name:         strings.TrimSpace(r.Name),
		Description:  strings.TrimSpace(r.Description),
		Entitlements: []previewEntitlement{},
	}
	if r.Priority != nil && *r.Priority == *r.Priority { // finite check (NaN != NaN)
		plan.Priority = int(*r.Priority)
	}
	for _, e := range r.Entitlement {
		if e.id() == "" {
			continue // mirror the desktop parser: no entitlement_id → skip
		}
		ent := previewEntitlement{Name: e.name(), Unit: e.unit()}
		if e.GrantUnits != nil && *e.GrantUnits == *e.GrantUnits {
			ent.Grant = int64(*e.GrantUnits)
		} else if e.GrantAlt != nil && *e.GrantAlt == *e.GrantAlt {
			ent.Grant = int64(*e.GrantAlt)
		}
		if len(e.Capabilities) > 0 {
			ent.Capabilities = e.Capabilities
		}
		eff := e.EffectiveAt
		if eff == nil {
			eff = e.EffectiveAlt
		}
		if eff != nil && *eff == *eff {
			ent.EffectiveAt = int64(*eff)
		}
		plan.Entitlements = append(plan.Entitlements, ent)
	}
	if r.StartsAt != nil && *r.StartsAt == *r.StartsAt {
		plan.StartsAt = int64(*r.StartsAt)
	}
	if r.EndsAt != nil && *r.EndsAt == *r.EndsAt {
		plan.EndsAt = int64(*r.EndsAt)
	}
	return plan
}

type previewResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Plans []rawPreviewPlan `json:"plans"`
	} `json:"data"`
}

// fetchClaimablePreview queries billing/preview for one account and returns
// the claimable plans. An account without JWT is not previewable (claim
// requires login; preview with the account identity keeps the campaign
// gateway's X-Device-Mid gate happy).
func fetchClaimablePreview(sa *storedAuth) ([]previewPlan, error) {
	if sa == nil || strings.TrimSpace(sa.Auth.JWT) == "" {
		return nil, fmt.Errorf("not logged in — no plan JWT (re-run login)")
	}
	platform, arch := platformArch()
	q := url.Values{}
	q.Set("app_version", zcodeIdentity().appVersion)
	q.Set("platform", platform+"-"+arch)
	endpoint := zcodeAPIBase + "/zcode-plan/billing/preview?" + q.Encode()
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
	// 404 = campaign not deployed (client.ts treats it as the empty case).
	if resp.StatusCode == http.StatusNotFound {
		return []previewPlan{}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("billing preview http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var out previewResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("billing preview parse: %w", err)
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("billing preview: code=%d msg=%s", out.Code, out.Msg)
	}
	plans := make([]previewPlan, 0, len(out.Data.Plans))
	for _, raw := range out.Data.Plans {
		if p := raw.normalize(); p != nil {
			plans = append(plans, *p)
		}
	}
	return plans, nil
}

// accountPreview is one account's preview outcome for the aggregator.
type accountPreview struct {
	Account string
	Plans   []previewPlan
	Err     error
	Skipped bool // no JWT — nothing to preview
}

// mergePreviewResults dedupes per-account preview results by plan_id (the
// same campaign is visible to every eligible account; zai and bigmodel may
// surface different sets). First sighting wins the metadata; the counter
// records how many accounts see each plan. Errors are surfaced per account.
func mergePreviewResults(results []accountPreview) ([]previewPlan, []map[string]any, int) {
	order := []string{}
	byID := map[string]*previewPlan{}
	var errs []map[string]any
	checked := 0
	for _, res := range results {
		if res.Skipped {
			continue
		}
		checked++
		if res.Err != nil {
			errs = append(errs, map[string]any{"account": res.Account, "error": res.Err.Error()})
			continue
		}
		for _, p := range res.Plans {
			if existing, ok := byID[p.PlanID]; ok {
				existing.Accounts++
				continue
			}
			cp := p
			cp.Accounts = 1
			byID[p.PlanID] = &cp
			order = append(order, p.PlanID)
		}
	}
	plans := make([]previewPlan, 0, len(order))
	for _, id := range order {
		plans = append(plans, *byID[id])
	}
	return plans, errs, checked
}

// handlePreviewList serves GET {mgmt}/preview: fan the preview query out to
// every account in parallel and merge. Read-only upstream; the claim POST is
// a separate management route (POST /claim — claim.go).
func handlePreviewList() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	results := make([]accountPreview, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			res := accountPreview{Account: firstNonEmpty(f.Name, f.ID)}
			sa, _, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				res.Err = fmt.Errorf("load auth: %w", err)
				results[i] = res
				return
			}
			if sa == nil || strings.TrimSpace(sa.Auth.JWT) == "" {
				res.Skipped = true
				results[i] = res
				return
			}
			plans, perr := fetchClaimablePreview(sa)
			res.Plans = plans
			res.Err = perr
			results[i] = res
		}(i, f)
	}
	wg.Wait()
	plans, errs, checked := mergePreviewResults(results)
	return map[string]any{
		"ok":               true,
		"server_time":      time.Now().UTC().Format(time.RFC3339),
		"accounts_checked": checked,
		"plans":            plans,
		"errors":           errs,
	}
}
