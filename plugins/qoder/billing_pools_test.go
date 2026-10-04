package main

import (
	"testing"
)

// TestSummarizeQuotaResponse_IncludesDedicatedAndOrgPools pins the v0.8.17
// fix (fork libo0118/qoder-custom review): dedicated resource packages
// (check-in / campaign grants) and the organization shared pool must join
// the totals and appear as their own package rows — the old two-pool sum
// silently dropped them and under-reported real credits.
func TestSummarizeQuotaResponse_IncludesDedicatedAndOrgPools(t *testing.T) {
	org := 55.5
	q := quotaUsageResponse{
		UserQuota: struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
			Unit      string  `json:"unit"`
		}{Total: 1000, Used: 200, Remaining: 800},
		AddOnQuota: struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		}{Total: 100, Used: 40, Remaining: 60},
		DedicatedResourcePackages: []quotaPool{
			{Name: "签到包", Total: 50, Used: 10, Remaining: 40},
			{Name: "", Total: 30, Used: 0, Remaining: 30}, // unnamed → fallback name
		},
		OrgResourcePackage: &quotaPool{Name: "团队池", Total: 500, Used: 100, Remaining: org},
	}

	sum := summarizeQuotaResponse(q)

	wantRemain := int64(800 + 60 + 40 + 30 + 55)
	wantUsed := int64(200 + 40 + 10 + 0 + 100)
	wantSize := int64(1000 + 100 + 50 + 30 + 500)
	if sum.TotalRemain != wantRemain || sum.TotalUsed != wantUsed || sum.TotalSize != wantSize {
		t.Fatalf("totals: remain=%d (want %d) used=%d (want %d) size=%d (want %d)",
			sum.TotalRemain, wantRemain, sum.TotalUsed, wantUsed, sum.TotalSize, wantSize)
	}
	if sum.PackCount != 5 {
		t.Fatalf("PackCount = %d, want 5 (base + addon + 2 dedicated + org)", sum.PackCount)
	}
	if len(sum.Packages) != 5 {
		t.Fatalf("len(Packages) = %d, want 5", len(sum.Packages))
	}
	// The named dedicated row and the org row surface verbatim.
	if sum.Packages[2].Name != "签到包" || sum.Packages[2].Remain != 40 {
		t.Fatalf("dedicated row #1: %+v", sum.Packages[2])
	}
	if sum.Packages[4].Name != "团队池" || sum.Packages[4].Remain != 55 {
		t.Fatalf("org row: %+v", sum.Packages[4])
	}
	// The unnamed dedicated package gets its fallback label.
	if sum.Packages[3].Name != "专用资源包 2" || sum.Packages[3].Remain != 30 {
		t.Fatalf("dedicated row #2 (unnamed): %+v", sum.Packages[3])
	}
}

// TestSummarizeQuotaResponse_LegacyTwoPoolShape: a response without the new
// pools must produce the exact legacy shape (totals unchanged, 2 packages).
func TestSummarizeQuotaResponse_LegacyTwoPoolShape(t *testing.T) {
	q := quotaUsageResponse{
		UserQuota: struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
			Unit      string  `json:"unit"`
		}{Total: 100, Used: 20, Remaining: 80},
		AddOnQuota: struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		}{Total: 10, Used: 2, Remaining: 8},
	}
	sum := summarizeQuotaResponse(q)
	if sum.TotalRemain != 88 || sum.TotalUsed != 22 || sum.TotalSize != 110 || sum.PackCount != 2 || len(sum.Packages) != 2 {
		t.Fatalf("legacy shape drift: %+v", sum)
	}
}
