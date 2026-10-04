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
	CampaignURL  string     `json:"campaignUrl"` // v0.8.46: official client opens this as a WebView; the daily 100 Credits row only appears in campaigns[] AFTER the surface is "opened"
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
	// v0.8.47: the v0.8.46 openCampaignSurface retry was REMOVED. The user's
	// working Python script (which sends NO Cosy-Machine* headers and NO
	// surface-open step) proves the campaigns endpoint already returns
	// CLAIMABLE CLAIM_BENEFIT rows in its initial response — the daily
	// 100-Credits row was only missing because the plugin was sending
	// simulated Cosy-Machine* headers that the server's anti-fraud layer
	// filtered. With v0.8.47's attachMachineIdentityHeaders now skipping
	// machine headers for derived identities, the first fetch returns the
	// full list and no surface-open retry is needed.
	return out, nil
}

// hasClaimableDailyRow reports whether the campaigns list already has a
// CLAIM_BENEFIT (or empty-actionType) CLAIMABLE row — the daily 100-Credits
// check-in. Kept for diagnostics; no longer drives a retry (v0.8.47 removed
// the surface-open path).
func hasClaimableDailyRow(status *campaignStatusResponse) bool {
	if status == nil {
		return false
	}
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		at := strings.ToUpper(strings.TrimSpace(c.ActionType))
		if at != "" && at != "CLAIM_BENEFIT" {
			continue
		}
		if !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
			continue
		}
		return true
	}
	return false
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
//
// v0.8.40 cooldown fix ("概率性不成功"): the latch used to be stamped BEFORE
// the probe fired, so one inconclusive attempt (network hiccup, upstream 5xx)
// silenced every retry for the next 6h — auto check-ins all day reported
// "今日暂无可领取权益" without ever POSTing. Only a CONCLUSIVE upstream
// verdict (claimed / already / blocked / typed eligibility) now arms the
// cooldown; inconclusive transport failures leave it free for the next tick.
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
	roundProbeMu.Unlock()
	res, err := claimCampaignByID(sa, &c)
	if err != nil || res == nil {
		return nil // transport/parse failure — retryable, no latch
	}
	if res["success"] == true {
		roundProbeMu.Lock()
		roundProbeLast[key] = time.Now()
		roundProbeMu.Unlock()
		return res
	}
	switch r, _ := res["result"].(string); r {
	case "ALREADY_CLAIMED", "BLOCKED", "NOT_ELIGIBLE":
		roundProbeMu.Lock()
		roundProbeLast[key] = time.Now()
		roundProbeMu.Unlock()
		return res
	}
	return res // inconclusive (http error body etc.) — returned, not latched
}

