package pool

import (
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// TestUntilNextMidnightBounds pins the two wire edges of the resume window:
// the duration is always in (0, 24h] and lands on local midnight.
func TestUntilNextMidnightBounds(t *testing.T) {
	d := UntilNextMidnight()
	if d <= 0 || d > 24*time.Hour {
		t.Fatalf("UntilNextMidnight() = %v, want (0, 24h]", d)
	}
	resume := time.Now().Add(d)
	if resume.Hour() != 0 || resume.Minute() != 0 || resume.Second() > 1 {
		t.Fatalf("resume instant %s is not local midnight", resume)
	}
}

// TestCooldownUntilMidnightSemantics: a CoolPlan cooldown with the midnight
// duration makes the account unhealthy immediately and healthy again after
// the window (simulated by waiting past `until` via a shorter second entry —
// here we verify status fields, the expiry walk is covered by healthy()).
func TestCooldownUntilMidnightSemantics(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	d := UntilNextMidnight()
	p.Cooldown("u1", CoolPlan, d, "credits exhausted (0) — resumes at local midnight")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Cooling {
		t.Fatal("account should be cooling right after Cooldown")
	}
	if st.Disabled {
		t.Fatal("Cooldown must not disable — only disable does")
	}
	if st.Until.IsZero() || !st.Until.After(time.Now()) {
		t.Fatalf("until %v should be in the future", st.Until)
	}
	if st.Until.Hour() != 0 || st.Until.Minute() != 0 {
		t.Fatalf("until %s should be local midnight", st.Until)
	}
	// Pick must skip the cooling account.
	if got := p.Pick(); got != nil {
		t.Fatalf("Pick() returned a cooling account: %s", got.UID)
	}
}

// TestExhaustThenRefillUnfreezes: the full user story — credits hit 0
// (cooldown to midnight), upstream refills early (check-in scan sees >0),
// ReenableIfCredits lifts the window before midnight.
func TestExhaustThenRefillUnfreezes(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.Cooldown("u1", CoolPlan, UntilNextMidnight(), "credits exhausted (0) — resumes at local midnight")
	if got := p.Pick(); got != nil {
		t.Fatal("exhausted account must not be picked")
	}

	// Refill arrives (sign-in scan / manual /credits with remain > 0).
	p.ReenableIfCredits("u1", 42)
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatal("refill should lift the cooling window")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatal("refilled account should be picked again")
	}
}

// TestCooldownKeepsDisabled: Cooldown must not resurrect a disabled account
// (session-dead is a human decision), and ReenableIfCredits must not either.
func TestCooldownKeepsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead (401)")

	p.Cooldown("u1", CoolPlan, UntilNextMidnight(), "credits exhausted (0) — resumes at local midnight")
	p.ReenableIfCredits("u1", 99)
	if got := p.Pick(); got != nil {
		t.Fatal("disabled account must stay unpickable")
	}
}

// TestQuotaFreezeTruthTable pins the gate-consultation predicate (v0.12.67):
// only the two quota-exhaustion reasons with a live `until` count as frozen —
// soft 429 / error-threshold coolings must NOT trigger the executor gate.
func TestQuotaFreezeTruthTable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "chat"}) // 1005/4008 executor-path freeze
	p.Add(&auth.Auth{UID: "scan"}) // check-in-scan zero freeze
	p.Add(&auth.Auth{UID: "soft"}) // 60s soft cooldown — not a quota freeze
	p.Add(&auth.Auth{UID: "gone"}) // expired freeze

	p.Cooldown("chat", CoolPlan, UntilNextMidnight(),
		ReasonQuotaChat+" — resumes at local midnight (2026-01-02 00:00)")
	p.Cooldown("scan", CoolPlan, UntilNextMidnight(), ReasonQuotaScan)
	p.Cooldown("soft", CoolSoft, 60*time.Second, "soft rate limit (429)")
	p.Cooldown("gone", CoolPlan, -time.Minute,
		ReasonQuotaChat+" — resumes at local midnight (yesterday)")

	cases := []struct {
		uid    string
		frozen bool
	}{
		{"chat", true}, {"scan", true}, {"soft", false}, {"gone", false}, {"absent", false},
	}
	for _, tc := range cases {
		frozen, until, reason := p.QuotaFreeze(tc.uid)
		if frozen != tc.frozen {
			t.Errorf("uid=%s frozen=%v, want %v (reason=%q)", tc.uid, frozen, tc.frozen, reason)
		}
		if frozen && (until.IsZero() || reason == "") {
			t.Errorf("uid=%s frozen without until/reason", tc.uid)
		}
		if !frozen && (!until.IsZero() || reason != "") {
			t.Errorf("uid=%s not frozen but returned until/reason", tc.uid)
		}
	}
}

// TestIsQuotaFreezeReason: prefix matching tolerates the appended resume
// timestamp (executor path) and rejects every other cooldown copy.
func TestIsQuotaFreezeReason(t *testing.T) {
	yes := []string{
		ReasonQuotaChat,
		ReasonQuotaChat + " — resumes at local midnight (2026-01-02 00:00)",
		ReasonQuotaScan,
	}
	no := []string{
		"soft rate limit (429)",
		"not found (404)",
		"consecutive errors",
		"session dead (401)",
		"",
		"quota exhausted-ish but not really", // wrong prefix wording
	}
	for _, r := range yes {
		if !IsQuotaFreezeReason(r) {
			t.Errorf("IsQuotaFreezeReason(%q)=false, want true", r)
		}
	}
	for _, r := range no {
		if IsQuotaFreezeReason(r) {
			t.Errorf("IsQuotaFreezeReason(%q)=true, want false", r)
		}
	}
}

// TestReleaseManualUnfreeze: Release clears the freeze (and error counters)
// immediately — the panel 「解除冷却」 contract. A disabled account stays
// disabled: Release is a cooldown reset, not a session resurrection.
func TestReleaseManualUnfreeze(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolPlan, UntilNextMidnight(), ReasonQuotaScan)
	p.NoteError("u1", 1, time.Minute) // errCount=1 alongside the freeze
	p.Disable("u2", "session dead (401)")

	st, ok := p.Release("u1")
	if !ok {
		t.Fatal("Release(u1) reported missing account")
	}
	if st.Cooling || st.Until != (time.Time{}) || st.Reason != "" || st.ErrCount != 0 {
		t.Fatalf("Release left residue: %+v", st)
	}
	if st.Nickname != "nick" {
		t.Fatalf("Release status lost identity: %+v", st)
	}
	if frozen, _, _ := p.QuotaFreeze("u1"); frozen {
		t.Fatal("QuotaFreeze still reports frozen after Release")
	}

	// Disabled account: reported but NOT resurrected.
	st2, ok2 := p.Release("u2")
	if !ok2 || !st2.Disabled {
		t.Fatalf("Release(u2) ok=%v disabled=%v, want true/true", ok2, st2.Disabled)
	}
	if got := p.Pick(); got != nil && got.UID == "u2" {
		t.Fatal("Release must not make a disabled account pickable")
	}

	// Unknown uid: clean miss.
	if _, ok3 := p.Release("nope"); ok3 {
		t.Fatal("Release(nope) should report ok=false")
	}
}
