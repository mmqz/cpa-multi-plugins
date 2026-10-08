//go:build live

// intl_surface_live_test.go — decision-tree evidence for the intl daily-row
// absence (field report u673e7fcc 2026-10-08: v0.8.61 still reports
// "今日暂无可领取权益" while the account holds 200 credits in 赠送/签到额度).
//
// Evidence so far (TestLiveCampaignEnvelopeRaw): BOTH dialects (derived
// machine identity AND no-machine) return the IDENTICAL envelope — exactly
// one VIEW_DETAILS row (act-20260901-493, the BOGO marketing campaign,
// claimStatus=CLAIMED, benefit=nil) and claimable=false. That kills the
// v0.8.61 "intl gateway filters rows by identity dialect" hypothesis: the
// server answer does not depend on the identity at all for this account.
//
// This file walks the remaining decision tree, every leg through the
// plugin's OWN production functions (no reimplementation, no Python):
//
//	leg A: baseline list via fetchCampaignStatusOnce (machine dialect)
//	leg B: surface-open with the plugin's billing dialect (billingHeaders —
//	       the v0.8.46 openCampaignSurface behavior that v0.8.47 removed),
//	       then an immediate re-list → does the daily row appear?
//	leg C: re-list in the activity page's own web XHR dialect (cookies from
//	       the plugin's billing jar + Origin/Referer of the shell page; one
//	       pass cookies+Bearer, one pass cookies-only) — the WebView bundle
//	       would call /me/campaigns this way, and the server's surface-open
//	       tracking may key on THIS shape rather than the Bearer call.
//	leg D: legacy daily-check-in status on intl (READ-ONLY,
//	       /sash/api/v1/me/daily-check-in/status) — record what intl says.
//	leg E: reward read on the VIEW_DETAILS row via the plugin's own
//	       fetchCampaignReward — face-value evidence.
//	leg F: limited-number launch signal via the plugin's own
//	       fetchLimitedNumber (client_launch_26 on intl).
//
// Run:
//
//	QD_TOKEN=dt-... QD_REGION=intl QD_UID=... \
//	  go test -tags live -run TestLiveIntlSurface -v ./plugins/qoder
//
// Skipped unless -tags live AND QD_TOKEN are set (hermetic CI unaffected).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func intlSurfaceSA(t *testing.T) *storedAuth {
	t.Helper()
	token := strings.TrimSpace(os.Getenv("QD_TOKEN"))
	if token == "" {
		t.Skip("QD_TOKEN not set — live intl surface verification skipped")
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

func intlListOnce(t *testing.T, tag string, sa *storedAuth, forceIdentity bool) *campaignStatusResponse {
	t.Helper()
	out, miSource, hadFlag, err := fetchCampaignStatusOnce(sa, forceIdentity)
	if err != nil {
		t.Logf("[%s] list error: %v", tag, err)
		return nil
	}
	t.Logf("[%s] identity=%s hadShowCampaign=%v", tag, miSource, hadFlag)
	dumpCampaignRows(t, tag, out)
	return out
}

// intlGet fires one GET through the plugin's transport with a header-mutator
// hook so each leg can speak its own dialect.
func intlGet(t *testing.T, tag, url string, sa *storedAuth, mutate func(*http.Request)) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Logf("[%s] build: %v", tag, err)
		return 0, nil
	}
	if mutate != nil {
		mutate(req)
	} else {
		billingHeaders(req, sa)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		t.Logf("[%s] transport: %v", tag, err)
		return 0, nil
	}
	return resp.StatusCode, resp.Body
}

