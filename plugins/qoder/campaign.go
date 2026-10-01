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
        "context"
        "encoding/json"
        "fmt"
        "net/http"
        "strings"
        "time"
)

// checkinContract selects the upstream check-in dialect for a credential.
type checkinContract int

const (
        checkinContractDaily    checkinContract = iota // RETIRED v0.12.80 — legacy daily-check-in is DISABLED upstream
        checkinContractCampaign                        // campaigns list + claim (Intl since v0.8.18, CN since v0.12.80)
)

// regionCapabilities records which upstream billing contracts exist per
// region. Both regions share quota/plan/refresh and — since v0.12.80 — the
// campaigns check-in dialect. They still differ in Pro-upgrade availability.
//
// v0.12.80 CN dialect switch (field report: "Qoder CN 账户仍然不能签到"):
// upstream DISABLED the legacy daily-check-in system globally — status
// reports DISABLED with zero streak and claim answers 409 even on unclaimed
// days while granting no credits (verified upstream 2026-09-21; same
// conclusion in the qoder2api project's packet-captured campaigns flow,
// "不走 daily-check-in/claim —— 该 legacy 端点已 DISABLED"). CN accounts now
// claim via GET /sash/api/v1/me/campaigns + POST .../campaigns/{id}/claim,
// the same system that already served Intl since v0.8.18. The legacy status
// endpoint stays readable and is merged as a read-only stats supplement
// (billing.go mergeLegacyCheckinStats).
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
                Contract:   checkinContractCampaign,
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
        // v0.8.35 fields — reverse-engineered from the official
        // growth-page/activity-iframe JS (cross-verified against the
        // qoder2api-hub capture). They explain WHY a row is not claimable:
        // task campaigns gate on achievements, device-targeted rows are
        // filtered server-side, and the reason string carries the upstream's
        // own verdict (e.g. ACHIEVEMENT_NOT_COMPLETED).
        RequiredAchievementKey string `json:"requiredAchievementKey,omitempty"`
        AchievementCompleted   bool   `json:"achievementCompleted,omitempty"`
        UnavailableReason      string `json:"unavailableReason,omitempty"`
        Placements             []any  `json:"placements,omitempty"`
}

type campaignBen struct {
        Kind   string `json:"kind"`
        Amount int64  `json:"amount"`
}

func fetchCampaignStatus(sa *storedAuth) (*campaignStatusResponse, error) {
        req, err := http.NewRequest(http.MethodGet, billingBaseFor(sa)+"/sash/api/v1/me/campaigns?forceRefresh=true", nil)
        if err != nil {
                return nil, err
        }
        // v0.12.76: bounded wait (billing.go parity) — a hung campaigns probe
        // used to ride the bridge's long default ceiling.
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        req = req.WithContext(ctx)
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
        sum := &checkinSummary{ActivityName: "权益活动"}
        if status == nil {
                return sum
        }
        // v0.12.80: a CLAIMABLE row is authoritative evidence of an active
        // benefit regardless of the envelope's showCampaign/claimable flags —
        // the CN campaigns response (unlike the Intl growth-page envelope this
        // dialect was built on) may not carry them. The old order left
        // Active=false with DailyCredit set whenever the flags were absent,
        // which the panel renders as an unreachable "不可签".
        if c := claimableCampaign(status); c != nil {
                sum.Active = true
                sum.DailyCredit = campaignCredit(c)
                return sum
        }
        sum.Active = status.ShowCampaign || status.Claimable
        if c := claimedCampaign(status); c != nil {
                sum.TodayCheckedIn = true
                sum.DailyCredit = campaignCredit(c)
                sum.TodayCredit = campaignCredit(c)
        }
        return sum
}

// claimCampaignByID POSTs one campaign's claim endpoint and normalizes the
// response to the panel's shared shape ({success, result, rewardCredits,
// campaign_id, ...} / {"success":false,"result":"ALREADY_CLAIMED"} /
// {"success":false,"message":...}). Shared by the check-in flow
// (performCampaignCheckin) and, since v0.8.34, by the Pro-upgrade flow
// (handleClaimPro) — the pro-upgrade pack rides this same campaigns system;
// see checkin.go for the upstream forensics that retired the standalone
// pro-upgrade endpoints.
func claimCampaignByID(sa *storedAuth, c *campaign) (map[string]any, error) {
        req, err := http.NewRequest(
                http.MethodPost,
                billingBaseFor(sa)+"/sash/api/v1/me/campaigns/"+c.CampaignID+"/claim",
                strings.NewReader("{}"),
        )
        if err != nil {
                return nil, err
        }
        // v0.12.76: bounded wait (billing.go parity).
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        req = req.WithContext(ctx)
        billingHeaders(req, sa)
        resp, err := hostHTTPDo(req)
        if err != nil {
                return map[string]any{"success": false, "message": err.Error()}, nil
        }
        if resp.StatusCode >= 400 {
                // v0.8.35: upstream also delivers the idempotent replay as an
                // HTTP 409 carrying errorCode=ALREADY_CLAIMED/REPLAYED (hub
                // capture) — normalize it like the 200 replayed body instead of
                // surfacing a raw http error.
                if resp.StatusCode == http.StatusConflict {
                        var e map[string]any
                        if json.Unmarshal(resp.Body, &e) == nil {
                                if ec, _ := e["errorCode"].(string); strings.Contains(strings.ToUpper(ec), "ALREADY") || strings.EqualFold(ec, "REPLAYED") {
                                        return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
                                }
                        }
                }
                return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
        }
        var m map[string]any
        if err := json.Unmarshal(resp.Body, &m); err != nil {
                return nil, err
        }
        // The activity page accepts either a bare payload or a {data:{...}}
        // envelope; the claim succeeded when status reports CLAIMED.
        // replayed=true is the upstream's idempotent replay (the same claim
        // landed earlier today — qoder2api capture): surface it as
        // ALREADY_CLAIMED so the panel shows 今日已签 instead of a fresh
        // success toast that would invite the user to claim again.
        //
        // v0.8.35: two more upstream verdicts, both live-verified by the
        // qoder2api-hub capture:
        //   - HTTP 409 with errorCode ALREADY_CLAIMED/REPLAYED → the same
        //     idempotent replay, delivered as an error status instead of a
        //     200 body;
        //   - status=BLOCKED / failureCode=SAME_PERSON_ALREADY_CLAIMED →
        //     upstream dedupes by PERSON, not by account: a second account
        //     on the same machine identity already took this round's grant.
        //     The row even disappears from that account's list afterwards.
        body := m
        if data, ok := m["data"].(map[string]any); ok {
                body = data
        }
        statusValue, _ := body["status"].(string)
        failureCode, _ := body["failureCode"].(string)
        if strings.EqualFold(statusValue, "CLAIMED") {
                if replayed, _ := body["replayed"].(bool); replayed {
                        return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
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
        if strings.EqualFold(statusValue, "BLOCKED") || strings.EqualFold(failureCode, "SAME_PERSON_ALREADY_CLAIMED") {
                return map[string]any{
                        "success":      false,
                        "result":       "BLOCKED",
                        "failure_code": failureCode,
                        "message":      "同人已领取（同一设备身份下的其他账号本轮已领，服务端按人去重）",
                }, nil
        }
        return map[string]any{"success": false, "upstream": m}, nil
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
        return claimCampaignByID(sa, c)
}
