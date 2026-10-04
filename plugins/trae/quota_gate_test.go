package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/pool"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// testEntUsage builds an EntUsageResult from the same wire shape client_test
// uses for the real endpoint (basic_usage_limit/basic_usage_amount pairs).
func testEntUsage(t *testing.T, packs string) *upstream.EntUsageResult {
	t.Helper()
	var r upstream.EntUsageResult
	if err := json.Unmarshal([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[`+packs+`]}`), &r); err != nil {
		t.Fatalf("fixture unmarshal: %v", err)
	}
	return &r
}

func gateFixture(t *testing.T, uid, reason string) *pool.Pool {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid})
	if reason != "" {
		p.Cooldown(uid, pool.CoolPlan, pool.UntilNextMidnight(), reason)
	}
	return p
}

// TestQuotaGateDecisionMatrix pins the v0.12.67 executor gate: a frozen
// credential gets exactly one ent-usage probe — refill thaws, known-zero
// refuses before any chat call, unknown/failed probes fail open.
func TestQuotaGateDecisionMatrix(t *testing.T) {
	const uid = "u1"
	freezeReason := pool.ReasonQuotaChat + " — resumes at local midnight (2026-01-02 00:00)"

	cases := []struct {
		name      string
		freeze    bool
		probe     func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error)
		want      gateDecision
		wantProbe int // probe invocations
	}{
		{
			name:   "not frozen proceeds without probing",
			freeze: false,
			probe: func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error) {
				t.Fatal("probe must not run for unfrozen credentials")
				return nil
			},
			want: gateProceed, wantProbe: 0,
		},
		{
			name:   "refill thaws and proceeds",
			freeze: true,
			probe: func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error) {
				return func(*auth.Auth) (*upstream.EntUsageResult, error) {
					return testEntUsage(t, `{"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":2000}},"usage":{"basic_usage_amount":300}}`), nil
				}
			},
			want: gateProceed, wantProbe: 1,
		},
		{
			name:   "known zero refuses before chat",
			freeze: true,
			probe: func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error) {
				return func(*auth.Auth) (*upstream.EntUsageResult, error) {
					return testEntUsage(t, `{"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":2000}},"usage":{"basic_usage_amount":2000}}`), nil
				}
			},
			want: gateRefuse, wantProbe: 1,
		},
		{
			name:   "unknown remain fails open",
			freeze: true,
			probe: func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error) {
				return func(*auth.Auth) (*upstream.EntUsageResult, error) {
					return testEntUsage(t, ``), nil // no packs → remain unknown
				}
			},
			want: gateProceed, wantProbe: 1,
		},
		{
			name:   "probe error fails open",
			freeze: true,
			probe: func(t *testing.T) func(*auth.Auth) (*upstream.EntUsageResult, error) {
				return func(*auth.Auth) (*upstream.EntUsageResult, error) {
					return nil, errors.New("ent usage down")
				}
			},
			want: gateProceed, wantProbe: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := gateFixture(t, uid, map[bool]string{true: freezeReason, false: ""}[tc.freeze])
			calls := 0
			probe := func(a *auth.Auth) (*upstream.EntUsageResult, error) {
				calls++
				return tc.probe(t)(a)
			}
			if got := quotaGateProbe(p, &auth.Auth{UID: uid}, probe); got != tc.want {
				t.Fatalf("decision=%v, want %v", got, tc.want)
			}
			if calls != tc.wantProbe {
				t.Fatalf("probe calls=%d, want %d", calls, tc.wantProbe)
			}
		})
	}
}

// TestQuotaGateRefillThawsPool: the thaw branch must actually lift the
// freeze in the pool (ReenableIfCredits), not just bypass the gate once —
// that is what makes CPA reset-quota / midnight refill end-to-end effective.
func TestQuotaGateRefillThawsPool(t *testing.T) {
	const uid = "u1"
	p := gateFixture(t, uid, pool.ReasonQuotaScan)
	if frozen, _, _ := p.QuotaFreeze(uid); !frozen {
		t.Fatal("fixture must start frozen")
	}
	probe := func(*auth.Auth) (*upstream.EntUsageResult, error) {
		return testEntUsage(t, `{"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":2000}},"usage":{"basic_usage_amount":500}}`), nil
	}
	if got := quotaGateProbe(p, &auth.Auth{UID: uid}, probe); got != gateProceed {
		t.Fatalf("refill decision=%v, want proceed", got)
	}
	if frozen, _, _ := p.QuotaFreeze(uid); frozen {
		t.Fatal("refill must thaw the pool freeze, not bypass the gate once")
	}
	st, _ := p.Status(uid)
	if st.Cooling {
		t.Fatalf("pool status still cooling: %+v", st)
	}
}

// TestQuotaGateRefusalLeavesFreezeIntact: a known-zero refusal keeps the
// midnight freeze (self-correcting) — the next pick probes again instead of
// silently recovering.
func TestQuotaGateRefusalLeavesFreezeIntact(t *testing.T) {
	const uid = "u1"
	p := gateFixture(t, uid, pool.ReasonQuotaChat)
	before, _, _ := p.QuotaFreeze(uid)
	probe := func(*auth.Auth) (*upstream.EntUsageResult, error) {
		return testEntUsage(t, `{"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":100}},"usage":{"basic_usage_amount":100}}`), nil
	}
	if got := quotaGateProbe(p, &auth.Auth{UID: uid}, probe); got != gateRefuse {
		t.Fatalf("zero-remain decision=%v, want refuse", got)
	}
	after, _, _ := p.QuotaFreeze(uid)
	if !before || !after {
		t.Fatalf("freeze must persist across refusal: before=%v after=%v", before, after)
	}
}
