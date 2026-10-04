// campaign_test.go pins the Intl check-in contract added in v0.8.18 (fork
// bfSan/qoder-cpa-plugin review, endpoint contract verified upstream): the
// per-region capability table and the campaign → checkinSummary mapping the
// panel renders. The claim HTTP path itself is exercised only end-to-end;
// here we pin the pure selection/summary logic that decides what the user sees.
package main

import "testing"

func TestCapabilitiesForRegion(t *testing.T) {
	// v0.12.80: BOTH regions ride the campaign contract — upstream disabled
	// the legacy CN daily-check-in (claim 409s on unclaimed days, grants
	// nothing). The CN flip is the fix for the "Qoder CN 不能签到" field
	// report; keep this test pinned so no future edit silently restores the
	// dead legacy path.
	cn := capabilitiesForRegion("cn")
	if cn.Contract != checkinContractCampaign {
		t.Fatalf("CN contract = %d, want campaign (legacy daily-check-in is DISABLED upstream)", cn.Contract)
	}
	if !cn.ProUpgrade {
		t.Fatal("CN must keep the Pro-upgrade contract")
	}
	intl := capabilitiesForRegion("intl")
	if intl.Contract != checkinContractCampaign {
		t.Fatalf("Intl contract = %d, want campaign", intl.Contract)
	}
	if intl.ProUpgrade {
		t.Fatal("Intl has no Pro-upgrade contract — must be a capability fact")
	}
	if !intl.Checkin {
		t.Fatal("Intl must report a real check-in capability")
	}
	// "global" normalizes onto the Intl dialect (same as authRegion).
	if got := capabilitiesForRegion("global"); got.Contract != checkinContractCampaign {
		t.Fatalf("global contract = %d, want campaign", got.Contract)
	}
}

func TestClaimableCampaignSelection(t *testing.T) {
	status := &campaignStatusResponse{
		ShowCampaign: true,
		Campaigns: []campaign{
			{CampaignID: "c-done", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "CREDITS", Amount: 50}},
			{CampaignID: "c-window", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", StartAt: 9000000000, EndAt: 10000000000, Benefit: &campaignBen{Kind: "CREDITS", Amount: 70}},
			{CampaignID: "c-live", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}},
			{CampaignID: "c-other", ActionType: "SURVEY", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "CREDITS", Amount: 999}},
		},
	}
	got := claimableCampaign(status)
	if got == nil {
		t.Fatal("claimable campaign not found")
	}
	if got.CampaignID != "c-live" {
		t.Fatalf("picked %q, want the in-window claimable c-live (expired/future windows and non-CLAIM_BENEFIT rows must be skipped)", got.CampaignID)
	}
	if amount := campaignCredit(got); amount != 100 {
		t.Fatalf("credit = %d, want 100", amount)
	}
	if claimed := claimedCampaign(status); claimed == nil || claimed.CampaignID != "c-done" {
		t.Fatalf("claimed detection failed: %+v", claimed)
	}
	// Non-CREDITS benefits count as zero credit but remain claimable.
	status.Campaigns = []campaign{{CampaignID: "c-disc", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "DISCOUNT", Amount: 5}}}
	if c := claimableCampaign(status); c == nil {
		t.Fatal("non-credit benefit must still be claimable")
	} else if amount := campaignCredit(c); amount != 0 {
		t.Fatalf("non-credit amount = %d, want 0", amount)
	}
}

func TestCampaignCheckinSummaryMapping(t *testing.T) {
	// Claimable now → active with the reward previewed.
	sum := campaignCheckinSummary(&campaignStatusResponse{
		ShowCampaign: true,
		Campaigns:    []campaign{{CampaignID: "c1", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}}},
	})
	if !sum.Active || sum.TodayCheckedIn || sum.DailyCredit != 100 {
		t.Fatalf("claimable summary = %+v", sum)
	}
	// Claimed row still listed → shows as checked-in today.
	sum = campaignCheckinSummary(&campaignStatusResponse{
		ShowCampaign: true,
		Campaigns:    []campaign{{CampaignID: "c1", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED", Benefit: &campaignBen{Kind: "CREDITS", Amount: 100}}},
	})
	if !sum.Active || !sum.TodayCheckedIn || sum.TodayCredit != 100 {
		t.Fatalf("claimed summary = %+v", sum)
	}
	// Claimed campaigns vanish from the list entirely — an inactive summary is
	// a normal end-of-day state (reason=none upstream), never an error path.
	sum = campaignCheckinSummary(&campaignStatusResponse{})
	if sum.Active {
		t.Fatal("empty campaign list must not report active")
	}
	if sum.ActivityName == "" {
		t.Fatal("summary must carry a display name for the panel")
	}
}

func TestCheckinSummaryShapesStayCompatible(t *testing.T) {
	// The campaign mapping must fill the same fields the CN daily summary
	// fills (panel rendering is dialect-agnostic).
	cn := &checkinSummary{Active: true, ActivityName: "每日签到"}
	intl := campaignCheckinSummary(&campaignStatusResponse{
		ShowCampaign: true,
		Campaigns:    []campaign{{CampaignID: "c1", ActionType: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", Benefit: &campaignBen{Kind: "CREDITS", Amount: 30}}},
	})
	if cn.ActivityName == "" {
		t.Fatal("CN summary must carry a display name")
	}
	if intl.ActivityName == "" {
		t.Fatal("intl summary must carry a display name")
	}
	if intl.DailyCredit != 30 {
		t.Fatalf("intl DailyCredit = %d, want 30", intl.DailyCredit)
	}
}
