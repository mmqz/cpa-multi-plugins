// growth_accept_triage_test.go covers the v0.12.65 accept-failure triage
// (Coding2API growth_runner 2026-09-20 parity): upstream "does not require
// acceptance" answers are normal responses rather than failures,
// "prerequisite not met: <code>" rejections are grouped by reason (a fresh
// account has every task gated by first_buddy — one summary line instead of
// a dozen identical noise lines), and only the remainder counts as real
// failures.
package main

import "testing"

func TestGrowthPrerequisiteOf(t *testing.T) {
	cases := []struct {
		msg   string
		code  string
		match bool
	}{
		{"prerequisite not met: first_buddy", "first_buddy", true},
		{"Prerequisite Not Met: first_buddy", "first_buddy", true}, // 大小不敏感
		{"prerequisite not met: first_buddy.", "first_buddy", true},
		{"prerequisite not met: first_buddy, please complete it first", "first_buddy", true},
		{"prerequisite not met:", "", true},   // 空 code 仍是前置类
		{"prerequisite not met:x", "x", true}, // 宽容缺空格
		{"task does not require acceptance", "", false},
		{"session expired", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		code, ok := growthPrerequisiteOf(c.msg)
		if ok != c.match || (ok && code != c.code) {
			t.Errorf("growthPrerequisiteOf(%q) = (%q, %v), want (%q, %v)",
				c.msg, code, ok, c.code, c.match)
		}
	}
}

func TestGrowthPrerequisiteLabel(t *testing.T) {
	if got := growthPrerequisiteLabel("first_buddy"); got == "" || got == "first_buddy" {
		t.Errorf("first_buddy label = %q, want an actionable phrase", got)
	}
	if got := growthPrerequisiteLabel("mystery_task"); got != "mystery_task" {
		t.Errorf("unknown code label = %q, want code passthrough", got)
	}
}

func TestGrowthAcceptNeedsNoAccept(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"task does not require acceptance", true},
		{"Task does not require acceptance.", true},
		{"  TASK DOES NOT REQUIRE ACCEPTANCE  ", true},
		{"prerequisite not met: first_buddy", false},
		{"internal error", false},
		{"", false},
	}
	for _, c := range cases {
		if got := growthAcceptNeedsNoAccept(c.msg); got != c.want {
			t.Errorf("growthAcceptNeedsNoAccept(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
