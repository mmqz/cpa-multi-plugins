package upstream

import "testing"

func poolPack(q PackQuota, u PackUsage) EntitlementPack {
	p := EntitlementPack{}
	p.EntitlementBaseInfo.Quota = q
	p.Usage = u
	return p
}

func i64p(v int64) *int64 { return &v }

func f64p(v float64) *float64 { return &v }

// v0.12.34: 官方 cashier 同口径积分池（Σ max(credits_limit-usage,0)，-1 不限）。
func TestCreditsPoolUsage(t *testing.T) {
	cases := []struct {
		name          string
		packs         []EntitlementPack
		billing       bool
		wantRemain    int64
		wantKnown     bool
		wantUnlimited bool
	}{
		{"no evidence no billing", []EntitlementPack{poolPack(PackQuota{}, PackUsage{})}, false, 0, false, false},
		{"billing without fields → known 0", []EntitlementPack{poolPack(PackQuota{}, PackUsage{})}, true, 0, true, false},
		{"limit minus usage", []EntitlementPack{poolPack(PackQuota{CreditsLimit: i64p(200)}, PackUsage{CreditsAmount: f64p(50)})}, false, 150, true, false},
		{"float usage rounds", []EntitlementPack{poolPack(PackQuota{CreditsLimit: i64p(200)}, PackUsage{CreditsAmount: f64p(49.4)})}, false, 151, true, false},
		{"missing usage → full", []EntitlementPack{poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{})}, false, 150, true, false},
		{"missing limit contributes 0", []EntitlementPack{poolPack(PackQuota{}, PackUsage{CreditsAmount: f64p(30)}), poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{})}, false, 150, true, false},
		{"unlimited wins", []EntitlementPack{poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{}), poolPack(PackQuota{CreditsLimit: i64p(-1)}, PackUsage{})}, false, -1, true, true},
		{"negative clamps to 0", []EntitlementPack{poolPack(PackQuota{CreditsLimit: i64p(10)}, PackUsage{CreditsAmount: f64p(99)})}, false, 0, true, false},
	}
	for _, c := range cases {
		got := CreditsPoolUsage(c.packs, c.billing)
		if got.Remain != c.wantRemain || got.Known != c.wantKnown || got.Unlimited != c.wantUnlimited {
			t.Errorf("%s: got %+v, want remain=%d known=%v unlimited=%v", c.name, got, c.wantRemain, c.wantKnown, c.wantUnlimited)
		}
	}
}

func TestCreditsPoolUsageFilters(t *testing.T) {
	promo := poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{})
	promo.EntitlementBaseInfo.ProductType = 3 // PROMO_CODE → 过滤
	if g := CreditsPoolUsage([]EntitlementPack{promo}, false); g.Known {
		t.Fatalf("promo pack should be filtered: %+v", g)
	}
	hidden := poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{})
	hidden.EntitlementBaseInfo.IsHide = true
	if g := CreditsPoolUsage([]EntitlementPack{hidden}, false); g.Known {
		t.Fatalf("hidden pack should be filtered: %+v", g)
	}
	cancelled := poolPack(PackQuota{CreditsLimit: i64p(150)}, PackUsage{})
	three := 3
	cancelled.EntitlementBaseInfo.Status = &three
	if g := CreditsPoolUsage([]EntitlementPack{cancelled}, false); g.Known {
		t.Fatalf("cancelled pack should be filtered: %+v", g)
	}
}
