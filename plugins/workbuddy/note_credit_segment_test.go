// note_credit_segment_test.go — v0.9.50: pins the credits-segment extraction
// against legacy usage-note residue.
//
// Field note (2026-10-04, INTL account swd1232): the base note carried
// "Tok 18.7M ｜ 该账号暂未解析出额度..." — residue of a pre-【用量】-marker
// usage segment that creditSegmentFromNote resurrected as "credits" and the
// 80-char cap then truncated mid-sentence. Two guards:
//
//  1. everything from the 【用量】 marker onward is dropped BEFORE the
//     " · " split (the usage segment contains its own " · " separators, so
//     the marker-prefix check alone only skipped its first piece);
//  2. legacy pre-marker residue ("Tok X ｜ …" shapes) is skipped — the
//     fullwidth bar and Tok prefix never occur in a credits segment.
package main

import (
	"strings"
	"testing"
)

func TestCreditSegmentDropsUsageMarkerSegment(t *testing.T) {
	// A live-shaped note: base + current-format usage segment (with its own
	// " · " separators inside).
	note := "INTL · 余410 已用0 池410 · 【用量】(自2026-10-03) 今日 请求228 · Tok 15.9M ｜ 该账号暂未解析出额度窗口 ｜ 累计 请求321 · 成功率46% · Tok 45.8M"
	got := creditSegmentFromNote(note)
	want := "余410 已用0 池410"
	if got != want {
		t.Fatalf("creditSegmentFromNote = %q, want %q (usage residue must not leak into the credits segment)", got, want)
	}
}

func TestCreditSegmentDropsLegacyPreMarkerResidue(t *testing.T) {
	// A pre-v0.9.42 note: usage content with no 【用量】 marker at all.
	note := "INTL · 余410 已用0 池410 · Tok 18.7M ｜ 该账号暂未解析出额度窗口"
	got := creditSegmentFromNote(note)
	want := "余410 已用0 池410"
	if got != want {
		t.Fatalf("creditSegmentFromNote = %q, want %q (legacy Tok/｜ residue must be retired)", got, want)
	}
}

func TestCreditSegmentKeepsPlainCredits(t *testing.T) {
	cases := map[string]string{
		"INTL · 余410 已用0 池410": "余410 已用0 池410",
		"CN · 耗尽 · 余0 已用500":   "耗尽 · 余0 已用500",
		"Global · 余12 已用3 池15": "余12 已用3 池15",
		"INTL · 积分未知":          "",
	}
	for note, want := range cases {
		if got := creditSegmentFromNote(note); got != want {
			t.Errorf("creditSegmentFromNote(%q) = %q, want %q", note, got, want)
		}
	}
}

func TestDisplayNoteNoResidueRegression(t *testing.T) {
	// End-to-end: displayNoteWithPrev fed the mangled on-disk note must
	// produce a clean BASE (no Tok/｜ residue before the 【用量】 marker).
	// Re-attaching the CURRENT usage segment after the marker is correct —
	// that is the usage writer's own live content.
	prev := "INTL · 余410 已用0 池410 · Tok 18.7M ｜ 该账号暂未解析出额度... · 【用量】(自2026-10-03) 今日 请求228 · Tok 15.9M ｜ 该账号暂未解析出额度窗口 ｜ 累计 请求321 · 成功率46% · Tok 45.8M"
	sa := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}, Account: storedAccount{Nickname: "swd1232"}}
	out := displayNoteWithPrev(sa, &creditsSummary{TotalRemain: 410, TotalSize: 410, PackCount: 4, Packages: []packageSummary{{Name: "Bonus Pack", Remain: 250, Size: 250}}}, false, prev)
	base := out
	if idx := strings.Index(out, usageSegmentMarker); idx >= 0 {
		base = out[:idx]
	}
	if strings.Contains(base, "Tok ") || strings.Contains(base, "该账号暂未解析出额度") || strings.Contains(base, "｜") {
		t.Fatalf("base note still carries usage residue: %q", base)
	}
	if !strings.HasPrefix(base, "INTL · 余410 已用0 池410") {
		t.Fatalf("displayNoteWithPrev lost the live credit segment: %q", base)
	}
}
