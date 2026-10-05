//go:build live

// checkin_live_test.go — run the PLUGIN's real check-in path against the real
// upstream, exactly the way production runs it. In a `go test` binary
// hostBridgeAvailable() is false, so hostHTTPDo routes through
// hostHTTPDoDirect → sharedHTTPClient (HTTP/2 disabled) — which is the SAME
// transport v0.8.51+ uses for billing/campaigns in production. The billing
// cookie handshake, machine identity derivation, campaigns read, bypass
// probe and claim POST below are all the plugin's own production functions,
// not a reimplementation.
//
// What this test answers (field report 2026-10-05, "init 还是有概率不能签到
// / 今日无可签领权益"): for each pass it dumps
//
//  1. the credits snapshot (fetchUserResource),
//  2. the RAW campaigns envelope via the plugin's fetchCampaignStatusOnce
//     (identity source, showCampaign flag, campaignUrl, every row's
//     id/key/actionType/claimStatus/benefit/unavailableReason),
//  3. the plugin's own verdict (performCheckinCall → performCampaignCheckin)
//     including every note the diagnosis attached,
//  4. what the panel's init would show (fetchCheckinStatus),
//  5. the post-claim credits delta.
//
// so a "今日暂无可领取权益" verdict can be traced to the exact server answer
// that produced it.
//
// Run:
//
//	QD_TOKEN=dt-... [QD_REGION=cn|intl] [QD_UID=...] [QD_LOOPS=3] [QD_GAP=15] \
//	  go test -tags live -run TestLiveCheckin_PluginPath -v ./plugins/qoder
//
// Skipped unless -tags live AND QD_TOKEN are set, so normal CI stays hermetic.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func liveSA(t *testing.T) *storedAuth {
	t.Helper()
	token := strings.TrimSpace(os.Getenv("QD_TOKEN"))
	if token == "" {
		t.Skip("QD_TOKEN not set — live check-in verification skipped")
	}
	region := normalizeRegion(os.Getenv("QD_REGION"))
	return &storedAuth{
		Auth: storedTokens{
			AccessToken: token,
			Domain:      domainForRegion(region),
			Region:      region,
		},
		Account: storedAccount{UID: strings.TrimSpace(os.Getenv("QD_UID"))},
	}
}

// dumpCampaignRows renders the parsed campaigns list the way the decision
// code sees it — one line per row, every field the gates read.
func dumpCampaignRows(t *testing.T, tag string, st *campaignStatusResponse) {
	t.Helper()
	if st == nil {
		t.Logf("[%s] campaigns: <nil status>", tag)
		return
	}
	t.Logf("[%s] envelope: showCampaign=%v claimable=%v campaignUrl=%q rows=%d",
		tag, st.ShowCampaign, st.Claimable, st.CampaignURL, len(st.Campaigns))
	for i := range st.Campaigns {
		c := &st.Campaigns[i]
		benefit := "<nil>"
		if c.Benefit != nil {
			benefit = fmt.Sprintf("%s+%d", c.Benefit.Kind, c.Benefit.Amount)
		}
		t.Logf("[%s] row[%d] id=%s key=%s actionType=%q claimStatus=%q benefit=%s unavailable=%q start=%d end=%d achKey=%q achDone=%v",
			tag, i, c.CampaignID, c.CampaignKey, c.ActionType, c.ClaimStatus, benefit, c.UnavailableReason, c.StartAt, c.EndAt, c.RequiredAchievementKey, c.AchievementCompleted)
	}
}

// dumpVerdict renders the plugin's normalized verdict map, keys sorted for
// stable logs.
func dumpVerdict(t *testing.T, tag string, res map[string]any, err error) {
	t.Helper()
	if err != nil {
		t.Logf("[%s] verdict: transport error: %v", tag, err)
		return
	}
	if res == nil {
		t.Logf("[%s] verdict: nil", tag)
		return
	}
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	// deterministic order
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		b, _ := json.Marshal(res[k])
		parts = append(parts, fmt.Sprintf("%s=%s", k, string(b)))
	}
	t.Logf("[%s] verdict: %s", tag, strings.Join(parts, " "))
}