func TestLiveIntlSurface(t *testing.T) {
	sa := intlSurfaceSA(t)
	base := billingBaseFor(sa)
	region := authRegion(sa)
	t.Logf("=== intl surface decision tree, region=%s base=%s ===", region, base)

	// leg A — baseline.
	t.Logf("--- leg A: baseline list (machine dialect) ---")
	outA := intlListOnce(t, "A baseline", sa, false)
	if outA != nil && outA.CampaignURL == "" {
		t.Logf("[A] no campaignUrl in envelope — surface-open legs cannot run")
	}

	// leg B — surface-open with the plugin's billing dialect, then re-list.
	if outA != nil && outA.CampaignURL != "" {
		t.Logf("--- leg B: surface-open (billing dialect) then re-list ---")
		st, body := intlGet(t, "B open", outA.CampaignURL, sa, nil)
		t.Logf("[B open] http %d, %d bytes", st, len(body))
		if st == 200 {
			snippet := string(body)
			if len(snippet) > 600 {
				snippet = snippet[:600]
			}
			t.Logf("[B open] shell head: %s", snippet)
		}
		intlListOnce(t, "B re-list", sa, false)
	}

	// leg C — re-list in the activity page's web XHR dialect. The WebView
	// bundle authenticates with the web-session cookies (and maybe the
	// Bearer) and carries Origin/Referer of the activity page. The server's
	// surface-open tracking may key on this shape, not on the Bearer call.
	if outA != nil && outA.CampaignURL != "" {
		t.Logf("--- leg C: re-list in web XHR dialect ---")
		web := func(withBearer bool) func(*http.Request) {
			return func(req *http.Request) {
				applyBillingSessionHeaders(req, sa)
				req.Header.Set("Accept", "application/json, text/plain, */*")
				req.Header.Set("Origin", "https://qoder.com")
				req.Header.Set("Referer", outA.CampaignURL)
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 QoderWork/0.4.3")
				if withBearer {
					req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
				}
			}
		}
		st, body := intlGet(t, "C xhr bearer+cookies", campaignsURL(sa), sa, web(true))
		t.Logf("[C xhr bearer+cookies] http %d rows-body=%s", st, truncateRedacted(string(body), 700))
		st2, body2 := intlGet(t, "C xhr cookies-only", campaignsURL(sa), sa, web(false))
		t.Logf("[C xhr cookies-only] http %d rows-body=%s", st2, truncateRedacted(string(body2), 700))

		// C2 — the full WebView walk: shell → bundle URL → bundle → XHR list.
		t.Logf("--- leg C2: full webview walk (shell → bundle → xhr list) ---")
		_, shell := intlGet(t, "C2 shell", outA.CampaignURL, sa, web(false))
		bundleURL := intlExtractBundleURL(string(shell))
		t.Logf("[C2] bundle url: %s", bundleURL)
		if bundleURL != "" {
			bst, bbody := intlGet(t, "C2 bundle", bundleURL, sa, func(req *http.Request) {
				req.Header.Set("Referer", outA.CampaignURL)
			})
			t.Logf("[C2 bundle] http %d, %d bytes", bst, len(bbody))
			apiHits := intlScanBundleAPIs(string(bbody))
			t.Logf("[C2 bundle] api paths in bundle: %v", apiHits)
			st3, body3 := intlGet(t, "C2 xhr after walk", campaignsURL(sa), sa, web(true))
			t.Logf("[C2 xhr after walk] http %d rows-body=%s", st3, truncateRedacted(string(body3), 700))
		}
	}

	// leg D — legacy daily-check-in status on intl (READ-ONLY).
	t.Logf("--- leg D: legacy daily-check-in status (read-only) ---")
	stD, bodyD := intlGet(t, "D status", base+"/sash/api/v1/me/daily-check-in/status", sa, nil)
	t.Logf("[D status] http %d body=%s", stD, truncateRedacted(string(bodyD), 400))

	// leg E — reward read on every visible row (plugin production fn).
	if outA != nil {
		t.Logf("--- leg E: reward read per row (fetchCampaignReward) ---")
		for i := range outA.Campaigns {
			c := &outA.Campaigns[i]
			body, err := fetchCampaignReward(sa, c.CampaignID)
			if err != nil {
				t.Logf("[E reward] %s id=%s: err=%v", c.CampaignKey, c.CampaignID, err)
				continue
			}
			kind, amount := rewardBenefit(body)
			b, _ := json.Marshal(body)
			t.Logf("[E reward] %s id=%s: kind=%s amount=%d body=%s", c.CampaignKey, c.CampaignID, kind, amount, truncateRedacted(string(b), 300))
		}
	}

	// leg F — limited-number launch signal (plugin production fn).
	t.Logf("--- leg F: limited-number (fetchLimitedNumber) ---")
	has, err := fetchLimitedNumber(sa)
	t.Logf("[F limited] has=%v err=%v", has, err)

	// leg G — re-list one final time so any delayed surface-open effect
	// (server-side flip after leg B/C GETs) shows up.
	t.Logf("--- leg G: final re-list after all opens ---")
	time.Sleep(2 * time.Second)
	intlListOnce(t, "G final", sa, false)
}

// intlExtractBundleURL pulls the first script/bundle URL out of the activity
// shell HTML (evidence leg for the surface-open walk).
func intlExtractBundleURL(shell string) string {
	for _, marker := range []string{`src="`, `href="`} {
		for _, part := range strings.Split(shell, marker)[1:] {
			u := part[:strings.Index(part, `"`)]
			if strings.Contains(u, ".js") || strings.Contains(u, "alicdn") {
				if strings.HasPrefix(u, "//") {
					return "https:" + u
				}
				return u
			}
		}
	}
	return ""
}

// intlScanBundleAPIs lists every /sash|/api path literal found in a bundle —
// the definitive inventory of what the activity page can call.
func intlScanBundleAPIs(bundle string) []string {
	if len(bundle) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, kw := range []string{"/sash/api", "/growth/api", "/api/v1/", "/api/v2/", "daily-check-in", "checkin", "check-in"} {
		for start := 0; ; {
			idx := strings.Index(bundle[start:], kw)
			if idx < 0 {
				break
			}
			at := start + idx
			end := at
			for end < len(bundle) && (isBundlePathByte(bundle[end])) {
				end++
			}
			frag := bundle[at:end]
			if len(frag) > 4 && !seen[frag] {
				seen[frag] = true
				out = append(out, frag)
			}
			start = at + len(kw)
			if len(out) >= 24 {
				return out
			}
		}
	}
	return out
}

func isBundlePathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '/', '-', '_', '.', '?', '=', '&', '{', '}', '$', ':':
		return true
	}
	return false
}

var _ = io.Discard // keep imports honest if legs are trimmed
var _ = fmt.Sprintf
