//go:build live

// intl_dialect_live_test.go — dialect isolation for the intl daily-row
// absence (u673e7fcc, 2026-10-08).
//
// Established by TestLiveIntlSurface:
//   - billing dialect (Cosy + cookies + UA Qoder, ±machine headers) → BOGO
//     row only, claimable=false;
//   - web XHR dialect (no Cosy at all) → a DIFFERENT response shape entirely
//     (uid echo, empty list) — the endpoint has two faces;
//   - issue #27 补充实测 (2026-10-01): an intl account got the daily
//     act-20260930-156 CLAIM_BENEFIT row with the MINIMAL dialect
//     Bearer + Cosy-ClientType: 10 + Cosy-Version: 0.4.3 + UA Qoder — no
//     cookies, no machine headers.
//
// The plugin sends Cosy-Version "0.3.4" (billing.go) — a full minor behind
// the official 0.4.3. Legs below isolate EVERY delta between the plugin's
// request and the proven-working minimal dialect:
//
//	H1: exact #27 dialect (Bearer, Cosy-ClientType 10, Cosy-Version 0.4.3,
//	    UA Qoder) — no cookies, no machine, no Accept.
//	H2: H1 + plugin cookie jar (does the web-session cookie change it?)
//	H3: H1 + Accept: application/json
//	H4: plugin billingHeaders but Cosy-Version bumped to 0.4.3 (cookies on,
//	    machine off)
//	H5: H4 + derived machine identity headers (full plugin dialect, 0.4.3)
//	H6: control — H1 but Cosy-Version 0.3.4 (expect: no daily row, matching
//	    the plugin's current behavior)
//	H7: H1 but Cosy-Version 0.5.0 (newer-than-official probe)
//
// Any leg that shows a CLAIM_BENEFIT row answers the root cause with the
// exact header delta. Raw envelopes are logged for every leg.
//
// Run:
//
//	QD_TOKEN=dt-... QD_REGION=intl QD_UID=... \
//	  go test -tags live -run TestLiveIntlDialect -v ./plugins/qoder
package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestLiveIntlDialect(t *testing.T) {
	sa := intlSurfaceSA(t)
	url := campaignsURL(sa)
	t.Logf("=== intl dialect isolation, base=%s uid=%s ===", billingBaseFor(sa), sa.Account.UID)

	minimal := func(ver string) func(*http.Request) {
		return func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
			req.Header.Set("Cosy-ClientType", "10")
			req.Header.Set("Cosy-Version", ver)
			req.Header.Set("User-Agent", "Qoder")
		}
	}
	bumped := func(machine bool) func(*http.Request) {
		return func(req *http.Request) {
			billingHeaders(req, sa)
			req.Header.Set("Cosy-Version", "0.4.3")
			if machine {
				mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
				attachMachineIdentityHeaders(req, &mi)
			}
		}
	}

	legs := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"H1 minimal 0.4.3 (issue#27 dialect)", minimal("0.4.3")},
		{"H2 minimal 0.4.3 + cookies", func(req *http.Request) {
			minimal("0.4.3")(req)
			applyBillingSessionHeaders(req, sa)
		}},
		{"H3 minimal 0.4.3 + accept", func(req *http.Request) {
			minimal("0.4.3")(req)
			req.Header.Set("Accept", "application/json")
		}},
		{"H4 billing + cookies, ver 0.4.3, no machine", bumped(false)},
		{"H5 billing + cookies + machine, ver 0.4.3", bumped(true)},
		{"H6 control minimal 0.3.4", minimal("0.3.4")},
		{"H7 minimal 0.5.0", minimal("0.5.0")},
	}
	for _, leg := range legs {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("%s: %v", leg.name, err)
		}
		leg.mutate(req)
		resp, err := hostHTTPDo(req)
		if err != nil {
			t.Logf("[%s] transport: %v", leg.name, err)
			continue
		}
		t.Logf("[%s] http %d", leg.name, resp.StatusCode)
		if resp.StatusCode == 200 {
			dumpCampaignRows(t, leg.name, parseCampaignEnvelope(resp.Body))
		} else {
			t.Logf("[%s] body=%s", leg.name, truncateRedacted(string(resp.Body), 240))
		}
	}
}

// parseCampaignEnvelope decodes a campaigns response for the row dumper.
func parseCampaignEnvelope(raw []byte) *campaignStatusResponse {
	var out campaignStatusResponse
	_ = json.Unmarshal(raw, &out)
	return &out
}