// dumpCredits renders a creditsSummary in the panel's terms.
func dumpCredits(t *testing.T, tag string, cr *creditsSummary) {
	t.Helper()
	if cr == nil {
		t.Logf("[%s] credits: <nil>", tag)
		return
	}
	pk := make([]string, 0, len(cr.Packages))
	for _, p := range cr.Packages {
		pk = append(pk, fmt.Sprintf("%s %d/%d(used %d)", p.Name, p.Remain, p.Size, p.Used))
	}
	t.Logf("[%s] credits: total_remain=%d total_used=%d packs=[%s]",
		tag, cr.TotalRemain, cr.TotalUsed, strings.Join(pk, " | "))
}

// liveMineCampaignURL fetches the envelope's campaignUrl with the browser
// billing dialect and scans the body for act-YYYYMMDD-NNN ids — evidence of
// round ids the filtered list hides but the activity surface knows.
func liveMineCampaignURL(t *testing.T, sa *storedAuth, url string) {
	t.Helper()
	if strings.TrimSpace(url) == "" {
		t.Logf("[activity] no campaignUrl in envelope — skip")
		return
	}
	req, err := newLiveGet(url, sa)
	if err != nil {
		t.Logf("[activity] build: %v", err)
		return
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		t.Logf("[activity] fetch %s: %v", url, err)
		return
	}
	body := string(resp.Body)
	ids := liveActIDRegexp.FindAllString(body, -1)
	uniq := map[string]bool{}
	dedup := make([]string, 0, len(ids))
	for _, id := range ids {
		if !uniq[id] {
			uniq[id] = true
			dedup = append(dedup, id)
		}
	}
	t.Logf("[activity] campaignUrl %s → http %d, %d bytes, act- ids found: %v",
		url, resp.StatusCode, len(resp.Body), dedup)
}

var liveActIDRegexp = regexp.MustCompile(`act-\d{8}-\d+`)

// newLiveGet builds a GET with the same headers billingHeaders attaches
// (Bearer + cookies), so the activity surface sees the account session.
func newLiveGet(url string, sa *storedAuth) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	return req, nil
}

// TestLiveClaimDialect answers ONE question that decides the intl fallback
// design: do the claim/reward endpoints resolve the act-YYYYMMDD-NNN KEY in
// the URL, or only the campaign UUID? All POSTs here hit an account that has
// ALREADY claimed today, so a resolvable id answers ALREADY_CLAIMED (409 or
// replayed body) — zero double-claim risk. A garbage id shows the not-found
// shape for contrast.
func TestLiveClaimDialect(t *testing.T) {
	sa := liveSA(t)

	// Pull the current list so the test uses THIS region's real round.
	st, err := fetchCampaignStatus(sa)
	if err != nil {
		t.Fatalf("campaigns: %v", err)
	}
	dumpCampaignRows(t, "list", st)
	var daily *campaign
	for i := range st.Campaigns {
		c := &st.Campaigns[i]
		if at := strings.ToUpper(strings.TrimSpace(c.ActionType)); at == "CLAIM_BENEFIT" || at == "" {
			daily = c
			break
		}
	}
	if daily == nil {
		t.Fatalf("no daily-shaped row in list — cannot run dialect test")
	}
	t.Logf("daily row: id=%s key=%s claimStatus=%s", daily.CampaignID, daily.CampaignKey, daily.ClaimStatus)

	post := func(tag, path string) {
		req, err := http.NewRequest(http.MethodPost, billingBaseFor(sa)+path, strings.NewReader("{}"))
		if err != nil {
			t.Logf("[%s] build: %v", tag, err)
			return
		}
		billingHeaders(req, sa)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hostHTTPDo(req)
		if err != nil {
			t.Logf("[%s] transport: %v", tag, err)
			return
		}
		t.Logf("[%s] http %d body=%s", tag, resp.StatusCode, truncateRedacted(string(resp.Body), 300))
	}
	get := func(tag, path string) {
		req, err := http.NewRequest(http.MethodGet, billingBaseFor(sa)+path, nil)
		if err != nil {
			t.Logf("[%s] build: %v", tag, err)
			return
		}
		billingHeaders(req, sa)
		resp, err := hostHTTPDo(req)
		if err != nil {
			t.Logf("[%s] transport: %v", tag, err)
			return
		}
		t.Logf("[%s] http %d body=%s", tag, resp.StatusCode, truncateRedacted(string(resp.Body), 300))
	}

	get("reward-by-uuid", "/sash/api/v1/me/campaigns/"+daily.CampaignID+"/reward")
	get("reward-by-key", "/sash/api/v1/me/campaigns/"+daily.CampaignKey+"/reward")
	post("claim-by-uuid", "/sash/api/v1/me/campaigns/"+daily.CampaignID+"/claim")
	post("claim-by-key", "/sash/api/v1/me/campaigns/"+daily.CampaignKey+"/claim")
	post("claim-by-garbage", "/sash/api/v1/me/campaigns/act-20991231-999/claim")
	get("limited-number", "/sash/api/v1/me/campaigns/client_launch_26/limited-number")
}

