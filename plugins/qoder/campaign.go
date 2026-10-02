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
        "sync"
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
        out, miSource, hadFlag, err := fetchCampaignStatusOnce(sa, false)
        if err != nil {
                return nil, err
        }
        // v0.8.38 (official client parity, Intl+CN desktop v0.4.3 asar): right
        // after every campaigns status refresh the client also reads the
        // client_launch_26 limited-number endpoint once (retry-once-when-empty).
        // That pair IS the "launch sync" the desktop client performs at every
        // sign-in — reproduce it best-effort so accounts hosted here see the
        // same qualification signal as an installed client. Never fatal.
        campaignLaunchSync(sa)
        // v0.8.39: remember every daily-shaped row (CLAIM_BENEFIT / empty
        // actionType) the server returned — the bypass-list verdict probe
        // (performCampaignCheckin) needs a real campaign id to POST when a
        // later round hides the row (per-person dedup / device targeting).
        rememberCampaignRound(authRegion(sa), sa.Account.UID, out)
        // v0.8.36 self-heal (hub live pattern): showCampaign=false with a NATIVE
        // identity usually means the identity rotated past its acceptance window
        // — force a fresh one from the official bridge and retry exactly once.
        // Derived identities never retry (there is nothing to rotate); CN
        // envelopes may omit the flag entirely, and an absent flag (hadFlag=false)
        // never triggers the retry either.
        if miSource == "runtime-info" && hadFlag && !out.ShowCampaign {
                machineIdentityFor(authRegion(sa), sa.Account.UID, true)
                if machineIdentityForceHook != nil {
                        machineIdentityForceHook()
                }
                if out2, _, _, err2 := fetchCampaignStatusOnce(sa, true); err2 == nil && out2.ShowCampaign {
                        rememberCampaignRound(authRegion(sa), sa.Account.UID, out2)
                        return out2, nil
                }
        }
        return out, nil
}

// ---------------------------------------------------------------------------
// v0.8.39 — the bypass-list verdict probe.
//
// Field report u673e7fcc ("上游未确认签到成功：message=当前没有可领取的活动")
// was never an upstream message: the old performCampaignCheckin printed it
// whenever the campaigns list showed no CLAIM_BENEFIT/CLAIMABLE row. The hub's
// field practice documents TWO live states in which the daily row is hidden
// while the round's claim endpoint still answers authoritatively:
//
//   - per-person dedup (official rule writes "每账号每轮一次", the server
//     executes per PERSON): once a sibling account on the same machine
//     identity claimed the round, the losers "列表里连活动都不显示" — yet a
//     direct claim POST still returns the definitive
//     status=BLOCKED + failureCode=SAME_PERSON_ALREADY_CLAIMED verdict;
//   - device-targeted filtering: derived/rotated identities silently lose
//     the row from the list.
//
// So instead of declaring "no claimable activity" from the list alone, the
// check-in now POSTs the claim endpoint on the LAST-SEEN daily campaign id
// (the ids are region-stable for the campaign's duration — act-YYYYMMDD-NNN
// rounds stay claimable across days until the next 10:00 UTC+8 refresh) and
// lets the upstream verdict decide: +100 lands, or the panel gets the real
// ALREADY / 同人已领 answer instead of a guess. One probe per account per
// cooldown; ids come exclusively from rows this deployment actually saw.
// ---------------------------------------------------------------------------

// roundMemoMaxAge bounds how long a seen campaign id stays probeable. Daily
// rounds refresh at 10:00 UTC+8; 48h comfortably covers a missed day.
const roundMemoMaxAge = 48 * time.Hour

// roundProbeCooldown rate-limits the bypass probe to one POST per account per
// window — the same 6h cadence the hub applies to its 同人已领 cooldown.
const roundProbeCooldown = 6 * time.Hour

type campaignRoundEntry struct {
        CampaignID  string
        CampaignKey string
        SeenAt      time.Time
}

type campaignRoundMemo struct {
        mu         sync.Mutex
        perAccount map[string]campaignRoundEntry // key region:uid
        perRegion  map[string]campaignRoundEntry // key region
}

