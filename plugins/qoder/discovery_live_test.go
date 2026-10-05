//go:build live

// discovery_live_test.go — one-shot evidence hunt: can a filtered deployment
// (derived identity on Intl) learn the CURRENT daily round's UUID from any
// surface that does not require the device-targeted campaigns list?
// Probes are read-only GETs plus the campaignUrl shell body dump.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLiveDiscoverySurfaces(t *testing.T) {
	sa := liveSA(t)
	base := billingBaseFor(sa)

	get := func(tag, url string, machine bool) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Logf("[%s] build: %v", tag, err)
			return
		}
		billingHeaders(req, sa)
		if machine {
			mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
			attachMachineIdentityHeaders(req, &mi)
		}
		resp, err := hostHTTPDo(req)
		if err != nil {
			t.Logf("[%s] transport: %v", tag, err)
			return
		}
		t.Logf("[%s] http %d body=%s", tag, resp.StatusCode, truncateRedacted(string(resp.Body), 400))
	}

	// The activity page shell — dump enough to see what its JS loads.
	req, _ := http.NewRequest(http.MethodGet, base+"/growth-page/activity-iframe", nil)
	billingHeaders(req, sa)
	if resp, err := hostHTTPDo(req); err == nil {
		t.Logf("[shell] http %d body=%s", resp.StatusCode, string(resp.Body))
	}

	// Public catalog guesses (no /me, marketing faces).
	for _, p := range []string{
		"/sash/api/v1/campaigns",
		"/sash/api/v1/placements",
		"/sash/api/v1/me/placements",
		"/sash/api/v1/campaign-placements",
		"/sash/api/v1/me/campaign-summaries",
	} {
		get("catalog"+p, base+p, false)
	}

	// Key-resolving probes on the CURRENT daily key (reward/claim need
	// uuids, but maybe other sub-resources resolve keys like
	// client_launch_26/limited-number does).
	st, err := fetchCampaignStatus(sa)
	if err == nil {
		for i := range st.Campaigns {
			c := &st.Campaigns[i]
			if k := c.CampaignKey; strings.HasPrefix(k, "act-") {
				get("key-status-"+k, base+"/sash/api/v1/me/campaigns/"+k, false)
				get("key-limited-"+k, base+"/sash/api/v1/me/campaigns/"+k+"/limited-number", false)
				get("key-grants-"+k, base+"/sash/api/v1/me/campaigns/"+k+"/grants", false)
				break
			}
		}
	}

	// Grant/ledger surfaces that might carry campaign references.
	for _, p := range []string{
		"/sash/api/v1/me/grants",
		"/sash/api/v1/me/credits/ledger",
		"/sash/api/v1/me/benefits",
		fmt.Sprintf("%s%s", base, "/sash/api/v1/me/rewards"),
	} {
		get("ledger"+p, p, false)
	}
}