var _ = liveActIDRegexp // kept for the campaignUrl miner above

// TestLiveCheckin_PluginPath is the whole production check-in path, live,
// with raw evidence per leg. See the file header for usage.
func TestLiveCheckin_PluginPath(t *testing.T) {
	sa := liveSA(t)
	loops := 1
	if v := strings.TrimSpace(os.Getenv("QD_LOOPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			loops = n
		}
	}
	gap := 15 * time.Second
	if v := strings.TrimSpace(os.Getenv("QD_GAP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			gap = time.Duration(n) * time.Second
		}
	}

	region := authRegion(sa)
	t.Logf("=== live check-in, region=%s base=%s loops=%d gap=%s ===",
		region, billingBaseFor(sa), loops, gap)

	for pass := 1; pass <= loops; pass++ {
		t.Logf("----- pass %d/%d -----", pass, loops)

		cr, err := fetchUserResource(sa)
		dumpCredits(t, fmt.Sprintf("pass%d before", pass), cr)
		if err != nil {
			t.Logf("[pass%d before] credits error: %v", pass, err)
		}

		// The plugin's own campaigns read (machine headers, launch sync,
		// memo seeding, no-machine retry — all inside).
		st, err := fetchCampaignStatus(sa)
		if err != nil {
			t.Logf("[pass%d list] fetchCampaignStatus error: %v", pass, err)
		} else {
			dumpCampaignRows(t, fmt.Sprintf("pass%d list", pass), st)
		}

		// What the bypass probe would fire with (memo state).
		if c, ok := roundMemo.probeFor(region, sa.Account.UID); ok {
			t.Logf("[pass%d memo] probe candidate id=%s key=%s", pass, c.CampaignID, c.CampaignKey)
		} else {
			t.Logf("[pass%d memo] no probe candidate (daily row never seen this deployment)", pass)
		}

		// The plugin's own verdict — the exact call checkinOneAccount makes.
		res, err := performCheckinCall(sa)
		dumpVerdict(t, fmt.Sprintf("pass%d checkin", pass), res, err)

		// Panel view (init renders this).
		ci, err := fetchCheckinStatus(sa)
		if err != nil {
			t.Logf("[pass%d panel] fetchCheckinStatus error: %v", pass, err)
		} else if ci != nil {
			t.Logf("[pass%d panel] TodayCheckedIn=%v Active=%v DailyCredit=%d Streak=%d",
				pass, ci.TodayCheckedIn, ci.Active, ci.DailyCredit, ci.StreakDays)
		}

		cr2, err := fetchUserResource(sa)
		dumpCredits(t, fmt.Sprintf("pass%d after", pass), cr2)
		if err == nil && cr != nil && cr2 != nil {
			t.Logf("[pass%d delta] %+d credits", pass, cr2.TotalRemain-cr.TotalRemain)
		}

		if pass < loops {
			time.Sleep(gap)
		}
	}
}

// TestLiveCampaignEnvelopeRaw fires ONE campaigns GET per dialect
// (machine-identity / no-machine) and prints the untouched JSON — the
// ground truth behind any filtering question.
func TestLiveCampaignEnvelopeRaw(t *testing.T) {
	sa := liveSA(t)

	out, miSource, hadFlag, err := fetchCampaignStatusOnce(sa, false)
	t.Logf("[machine] identity source=%s hadShowCampaign=%v err=%v", miSource, hadFlag, err)
	if err == nil {
		dumpCampaignRows(t, "machine", out)
		raw, _ := json.Marshal(out)
		t.Logf("[machine] RAW=%s", string(raw))
		if out.CampaignURL != "" {
			liveMineCampaignURL(t, sa, out.CampaignURL)
		}
	}

	out2, _, _, err2 := fetchCampaignStatusNoMachine(sa)
	t.Logf("[nomachine] err=%v", err2)
	if err2 == nil {
		dumpCampaignRows(t, "nomachine", out2)
		raw, _ := json.Marshal(out2)
		t.Logf("[nomachine] RAW=%s", string(raw))
	}
}