var roundMemo = &campaignRoundMemo{
        perAccount: map[string]campaignRoundEntry{},
        perRegion:  map[string]campaignRoundEntry{},
}

// rememberCampaignRound records the first daily-shaped row of the response.
// Any status counts (CLAIMABLE / CLAIMED): a claimed row is exactly the id a
// hidden next round will reuse.
func rememberCampaignRound(region, uid string, status *campaignStatusResponse) {
        if status == nil {
                return
        }
        for i := range status.Campaigns {
                c := &status.Campaigns[i]
                if c.CampaignID == "" {
                        continue
                }
                if at := strings.ToUpper(strings.TrimSpace(c.ActionType)); at != "" && at != "CLAIM_BENEFIT" {
                        continue
                }
                roundMemo.remember(region, uid, c)
                return
        }
}

func (m *campaignRoundMemo) remember(region, uid string, c *campaign) {
        e := campaignRoundEntry{CampaignID: c.CampaignID, CampaignKey: c.CampaignKey, SeenAt: time.Now()}
        m.mu.Lock()
        defer m.mu.Unlock()
        m.perAccount[region+":"+uid] = e
        m.perRegion[region] = e
}

// probeFor returns the id a bypass probe may POST for one account: the
// account's own last-seen daily row first, else the deployment's freshest
// same-region row (the daily campaign id is shared per region — every account
// that can see the row sees the same round). ("", false) when nothing fresh
// exists; probe candidates are never fabricated.
func (m *campaignRoundMemo) probeFor(region, uid string) (campaign, bool) {
        m.mu.Lock()
        defer m.mu.Unlock()
        now := time.Now()
        if e, ok := m.perAccount[region+":"+uid]; ok && now.Sub(e.SeenAt) < roundMemoMaxAge {
                return campaign{CampaignID: e.CampaignID, CampaignKey: e.CampaignKey}, true
        }
        if e, ok := m.perRegion[region]; ok && now.Sub(e.SeenAt) < roundMemoMaxAge {
                return campaign{CampaignID: e.CampaignID, CampaignKey: e.CampaignKey}, true
        }
        return campaign{}, false
}

var (
        roundProbeMu   sync.Mutex
        roundProbeLast = map[string]time.Time{} // key region:uid
)

// probeHiddenRound runs the bypass-list verdict probe for one account and
// returns the normalized claim result, or nil when no fresh id exists or the
// cooldown forbids another POST. Network/parse errors return nil — the probe
// is best-effort and must never mask the list-based diagnosis.
func probeHiddenRound(sa *storedAuth) map[string]any {
        region := authRegion(sa)
        key := region + ":" + sa.Account.UID
        roundProbeMu.Lock()
        if t, ok := roundProbeLast[key]; ok && time.Since(t) < roundProbeCooldown {
                roundProbeMu.Unlock()
                return nil
        }
        c, ok := roundMemo.probeFor(region, sa.Account.UID)
        if !ok {
                roundProbeMu.Unlock()
                return nil
        }
        roundProbeLast[key] = time.Now()
        roundProbeMu.Unlock()
        res, err := claimCampaignByID(sa, &c)
        if err != nil {
                return nil
        }
        return res
}

func campaignsURL(sa *storedAuth) string {
        return billingBaseFor(sa) + "/sash/api/v1/me/campaigns?forceRefresh=true"
}