// campaignsURL returns the campaigns list URL.
//
// v0.8.46 (official desktop client v0.4.3 asar forensics): the official
// campaignMainService.getStatus() builds the URL as
//
//	new URL("/sash/api/v1/me/campaigns", this.discoveryBaseUrl)
//
// with NO query string. The `forceRefresh` parameter is a LOCAL cache-bypass
// flag only — it appears in log metadata but is NEVER sent to the server.
// The old `?forceRefresh=true` caused the server to return a FILTERED
// campaigns list for accounts that hadn't "opened" the activity page: only
// VIEW_DETAILS rows appeared (act-20260901-493 etc.), the daily
// CLAIM_BENEFIT 100-Credits row was silently dropped. Removing the parameter
// makes the server return the full cached list including the daily row.
func campaignsURL(sa *storedAuth) string {
	return billingBaseFor(sa) + "/sash/api/v1/me/campaigns"
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

// ---------------------------------------------------------------------------
// v0.8.40 — upstream eligibility taxonomy + multi-row claiming.
//
// Field report ("签到的积分概率性不成功或者成功后积分无变化"): two distinct
// upstream states the old code mis-rendered.
//
//   - 概率性不成功: the daily rounds are 先到先得 — once the day's stock is
//     gone the claim answers 200 + failureCode=REDEMPTION_CODE_OUT_OF_STOCK
//     (refresh 10:00 UTC+8). Same family: ACHIEVEMENT_NOT_COMPLETED,
//     CAMPAIGN_NOT_ACTIVE, RISK_BLOCKED, RISK_DEPENDENCY_UNAVAILABLE. The old
//     claimer dropped these into its generic-upstream bucket → "上游未确认
//     签到成功" error toasts on healthy accounts, "probabilistic" because
//     stock depends on when in the day the check-in runs.
//   - 成功后积分无变化: coupon/redemption rows (e.g. CN act-20260928-620
//     奶茶免单卡) answer CLAIMED with a redemptionCode and NO credits
//     benefit — claiming one IS a success, but the credits pool never moves.
//     The old claimer treated every CLAIMED as a credits grant.
//
// Evidence: qoder2api-hub qoder_accounts.py (_CAMPAIGN_FAILURE_CN,
// claim_campaign redemptionCode handling) + official growth-page state
// machine. Taxonomy table mirrors the hub's Chinese verdicts verbatim.
// ---------------------------------------------------------------------------

var campaignFailureCN = map[string]string{
	"REDEMPTION_CODE_OUT_OF_STOCK": "今日名额已发完（每日 10:00 刷新，次日 10:00 后自动重试）",
	"ACHIEVEMENT_NOT_COMPLETED":    "需先在官方客户端完成新人任务（成就未完成）",
	"CAMPAIGN_NOT_ACTIVE":          "活动已结束或未开始",
	"RISK_BLOCKED":                 "风控拦截（当前设备/账号不可领取）",
	"RISK_DEPENDENCY_UNAVAILABLE":  "风控服务暂不可用，稍后自动重试",
}

// campaignBenefitKindOf pulls the benefit kind out of a claim-response body
// (bare or {data:...} unwrapped by the caller). Empty when absent.
func campaignBenefitKindOf(body map[string]any) (string, bool) {
	if body == nil {
		return "", false
	}
	if b, ok := body["benefit"].(map[string]any); ok {
		if k, ok := b["kind"].(string); ok {
			return strings.ToUpper(k), true
		}
	}
	if k, ok := body["kind"].(string); ok && k != "" {
		return strings.ToUpper(k), true
	}
	return "", false
}

// campaignBenefitClass buckets a list row's benefit for the claimer:
//
//	"credits"    — CREDITS (or absent kind): the daily check-in's currency
//	"redemption" — REDEMPTION_CODE / REDEMPTION_COUPON / COUPON rows
//	"other"      — any other readable kind (subscription-shaped etc.)
func campaignBenefitClass(c *campaign) string {
	if c == nil || c.Benefit == nil {
		return "credits" // absent kind: hub treats as claimable daily
	}
	switch strings.ToUpper(strings.TrimSpace(c.Benefit.Kind)) {
	case "", "CREDITS":
		return "credits"
	case "REDEMPTION_CODE", "REDEMPTION_COUPON", "COUPON":
		return "redemption"
	default:
		return "other"
	}
}

// campaignTitle renders a human name for a row: the official zh title from
// the placements payload (hub: placements content.zh.title), else the
// campaign key. Keeps panel messages readable ("每天领 100 Credits" instead
// of "act-20260930-125").
func campaignTitle(c *campaign) string {
	if c == nil {
		return ""
	}
	for _, p := range c.Placements {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		content, ok := pm["content"].(map[string]any)
		if !ok {
			continue
		}
		for _, lang := range []string{"zh", "en"} {
			lm, ok := content[lang].(map[string]any)
			if !ok {
				continue
			}
			if t, ok := lm["title"].(string); ok && strings.TrimSpace(t) != "" {
				return strings.TrimSpace(t)
			}
		}
	}
	if c.CampaignKey != "" {
		return c.CampaignKey
	}
	return c.CampaignID
}

// claimableCampaigns returns every currently-claimable row (window-checked),
// credits-kind rows first so a same-round coupon never delays the daily
// grant. v0.8.40: the check-in is now a MULTI-claim pass — an account may
// hold a credits row AND a redemption row simultaneously, and claiming only
// the first left the rest for a "tomorrow" that never came.
func claimableCampaigns(status *campaignStatusResponse) []*campaign {
	if status == nil {
		return nil
	}
	now := time.Now().Unix()
	var credits, rest []*campaign
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		at := strings.ToUpper(strings.TrimSpace(c.ActionType))
		if at != "" && at != "CLAIM_BENEFIT" {
			continue
		}
		// Empty-actionType rows stay credits-only (v0.8.39 guard): a
		// stray subscription-shaped row must never ride check-in.
		// Rows with an explicit kind are classed by that kind.
		if at == "" {
			if cls := campaignBenefitClass(c); cls != "credits" {
				continue
			}
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
		if campaignBenefitClass(c) == "credits" {
			credits = append(credits, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(credits, rest...)
}

// campaignListVerdict reads the NOT-claimable rows' unavailableReason and
// returns a typed verdict when the list itself explains why nothing is
// claimable right now (out-of-stock / achievement-gated). ("", false) when
// the rows carry no explanation. Hub state machine: the official frontend
// renders these same two states as outOfStock / locked.
func campaignListVerdict(status *campaignStatusResponse) (map[string]any, bool) {
	if status == nil {
		return nil, false
	}
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		at := strings.ToUpper(strings.TrimSpace(c.ActionType))
		if at != "" && at != "CLAIM_BENEFIT" {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(c.UnavailableReason)) {
		case "REDEMPTION_CODE_OUT_OF_STOCK":
			return map[string]any{
				"success":      false,
				"result":       "NOT_ELIGIBLE",
				"failure_code": "REDEMPTION_CODE_OUT_OF_STOCK",
				"message":      campaignFailureCN["REDEMPTION_CODE_OUT_OF_STOCK"],
			}, true
		case "ACHIEVEMENT_NOT_COMPLETED":
			return map[string]any{
				"success":      false,
				"result":       "NOT_ELIGIBLE",
				"failure_code": "ACHIEVEMENT_NOT_COMPLETED",
				"message":      campaignFailureCN["ACHIEVEMENT_NOT_COMPLETED"],
			}, true
		}
	}
	return nil, false
}

// claimableCampaign returns the first campaign that is currently claimable
// and inside its activity window.
//
// v0.8.39 (hub parity, field report u673e7fcc): rows with an EMPTY actionType
// are claimable too — the qoder2api-hub's daily claimer only skips rows whose
// actionType is present AND not CLAIM_BENEFIT ("action_type not in
// (”, 'CLAIM_BENEFIT') → continue"); demanding CLAIM_BENEFIT verbatim
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
	req.Header.Set("Content-Type", "application/json") // POST with body — official client's WebView JS sets this via fetch
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
	// v0.8.40: coupon/redemption campaigns answer CLAIMED with a
	// redemptionCode instead of a credits benefit (hub capture:
	// act-20260928-620 奶茶免单卡; "仅 CLAIMED 无码 = 发放确认中").
	// Surfacing the code is the whole point of claiming such a row — and
	// the caller must know NO credits moved, or the panel invites
	// "签到成功但积分无变化" bug reports.
	redemptionCode, _ := body["redemptionCode"].(string)
	benefitKind, _ := campaignBenefitKindOf(body)
	if (strings.EqualFold(statusValue, "CLAIMED") || strings.EqualFold(statusValue, "GRANTED") || strings.EqualFold(statusValue, "SUCCESS")) && !strings.EqualFold(statusValue, "BLOCKED") {
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
		// v0.8.40: a redemption-code reward is NOT a credits grant.
		// Face value 0 + explicit kind so the aggregation in
		// performCampaignCheckin and the panel render it honestly.
		if strings.TrimSpace(redemptionCode) != "" {
			return map[string]any{
				"success":         true,
				"result":          "CLAIMED",
				"reward_credits":  float64(0),
				"rewardCredits":   float64(0),
				"benefit_kind":    "REDEMPTION_CODE",
				"redemption_code": redemptionCode,
				"campaign_id":     c.CampaignID,
				"campaign_key":    c.CampaignKey,
				"campaign_title":  campaignTitle(c),
				"message":         "已领取兑换券：" + redemptionCode + "（非积分奖励，请到官方活动页兑换）",
			}, nil
		}
		if strings.EqualFold(statusValue, "CLAIMED") && amount == 0 && strings.EqualFold(benefitKind, "REDEMPTION_CODE") {
			// Grant confirmed but the code not minted yet — hub's
			// "confirming" state. Success, with an explicit lag note.
			return map[string]any{
				"success":         true,
				"result":          "CLAIMED",
				"reward_credits":  float64(0),
				"rewardCredits":   float64(0),
				"benefit_kind":    "REDEMPTION_CODE",
				"redemption_code": "",
				"campaign_id":     c.CampaignID,
				"campaign_key":    c.CampaignKey,
				"campaign_title":  campaignTitle(c),
				"message":         "兑换券已确认领取，兑换码发放中（稍后在官方活动页查看）",
			}, nil
		}
		return map[string]any{
			"success":        true,
			"result":         "CLAIMED",
			"reward_credits": float64(amount),
			"rewardCredits":  float64(amount),
			"benefit_kind":   benefitKind,
			"campaign_id":    c.CampaignID,
			"campaign_key":   c.CampaignKey,
			"campaign_title": campaignTitle(c),
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
	// v0.8.40 (hub taxonomy, field report "签到的积分概率性不成功"):
	// the upstream ALSO answers 200 with an eligibility failureCode —
	// REDEMPTION_CODE_OUT_OF_STOCK (the daily rounds are 先到先得 with a
	// 10:00 UTC+8 refresh), ACHIEVEMENT_NOT_COMPLETED (new-user task rows),
	// CAMPAIGN_NOT_ACTIVE, RISK_BLOCKED / RISK_DEPENDENCY_UNAVAILABLE.
	// The old code dropped those into the generic {"upstream": m} bucket,
	// which checkinOneAccount rendered as 上游未确认签到成功 — an error
	// toast for a normal, retryable state. Typed verdicts now.
	if msg, ok := campaignFailureCN[strings.ToUpper(failureCode)]; ok {
		return map[string]any{
			"success":      false,
			"result":       "NOT_ELIGIBLE",
			"failure_code": strings.ToUpper(failureCode),
			"message":      msg,
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
	// v0.8.40: the check-in is a MULTI-claim pass now. An account can
	// hold a credits row (the daily +100) AND a coupon row at once, and
	// claiming only the first stranded the rest. Claim every claimable
	// row, credits-kind first, and aggregate: earned credits, redemption
	// codes, and per-row verdicts. The aggregate decides the result.
	rows := claimableCampaigns(status)
	if len(rows) == 0 {
		if claimedCampaign(status) != nil {
			return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
		}
		// v0.8.40 (hub taxonomy): when the LIST itself explains the
		// state (out-of-stock / achievement-gated), that typed verdict
		// outranks the generic nothing-claimable diagnosis — the panel
		// gets "名额已发完，次日 10:00 后自动重试" instead of an error.
		if v, ok := campaignListVerdict(status); ok {
			return v, nil
		}
		// v0.8.39: a hidden row is not proof of nothing-to-claim —
		// POST the last-seen daily campaign id once and let upstream
		// hand down its verdict (probeHiddenRound's doc block). Only a
		// conclusive verdict short-circuits; inconclusive results
		// fall through to the list-based diagnosis.
		//
		// v0.8.41 (issue #27 finding-3 parity, field report
		// act-20260901-922 / act-20260901-493): before probing, attempt the
		// CLAIMABLE VIEW_DETAILS rows themselves — the official client does
		// not stop at CLAIM_BENEFIT rows, the captured launch flow POSTs the
		// same claim on a VIEW_DETAILS (bogo) row and gets 200 CLAIMED. The
		// old code only wrote 需在官方活动页完成领取 next to those rows,
		// stranding the account's one visible benefit behind a manual chore.
		landed, notes := attemptViewDetailsClaims(sa, status)
		if landed != nil {
			return landed, nil
		}
		probeRes := probeHiddenRound(sa)
		if probeRes != nil {
			if success, _ := probeRes["success"].(bool); success {
				mergeVerdictNotes(probeRes, notes)
				return probeRes, nil
			}
			switch r, _ := probeRes["result"].(string); r {
			case "ALREADY_CLAIMED", "BLOCKED":
				mergeVerdictNotes(probeRes, notes)
				return probeRes, nil
			case "NOT_ELIGIBLE":
				// v0.8.41: this typed verdict used to be discarded here —
				// the user saw the generic idle line while upstream had
				// just answered the real reason (round ended / out of
				// stock). Return it typed, with the VIEW_DETAILS verdicts
				// and the list diagnosis folded into the message.
				msg, _ := probeRes["message"].(string)
				all := make([]string, 0, 4)
				if strings.TrimSpace(msg) != "" {
					all = append(all, msg)
				}
				all = append(all, notes...)
				all = append(all, campaignIdleDiagnosis(status, sa))
				probeRes["message"] = strings.Join(all, "；")
				return probeRes, nil
			}
		}
		diag := campaignIdleDiagnosis(status, sa)
		if len(notes) > 0 {
			diag += "；" + strings.Join(notes, "；")
		}
		return map[string]any{
			"success": false,
			"result":  "NOTHING_CLAIMABLE",
			"message": diag,
		}, nil
	}
	earned := float64(0)
	creditsClaimed := false
	codes := make([]string, 0, 2)
	notes := make([]string, 0, 3)
	blocked, already := false, false
	failureCode := ""
	for _, c := range rows {
		res, err := claimCampaignByID(sa, c)
		if err != nil {
			notes = append(notes, campaignTitle(c)+": "+err.Error())
			continue
		}
		if success, _ := res["success"].(bool); success {
			if rc, ok := res["reward_credits"].(float64); ok && rc > 0 {
				earned += rc
				creditsClaimed = true
			}
			if code, ok := res["redemption_code"].(string); ok && strings.TrimSpace(code) != "" {
				codes = append(codes, code)
			} else if kind, _ := res["benefit_kind"].(string); strings.EqualFold(kind, "REDEMPTION_CODE") {
				notes = append(notes, campaignTitle(c)+"：兑换码发放中")
			}
			continue
		}
		switch r, _ := res["result"].(string); r {
		case "ALREADY_CLAIMED":
			already = true
		case "BLOCKED":
			blocked = true
		case "NOT_ELIGIBLE":
			if msg, ok := res["message"].(string); ok && msg != "" {
				notes = append(notes, msg)
			}
			if fc, ok := res["failure_code"].(string); ok && fc != "" && failureCode == "" {
				failureCode = fc
			}
		default:
			if msg, ok := res["message"].(string); ok && msg != "" {
				notes = append(notes, campaignTitle(c)+": "+msg)
			}
		}
	}
	if creditsClaimed {
		out := map[string]any{
			"success":        true,
			"result":         "CLAIMED",
			"reward_credits": earned,
			"rewardCredits":  earned,
		}
		if len(codes) > 0 {
			out["redemption_codes"] = codes
		}
		if len(notes) > 0 {
			out["message"] = strings.Join(notes, "；")
		}
		return out, nil
	}
	if len(codes) > 0 {
		// Only coupon rows landed — a success that moves NO credits.
		out := map[string]any{
			"success":         true,
			"result":          "CLAIMED",
			"reward_credits":  float64(0),
			"rewardCredits":   float64(0),
			"benefit_kind":    "REDEMPTION_CODE",
			"redemption_code": codes[0],
			"message":         "已领取兑换券：" + strings.Join(codes, "、") + "（非积分奖励，请到官方活动页兑换）",
		}
		if len(notes) > 0 {
			out["message"] = out["message"].(string) + "；" + strings.Join(notes, "；")
		}
		return out, nil
	}
	if blocked {
		return map[string]any{
			"success": false,
			"result":  "BLOCKED",
			"message": "同人已领取（同一设备身份下的其他账号本轮已领，服务端按人去重）",
		}, nil
	}
	if already {
		return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
	}
	if len(notes) > 0 {
		// Every row answered with an eligibility verdict — surface the
		// first as the typed result instead of a bare "nothing" line.
		return map[string]any{
			"success":      false,
			"result":       "NOT_ELIGIBLE",
			"failure_code": failureCode,
			"message":      strings.Join(notes, "；"),
		}, nil
	}
	return map[string]any{
		"success": false,
		"result":  "NOTHING_CLAIMABLE",
		"message": campaignIdleDiagnosis(status, sa),
	}, nil
}

// isRedemptionKind reports whether a benefit kind is a coupon/redemption
// family reward (claimable from check-in; moves no credits — the caller must
// surface the code, see the v0.8.40 coupon verdicts).
// rewardReadDead reports whether a /reward read error carries the upstream's
// GRANT_NOT_FOUND verdict — the definitive server answer that this account
// has NO grant record for the campaign (a dead/expired round), not an
// unreadable face value. v0.8.43 (official-package forensics): the
// errorCode exists server-side only — neither official bundle (CN v0.4.3
// exe nor the intl deb's out/main/index.js) contains the string, the client
// renders whatever the server answers — so the verdict is matched from the
// captured body, and the panel names it as a closed campaign instead of
// offering the claim_unverified opt-in on a guaranteed no.
func rewardReadDead(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "GRANT_NOT_FOUND")
}

func isRedemptionKind(kind string) bool {
	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case "REDEMPTION_CODE", "REDEMPTION_COUPON", "COUPON":
		return true
	}
	return false
}

// mergeVerdictNotes appends per-row verdict notes to a result's message
// without clobbering an existing one (probe results may already carry an
// upstream message).
func mergeVerdictNotes(res map[string]any, notes []string) {
	if res == nil || len(notes) == 0 {
		return
	}
	m, _ := res["message"].(string)
	m = strings.TrimSpace(m)
	if m == "" {
		res["message"] = strings.Join(notes, "；")
		return
	}
	res["message"] = m + "；" + strings.Join(notes, "；")
}

// attemptViewDetailsClaims (v0.8.41) claims the CLAIMABLE VIEW_DETAILS rows
// the strict pass (claimableCampaigns) skips. Evidence: the official client's
// captured launch flow POSTs the same claim endpoint on a VIEW_DETAILS row
// (issue #27 finding 3: the bogo promo answered 200 CLAIMED) — a VIEW_DETAILS
// row is a claim the client performs, not a page-only chore. The claim
// response stays the authoritative eligibility oracle: whatever the upstream
// answers (grant / replay / dedup / typed refusal) is surfaced verbatim
// instead of the old passive "需在官方活动页完成领取" note.
//
// Policy per row (cap 5, mirroring the Pro flow's rewardProbeCap):
//   - GET .../reward reveals CREDITS / redemption-family / bare-amount
//     face value → claim it; grants aggregate like the multi-claim pass.
//   - reward reveals any other readable kind (subscription-shaped) → NOT
//     claimed from check-in (the Pro-upgrade flow owns those); noted.
//   - reward unreadable / face-value-less → claim only when the
//     claim_unverified opt-in is on (same consent gate as the Pro flow,
//     blindClaimCampaign semantics).
//
// Returns (landed, notes): landed is a performCampaignCheckin-shaped success
// aggregate when a claim actually granted credits or produced a code, nil
// otherwise; notes carry one verdict per attempted row for the diagnosis.
func attemptViewDetailsClaims(sa *storedAuth, status *campaignStatusResponse) (map[string]any, []string) {
	if status == nil {
		return nil, nil
	}
	const maxRows = 5
	notes := make([]string, 0, 2)
	earned := float64(0)
	creditsClaimed := false
	codes := make([]string, 0, 2)
	attempted := 0
	now := time.Now().Unix()
	for i := range status.Campaigns {
		if attempted >= maxRows {
			break
		}
		c := &status.Campaigns[i]
		if !strings.EqualFold(strings.ToUpper(strings.TrimSpace(c.ActionType)), "VIEW_DETAILS") {
			continue
		}
		if !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
			continue
		}
		if (c.StartAt > 0 && now < c.StartAt) || (c.EndAt > 0 && now > c.EndAt) {
			continue
		}
		attempted++
		kind, amount := "", int64(0)
		body, rerr := fetchCampaignReward(sa, c.CampaignID)
		if rerr == nil {
			kind, amount = rewardBenefit(body)
		}
		kUpper := strings.ToUpper(strings.TrimSpace(kind))
		readable := rerr == nil && (kUpper == "CREDITS" || isRedemptionKind(kUpper) || (kUpper == "" && amount > 0))
		if rerr == nil && !readable && kUpper != "" {
			// Subscription/other-shaped reward — the Pro flow owns
			// these; check-in must not ride a subscription claim.
			notes = append(notes, fmt.Sprintf("%s（奖励类型 %s，归 Pro/订阅流程，签到不代领）", c.CampaignKey, kind))
			continue
		}
		if !readable {
			// v0.8.43: GRANT_NOT_FOUND is the upstream's authoritative
			// "no grant record for this account" — a dead round, not a
			// face-value problem. Name it and move on; the opt-in hint
			// would only promise a claim the server already refused.
			if rerr != nil && rewardReadDead(rerr) {
				notes = append(notes, fmt.Sprintf("%s（活动已失效：上游 GRANT_NOT_FOUND，本账号无此活动的发放记录）", c.CampaignKey))
				continue
			}
			if !claimUnverifiedEnabled() {
				if rerr != nil {
					notes = append(notes, fmt.Sprintf("%s（面值不可读：%s；配置 claim_unverified 后签到可代领）", c.CampaignKey, truncateRedacted(rerr.Error(), 80)))
				} else {
					notes = append(notes, fmt.Sprintf("%s（reward 无面值；配置 claim_unverified 后签到可代领）", c.CampaignKey))
				}
				continue
			}
		}
		res, err := claimCampaignByID(sa, c)
		if err != nil {
			notes = append(notes, campaignTitle(c)+": "+err.Error())
			continue
		}
		if success, _ := res["success"].(bool); success {
			rc, _ := res["reward_credits"].(float64)
			if rc > 0 {
				earned += rc
				creditsClaimed = true
			} else if amount > 0 && (kUpper == "CREDITS" || kUpper == "") {
				// The reward probe knew the face value but the
				// claim body carried none (v0.8.39 synthetic-row
				// case) — trust the read-only probe.
				earned += float64(amount)
				creditsClaimed = true
			}
			if code, ok := res["redemption_code"].(string); ok && strings.TrimSpace(code) != "" {
				codes = append(codes, code)
			}
			continue
		}
		switch r, _ := res["result"].(string); r {
		case "ALREADY_CLAIMED":
			notes = append(notes, campaignTitle(c)+"：已领取过")
		case "BLOCKED":
			notes = append(notes, campaignTitle(c)+"：同人已领取（服务端按人去重）")
		case "NOT_ELIGIBLE":
			if m, ok := res["message"].(string); ok && m != "" {
				notes = append(notes, campaignTitle(c)+"："+m)
			} else if fc, ok := res["failure_code"].(string); ok && fc != "" {
				notes = append(notes, campaignTitle(c)+"：暂不可领取（"+fc+"）")
			}
		default:
			if m, ok := res["message"].(string); ok && m != "" {
				notes = append(notes, campaignTitle(c)+": "+truncateRedacted(m, 100))
			} else {
				notes = append(notes, campaignTitle(c)+"：领取未成功")
			}
		}
	}
	if !creditsClaimed && len(codes) == 0 {
		return nil, notes
	}
	out := map[string]any{
		"success":        true,
		"result":         "CLAIMED",
		"reward_credits": earned,
		"rewardCredits":  earned,
	}
	if len(codes) > 0 {
		out["redemption_code"] = codes[0]
		out["redemption_codes"] = codes
		out["benefit_kind"] = "REDEMPTION_CODE"
		out["message"] = "已领取兑换券：" + strings.Join(codes, "、") + "（非积分奖励，请到官方活动页兑换）"
	}
	if len(notes) > 0 {
		mergeVerdictNotes(out, notes)
	}
	return out, notes
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
