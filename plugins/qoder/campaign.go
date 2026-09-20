// campaign.go implements the Intl check-in contract plus the per-region
// billing capability table.
//
// Qoder Intl does not expose the CN daily-check-in endpoints
// (/sash/api/v1/me/daily-check-in/{status,claim} are CN-only). Its daily
// benefit is delivered as a marketing campaign: GET /sash/api/v1/me/campaigns
// lists the account's campaigns and POST /sash/api/v1/me/campaigns/{id}/claim
// claims one. The web growth page (activity bundle) uses exactly these two
// calls and the desktop client polls the same status endpoint (fork
// bfSan/qoder-cpa-plugin review, endpoint contract verified against the
// desktop client log). Before v0.8.18 the panel rendered the CN check-in
// button for Intl accounts and the claim died on a 404; Intl now routes by
// contract instead of by endpoint guesswork.
//
// The capability table also records that Intl has no Pro-upgrade contract.
// That absence is a capability fact, not a transient failure: the panel must
// hide the 领取Pro button for Intl accounts instead of firing a request that
// can only 404.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// checkinContract selects the upstream check-in dialect for a credential.
type checkinContract int

const (
	checkinContractDaily    checkinContract = iota // CN: daily-check-in status/claim
	checkinContractCampaign                        // Intl: campaigns list + claim
)

// regionCapabilities records which upstream billing contracts exist per
// region. Both regions share quota/plan/refresh; they differ in check-in
// dialect and Pro-upgrade availability.
type regionCapabilities struct {
	Checkin    bool
	ProUpgrade bool
	Contract   checkinContract
}

func capabilitiesForRegion(region string) regionCapabilities {
	if normalizeRegion(region) == regionIntl {
		return regionCapabilities{
			Checkin:    true,
			ProUpgrade: false,
			Contract:   checkinContractCampaign,
		}
	}
	return regionCapabilities{
		Checkin:    true,
		ProUpgrade: true,
		Contract:   checkinContractDaily,
	}
}

// supportsProUpgrade reports whether the credential's region has a Pro
// upgrade contract at all (Intl does not — skip, never retry a 404).
func supportsProUpgrade(sa *storedAuth) bool {
	return capabilitiesForRegion(authRegion(sa)).ProUpgrade
}

type campaignStatusResponse struct {
	ShowCampaign bool       `json:"showCampaign"`
	Claimable    bool       `json:"claimable"`
	Campaigns    []campaign `json:"campaigns"`
}

type campaign struct {
	CampaignID  string       `json:"campaignId"`
	CampaignKey string       `json:"campaignKey"`
	ActionType  string       `json:"actionType"`
	StartAt     int64        `json:"startAt"`
	EndAt       int64        `json:"endAt"`
	ClaimStatus string       `json:"claimStatus"` // CLAIMABLE | CLAIMED | ...
	Benefit     *campaignBen `json:"benefit,omitempty"`
}

type campaignBen struct {
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

func fetchCampaignStatus(sa *storedAuth) (*campaignStatusResponse, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/sash/api/v1/me/campaigns?forceRefresh=true", nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("campaigns http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var out campaignStatusResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("campaigns parse: %w", err)
	}
	return &out, nil
}

// claimableCampaign returns the first CLAIM_BENEFIT campaign that is
// currently claimable and inside its activity window.
func claimableCampaign(status *campaignStatusResponse) *campaign {
	if status == nil {
		return nil
	}
	now := time.Now().Unix()
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		if !strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") {
			continue
		}
		if !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
			continue
		}
		if c.StartAt > 0 && now < c.StartAt {
			continue
		}
		if c.EndAt > 0 && now > c.EndAt {
			continue
		}
		return c
	}
	return nil
}

// claimedCampaign returns a CLAIM_BENEFIT row already claimed. Note: claimed
// campaigns disappear from /me/campaigns entirely once the activity ends, so
// an inactive summary is a normal state, not a failure (v0.8.18: surfaced as
// reason=none instead of an error path).
func claimedCampaign(status *campaignStatusResponse) *campaign {
	if status == nil {
		return nil
	}
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		if strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") && strings.EqualFold(c.ClaimStatus, "CLAIMED") {
			return c
		}
	}
	return nil
}

func campaignCredit(c *campaign) int64 {
	if c == nil || c.Benefit == nil || !strings.EqualFold(c.Benefit.Kind, "CREDITS") {
		return 0
	}
	return c.Benefit.Amount
}

// fetchCampaignCheckinSummary maps the campaign list onto the panel's shared
// checkinSummary shape so dashboard rendering stays dialect-agnostic.
func fetchCampaignCheckinSummary(sa *storedAuth) (*checkinSummary, error) {
	status, err := fetchCampaignStatus(sa)
	if err != nil {
		return nil, err
	}
	return campaignCheckinSummary(status), nil
}

func campaignCheckinSummary(status *campaignStatusResponse) *checkinSummary {
	sum := &checkinSummary{
		Active:       status != nil && (status.ShowCampaign || status.Claimable),
		ActivityName: "权益活动",
	}
	if status == nil {
		return sum
	}
	if c := claimableCampaign(status); c != nil {
		sum.DailyCredit = campaignCredit(c)
		return sum
	}
	if c := claimedCampaign(status); c != nil {
		sum.TodayCheckedIn = true
		sum.DailyCredit = campaignCredit(c)
		sum.TodayCredit = campaignCredit(c)
	}
	return sum
}

// performCampaignCheckin claims one Intl campaign and normalizes the result
// to the same shape as the CN daily-check-in claim ({"success":true,
// "rewardCredits":N} / result=ALREADY_CLAIMED / success+message failure).
func performCampaignCheckin(sa *storedAuth) (map[string]any, error) {
	status, err := fetchCampaignStatus(sa)
	if err != nil {
		return nil, err
	}
	c := claimableCampaign(status)
	if c == nil {
		if claimedCampaign(status) != nil {
			return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
		}
		return map[string]any{"success": false, "message": "当前没有可领取的活动"}, nil
	}
	req, err := http.NewRequest(
		http.MethodPost,
		upstreamBaseFor(sa)+"/sash/api/v1/me/campaigns/"+c.CampaignID+"/claim",
		strings.NewReader("{}"),
	)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	// The activity page accepts either a bare payload or a {data:{...}}
	// envelope; the claim succeeded when status reports CLAIMED.
	body := m
	if data, ok := m["data"].(map[string]any); ok {
		body = data
	}
	if statusValue, _ := body["status"].(string); !strings.EqualFold(statusValue, "CLAIMED") {
		return map[string]any{"success": false, "upstream": m}, nil
	}
	return map[string]any{
		"success":        true,
		"result":         "CLAIMED",
		"rewardCredits":  float64(campaignCredit(c)),
		"campaign_id":    c.CampaignID,
		"campaign_key":   c.CampaignKey,
		"campaign_title": c.CampaignKey,
	}, nil
}