// fetchCampaignStatusOnce fires one campaigns GET with the desktop headers
// plus the machine-identity layer. Returns the parsed envelope, the identity
// source actually attached ("runtime-info" | "derived"), and whether the
// upstream envelope carried the showCampaign key at all (CN responses may
// omit it — a plain bool field cannot distinguish absent from false).
func fetchCampaignStatusOnce(sa *storedAuth, forceIdentity bool) (*campaignStatusResponse, string, bool, error) {
        req, err := http.NewRequest(http.MethodGet, campaignsURL(sa), nil)
        if err != nil {
                return nil, "", false, err
        }
        // v0.12.76: bounded wait (billing.go parity) — a hung campaigns probe
        // used to ride the bridge's long default ceiling.
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        req = req.WithContext(ctx)
        billingHeaders(req, sa)
        mi := machineIdentityFor(authRegion(sa), sa.Account.UID, forceIdentity)
        attachMachineIdentityHeaders(req, &mi)
        resp, err := hostHTTPDo(req)
        if err != nil {
                return nil, mi.Source, false, err
        }
        if resp.StatusCode >= 400 {
                return nil, mi.Source, false, fmt.Errorf("campaigns http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
        }
        var probe map[string]any
        _ = json.Unmarshal(resp.Body, &probe)
        _, hadFlag := probe["showCampaign"]
        var out campaignStatusResponse
        if err := json.Unmarshal(resp.Body, &out); err != nil {
                return nil, mi.Source, hadFlag, fmt.Errorf("campaigns parse: %w", err)
        }
        return &out, mi.Source, hadFlag, nil
}

// clientLaunchCampaignKey is the campaign whose limited-number endpoint the
// official desktop client reads at every sign-in/launch (asar string literal,
// CampaignMainService.$Br — not a guessed id).
const clientLaunchCampaignKey = "client_launch_26"

// launchSyncWindow rate-limits the launch sync to once per account per window
// (the official client fires it per sign-in; panel loads + check-in + claims
// all ride fetchCampaignStatus here, so the guard keeps the extra GET rare).
const launchSyncWindow = 6 * time.Hour

var (
        launchSyncMu   sync.Mutex
        launchSyncLast = map[string]time.Time{}
)

// campaignLaunchSync mirrors the official client's launch step: after a
// campaigns status refresh it GETs
// /sash/api/v1/me/campaigns/client_launch_26/limited-number, and when the
// server answers hasNumber=false it waits 750ms and re-reads exactly once
// (CampaignMainService.resolveLimitedNumber: the first GET may allocate the
// number, the retry confirms it). Best-effort by design: any error is
// swallowed — the endpoint is a qualification signal, not a claim, and its
// failure must never flip a campaign read into an error.
func campaignLaunchSync(sa *storedAuth) {
        key := authRegion(sa) + ":" + sa.Account.UID
        launchSyncMu.Lock()
        if t, ok := launchSyncLast[key]; ok && time.Since(t) < launchSyncWindow {
                launchSyncMu.Unlock()
                return
        }
        launchSyncLast[key] = time.Now()
        launchSyncMu.Unlock()
        for attempt := 0; attempt < 2; attempt++ {
                if attempt > 0 {
                        time.Sleep(750 * time.Millisecond)
                }
                if has, err := fetchLimitedNumber(sa); err == nil && has {
                        return
                }
        }
}

// fetchLimitedNumber reads one limited-number endpoint. The official parser
// accepts a bare {hasNumber:number, number:int, createdAt} payload (asar
// function cwr); a {data:...} envelope is unwrapped defensively.
func fetchLimitedNumber(sa *storedAuth) (bool, error) {
        req, err := http.NewRequest(http.MethodGet,
                billingBaseFor(sa)+"/sash/api/v1/me/campaigns/"+clientLaunchCampaignKey+"/limited-number", nil)
        if err != nil {
                return false, err
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        req = req.WithContext(ctx)
        billingHeaders(req, sa)
        mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
        attachMachineIdentityHeaders(req, &mi)
        resp, err := hostHTTPDo(req)
        if err != nil {
                return false, err
        }
        if resp.StatusCode >= 400 {
                return false, fmt.Errorf("limited-number http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 160))
        }
        var m map[string]any
        if err := json.Unmarshal(resp.Body, &m); err != nil {
                return false, err
        }
        if data, ok := m["data"].(map[string]any); ok {
                m = data
        }
        has, _ := m["hasNumber"].(bool)
        return has, nil
}

// fetchCampaignReward reads one campaign's grant state via
// GET /sash/api/v1/me/campaigns/{id}/reward — the read-only endpoint the
// official growth-page/activity-iframe JS uses to render the real benefit
// (hub capture). The list rows sometimes carry no benefit for
// detail-oriented rows (actionType=VIEW_DETAILS), so the reward probe is
// how 领取Pro sees the actual face value without opening the page. Read-only
// and idempotent upstream; any HTTP error is the caller's "unknown" signal.
func fetchCampaignReward(sa *storedAuth, campaignID string) (map[string]any, error) {
        req, err := http.NewRequest(http.MethodGet,
                billingBaseFor(sa)+"/sash/api/v1/me/campaigns/"+campaignID+"/reward", nil)
        if err != nil {
                return nil, err
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        req = req.WithContext(ctx)
        billingHeaders(req, sa)
        mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
        attachMachineIdentityHeaders(req, &mi)
        resp, err := hostHTTPDo(req)
        if err != nil {
                return nil, err
        }
        if resp.StatusCode >= 400 {
                return nil, fmt.Errorf("reward http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
        }
        var m map[string]any
        if err := json.Unmarshal(resp.Body, &m); err != nil {
                return nil, err
        }
        return m, nil
}

// rewardBenefit unwraps a /reward payload (bare or {data:{...}} envelope)
// into its benefit kind and amount; zero values mean "not revealed".
func rewardBenefit(body map[string]any) (string, int64) {
        if body == nil {
                return "", 0
        }
        if data, ok := body["data"].(map[string]any); ok {
                body = data
        }
        kind, _ := body["kind"].(string)
        amount := float64(0)
        if b, ok := body["benefit"].(map[string]any); ok {
                if k, ok := b["kind"].(string); ok && k != "" {
                        kind = k
                }
                amount, _ = b["amount"].(float64)
        } else if a, ok := body["amount"].(float64); ok {
                amount = a
        }
        return kind, int64(amount)
}

// claimableCampaign returns the first campaign that is currently claimable
// and inside its activity window.
//
// v0.8.39 (hub parity, field report u673e7fcc): rows with an EMPTY actionType
// are claimable too — the qoder2api-hub's daily claimer only skips rows whose
// actionType is present AND not CLAIM_BENEFIT ("action_type not in
// ('', 'CLAIM_BENEFIT') → continue"); demanding CLAIM_BENEFIT verbatim
// skipped daily rounds the server ships without the field. A readable benefit
// kind must be CREDITS (or absent) so check-in never claims
// subscription/Pro-shaped rows — those belong to the Pro flow.
func claimableCampaign(status *campaignStatusResponse) *campaign {
        if status == nil {
                return nil
        }
        now := time.Now().Unix()
        for i := range status.Campaigns {
                c := &status.Campaigns[i]
                at := strings.ToUpper(strings.TrimSpace(c.ActionType))
                if at != "" && at != "CLAIM_BENEFIT" {
                        continue
                }
                // Empty-actionType rows are the new territory (v0.8.39): claim
                // them only when the readable benefit is credits-shaped so a
                // stray subscription/Pro-shaped row can never ride check-in.
                // CLAIM_BENEFIT rows keep their historical semantics — the
                // daily round sometimes ships a coupon benefit, and claiming
                // it IS the day's check-in.
                if at == "" && c.Benefit != nil && c.Benefit.Kind != "" && !strings.EqualFold(c.Benefit.Kind, "CREDITS") {
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
        mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
        attachMachineIdentityHeaders(req, &mi)
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
                // v0.8.39: the bypass-list probe claims with a synthetic row
                // that carries no benefit — read the face value from the claim
                // response itself (hub: benefit.amount) so the panel shows the
                // real +N instead of 0.
                amount := campaignCredit(c)
                if amount == 0 {
                        if b, ok := body["benefit"].(map[string]any); ok {
                                if a, ok2 := b["amount"].(float64); ok2 && a > 0 {
                                        amount = int64(a)
                                }
                        }
                }
                return map[string]any{
                        "success":        true,
                        "result":         "CLAIMED",
                        "rewardCredits":  float64(amount),
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
// "rewardCredits":N} / result=ALREADY_CLAIMED / result=NOTHING_CLAIMABLE /
// success+message failure).
//
// v0.12.109: "nothing claimable" used to answer a bare failure message that
// the panel wrapped into 上游未确认签到成功 — an error toast for a NORMAL
// state (field report u673e7fcc: the account's only row was a VIEW_DETAILS
// newbie campaign, which this claimer correctly refuses to POST). It now
// returns a typed result=NOTHING_CLAIMABLE with a row-level diagnosis, and
// checkinOneAccount renders it as a skip.
func performCampaignCheckin(sa *storedAuth) (map[string]any, error) {
        // v0.8.39 (hub parity): the official client re-runs its native bridge
        // before every claim path — an identity that rotated past its
        // acceptance window silently filters the device-targeted rows the
        // claim depends on. Native identities re-run the bridge (~3.7s);
        // derived ones just recompute (pure CPU, nothing to rotate).
        machineIdentityFor(authRegion(sa), sa.Account.UID, true)
        status, err := fetchCampaignStatus(sa)
        if err != nil {
                return nil, err
        }
        c := claimableCampaign(status)
        if c == nil {
                if claimedCampaign(status) != nil {
                        return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
                }
                // v0.8.39: a hidden row is not proof of nothing-to-claim —
                // POST the last-seen daily campaign id once and let upstream
                // hand down its verdict (probeHiddenRound's doc block). Only a
                // conclusive verdict short-circuits; inconclusive results
                // fall through to the list-based diagnosis.
                if res := probeHiddenRound(sa); res != nil {
                        if success, _ := res["success"].(bool); success {
                                return res, nil
                        }
                        if r, _ := res["result"].(string); r == "ALREADY_CLAIMED" || r == "BLOCKED" {
                                return res, nil
                        }
                }
                return map[string]any{
                        "success": false,
                        "result":  "NOTHING_CLAIMABLE",
                        "message": campaignIdleDiagnosis(status, sa),
                }, nil
        }
        return claimCampaignByID(sa, c)
}

// campaignIdleDiagnosis explains WHY no claimable row is visible, in one
// panel-renderable line. v0.8.39: the upstream's own unavailableReason
// taxonomy is rendered in official semantics (hub labels — 名额发完 / 成就
// 未完成 / 风控拦截 / 活动未开始), claimable VIEW_DETAILS rows (the newbie
// packs — the benefit hides behind the activity page) are named explicitly,
// and the machine-identity hint rides along when this host runs on a derived
// pseudo-device. Current official newbie reality rides the message too:
// first-login grants 300+100 credits and the +1800 Pro pack is no longer
// delivered (user field report 2026-10-01).
func campaignIdleDiagnosis(status *campaignStatusResponse, sa *storedAuth) string {
        if status == nil || len(status.Campaigns) == 0 {
                return "今日暂无可领取权益（活动列表为空）" + machineIdentityHint(sa)
        }
        reasons := map[string]string{
                "REDEMPTION_CODE_OUT_OF_STOCK": "名额已发完（次日 10:00 后可再试）",
                "ACHIEVEMENT_NOT_COMPLETED":    "需先在官方桌面端完成新人任务",
                "RISK_BLOCKED":                 "风控拦截",
                "CAMPAIGN_NOT_ACTIVE":          "活动未开始或已结束",
        }
        parts := make([]string, 0, 2)
        locked := make([]string, 0, 2)
        for i := range status.Campaigns {
                c := &status.Campaigns[i]
                if strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
                        if at := strings.ToUpper(strings.TrimSpace(c.ActionType)); at != "" && at != "CLAIM_BENEFIT" {
                                parts = append(parts, fmt.Sprintf("%s（%s，需在官方活动页完成领取）", c.CampaignKey, c.ActionType))
                        }
                        continue
                }
                if r := reasons[strings.ToUpper(strings.TrimSpace(c.UnavailableReason))]; r != "" {
                        locked = append(locked, fmt.Sprintf("%s（%s）", c.CampaignKey, r))
                }
        }
        segs := make([]string, 0, 3)
        if len(parts) > 0 {
                segs = append(segs, "存在需活动页领取的活动行："+strings.Join(parts, "、"))
        }
        if len(locked) > 0 {
                segs = append(segs, "另有暂不可领活动："+strings.Join(locked, "、"))
        }
        if len(segs) == 0 {
                return "今日暂无可领取权益" + machineIdentityHint(sa)
        }
        return "今日暂无可直接签领的权益；" + strings.Join(segs, "；") + machineIdentityHint(sa)
}
