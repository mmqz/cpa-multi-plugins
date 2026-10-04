//go:build live

// billing_live_test.go — real-credential verification that the plugin's
// production billing path returns actual credit data. This is the pre-release
// gate for billing EOF fixes: it exercises billingCall → billingCallOnce →
// hostHTTPDo (codebuddy.ai direct bypass → hostHTTPDoDirect) → pooled/rescue
// transports with the resilient dialer — the exact code path a deployed .so
// runs.
//
// Run:
//
//	WB_JWT=... WB_UID=... go test -tags live -run TestLiveBilling -v ./plugins/workbuddy
//
// Skipped unless -tags live AND WB_JWT are set, so normal CI stays hermetic.
package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLiveBilling_GetUserResource(t *testing.T) {
	jwt := os.Getenv("WB_JWT")
	if jwt == "" {
		t.Skip("WB_JWT not set — live billing verification skipped")
	}
	uid := os.Getenv("WB_UID")

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: jwt, Domain: "www.codebuddy.ai", Region: "intl", LoginPlatform: "ide"},
		Account: storedAccount{UID: uid},
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}

	data, err := billingCall(sa, "/v2/billing/meter/get-user-resource", body)
	if err != nil {
		t.Fatalf("live billing call failed: %v", err)
	}

	// billingCall returns the apiEnvelope data member. The Intl meter API
	// nests one level deeper: {"Response":{"Data":{"TotalDosage":...}}}.
	var payload struct {
		Response struct {
			Data struct {
				TotalDosage int               `json:"TotalDosage"`
				TotalCount  int               `json:"TotalCount"`
				Accounts    []json.RawMessage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode response: %v — raw head: %.400s", err, string(data))
	}
	t.Logf("LIVE OK: TotalDosage=%d TotalCount=%d accounts=%d",
		payload.Response.Data.TotalDosage, payload.Response.Data.TotalCount, len(payload.Response.Data.Accounts))
	if payload.Response.Data.TotalCount == 0 {
		t.Fatalf("expected non-empty resource list, got TotalCount=0 (raw: %.400s)", string(data))
	}
	if payload.Response.Data.TotalDosage < 0 {
		t.Fatalf("unexpected negative TotalDosage: %d", payload.Response.Data.TotalDosage)
	}
}

// TestLiveBilling_DialStageEvidence additionally runs the staged probe against
// the real host so the test log records what each stage looks like from this
// network (useful as a baseline when comparing field reports).
func TestLiveBilling_DialStageEvidence(t *testing.T) {
	uid := os.Getenv("WB_UID")
	_ = uid
	suffix := billingNetDiagSuffix("https://www.codebuddy.ai")
	if suffix == "" {
		t.Fatal("expected non-empty diag suffix for a hostname base")
	}
	t.Logf("net-diag: %s", suffix)
}
