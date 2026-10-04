package main

import (
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/pool"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// TestQuotaExhaustedKnown pins the trust boundary of the proactive
// midnight-cooldown (v0.12.66): a parsed 0 is the upstream's real answer
// (0.12.65 guarantees unknown never stamps 0), but ANY known non-zero source
// vetoes the exhaustion, and a summary with nothing known is not evidence.
func TestQuotaExhaustedKnown(t *testing.T) {
	tests := []struct {
		name string
		sum  upstream.UsageSummary
		want bool
	}{
		{
			name: "remain known zero",
			sum:  upstream.UsageSummary{RemainKnown: true, Remain: 0},
			want: true,
		},
		{
			name: "credits pool known zero",
			sum:  upstream.UsageSummary{RemainKnown: false, CreditsPool: upstream.CreditsPoolInfo{Known: true, Remain: 0}},
			want: true,
		},
		{
			name: "both known zero",
			sum:  upstream.UsageSummary{RemainKnown: true, Remain: 0, CreditsPool: upstream.CreditsPoolInfo{Known: true, Remain: 0}},
			want: true,
		},
		{
			name: "remain non-zero vetoes",
			sum:  upstream.UsageSummary{RemainKnown: true, Remain: 5, CreditsPool: upstream.CreditsPoolInfo{Known: true, Remain: 0}},
			want: false,
		},
		{
			name: "credits pool non-zero vetoes",
			sum:  upstream.UsageSummary{RemainKnown: true, Remain: 0, CreditsPool: upstream.CreditsPoolInfo{Known: true, Remain: 12}},
			want: false,
		},
		{
			name: "unlimited remain is not exhaustion",
			sum:  upstream.UsageSummary{RemainKnown: true, Remain: -1},
			want: false,
		},
		{
			name: "nothing known is no evidence",
			sum:  upstream.UsageSummary{RemainKnown: false},
			want: false,
		},
	}
	for _, tc := range tests {
		if got := quotaExhaustedKnown(tc.sum); got != tc.want {
			t.Errorf("%s: quotaExhaustedKnown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestApplyCooldownOnPlanLimitMidnightWindow: the 1005/4008 path now cools
// until the next local midnight, not a fixed 12h — the resume instant the
// panel reports must be midnight and inside the next 24h.
func TestApplyCooldownOnPlanLimitMidnightWindow(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-mid"})
	applyCooldownOn(p, "u-mid", upstream.ErrPlanLimit)

	st, ok := p.Status("u-mid")
	if !ok || !st.Cooling {
		t.Fatalf("plan-limit error must cool the account (status=%+v ok=%v)", st, ok)
	}
	until := st.Until
	if until.Hour() != 0 || until.Minute() != 0 {
		t.Fatalf("resume instant %s must be local midnight", until)
	}
	if d := time.Until(until); d <= 0 || d > 24*time.Hour {
		t.Fatalf("resume window %v out of (0, 24h]", d)
	}
	if got := p.Pick(); got != nil {
		t.Fatal("cooled account must not be picked")
	}
}
