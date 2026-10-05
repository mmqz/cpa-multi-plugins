package main

// checkin_seed_dialect_test.go — v0.8.58: the round-id SEED chain (the
// bypass probe's last-resort id source for permanently list-filtered
// deployments) and the derived-identity desktop dialect (Cosy-MachineOS /
// Cosy-MachineHostname shaped like the official Windows client instead of
// the impossible "x86_64_linux + container id" tell). Hermetic: no network.

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRoundSeedChain: builtin seed for CN, none for Intl until the operator
// configures one, operator values win over builtin, act- keys never ride the
// claim path.
func TestRoundSeedChain(t *testing.T) {
	prev := checkinRoundSeeds
	t.Cleanup(func() {
		checkinRoundSeedsMu.Lock()
		checkinRoundSeeds = prev
		checkinRoundSeedsMu.Unlock()
	})
	checkinRoundSeedsMu.Lock()
	checkinRoundSeeds = nil
	checkinRoundSeedsMu.Unlock()

	// 1. builtin: CN has the live-verified seed, Intl does not.
	id, src, ok := roundSeedFor(regionCN)
	if !ok || id != builtinRoundSeeds[regionCN].CampaignID || src != "内置已验证轮次" {
		t.Fatalf("cn builtin seed = %q %q %v", id, src, ok)
	}
	if _, _, ok := roundSeedFor(regionIntl); ok {
		t.Fatal("intl must have no builtin seed (no UUID ever captured)")
	}

	// 2. operator config wins, act- keys and malformed ids are dropped.
	applyRoundSeeds("intl=01A0ABCD-0000-0000-0000-0000000000AB, cn=act-20260930-295, jp=bad")
	if id, _, ok := roundSeedFor(regionIntl); !ok || id != "01a0abcd-0000-0000-0000-0000000000ab" {
		t.Fatalf("operator intl seed = %q %v, want the configured UUID (lowercased)", id, ok)
	}
	if _, _, ok := roundSeedFor(regionCN); !ok {
		t.Fatal("cn seed must still resolve via builtin after the operator's act- key entry is dropped")
	}
	if got := operatorRoundSeed(regionCN); got != "" {
		t.Fatalf("act- key must never ride the claim path, got %q", got)
	}

	// 3. clearing the config restores the builtin.
	applyRoundSeeds("")
	if id, _, ok := roundSeedFor(regionCN); !ok || id != builtinRoundSeeds[regionCN].CampaignID {
		t.Fatalf("cn builtin seed must survive a config clear, got %q %v", id, ok)
	}
}

// TestColdMemoIntlNote: with no memo and no seed (the pure-Intl container
// case) the diagnosis must name the exact remedies instead of the blind
// "none" line.
func TestColdMemoIntlNote(t *testing.T) {
	prevLast := roundProbeLast
	prevMemo := roundMemo
	t.Cleanup(func() {
		roundProbeLast = prevLast
		roundMemo = prevMemo
	})
	roundProbeLast = map[string]roundProbeLatch{}
	roundMemo = &campaignRoundMemo{perAccount: map[string]campaignRoundEntry{}, perRegion: map[string]campaignRoundEntry{}}
	checkinRoundSeedsMu.Lock()
	checkinRoundSeeds = nil
	checkinRoundSeedsMu.Unlock()

	newBillingServer(t, "intl", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
		},
	})
	res, err := performCheckinCall(&storedAuth{Auth: storedTokens{AccessToken: "dt-intl-cold", Region: "intl"}})
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "checkin_round_seeds") || !strings.Contains(msg, "QD_UMID_BIN") {
		t.Fatalf("intl cold-memo diagnosis must name the remedies, got %q", msg)
	}
	if result, _ := res["result"].(string); result != "NOTHING_CLAIMABLE" {
		t.Fatalf("result = %v, want NOTHING_CLAIMABLE", result)
	}
}

// TestDerivedIdentityDesktopDialect: a derived identity must present the
// official Windows desktop dialect — the official client never ships a Linux
// bridge, so "x86_64_linux" + a raw container hostname is a structural tell
// no official client can produce (the Intl gateway's strictness is the field
// case; CN demonstrably ignores it).
func TestDerivedIdentityDesktopDialect(t *testing.T) {
	id := derivedMachineIdentity("uid-dialect")
	if id.MachineOS != "x86_64_win32" {
		t.Fatalf("derived MachineOS = %q, want the official desktop value x86_64_win32", id.MachineOS)
	}
	if !strings.HasPrefix(id.MachineHostname, "DESKTOP-") || len(id.MachineHostname) != len("DESKTOP-")+7 {
		t.Fatalf("derived MachineHostname = %q, want DESKTOP-XXXXXXX shape", id.MachineHostname)
	}
	if id.MachineHostname == "DESKTOP-QODER" && osHostnameLooksReal() {
		t.Fatalf("derived MachineHostname fell back to the hub constant although a hostname exists")
	}
	// Stable across calls (the identity must not rotate per request).
	if again := derivedMachineIdentity("uid-dialect"); again.MachineHostname != id.MachineHostname || again.MachineToken != id.MachineToken {
		t.Fatal("derived identity must be stable per uid")
	}
	// Distinct uids keep distinct token bodies (account isolation, hub
	// doctrine) while sharing the host's shaped hostname.
	if other := derivedMachineIdentity("uid-other"); other.MachineToken == id.MachineToken {
		t.Fatal("distinct uids must not share a machine token")
	}
	if other := derivedMachineIdentity("uid-other"); other.MachineHostname != id.MachineHostname {
		t.Fatal("hostname is machine-level: same host, same shaped name")
	}
	// The token/type/code shapes stay the official ones (v0.8.44 contract).
	if !strings.HasPrefix(id.MachineToken, "P1gA") || len(id.MachineToken) != 88 {
		t.Fatalf("machine token shape broken: len=%d prefix ok=%v", len(id.MachineToken), strings.HasPrefix(id.MachineToken, "P1gA"))
	}
	if len(id.MachineType) != 18 || id.MachineType[8:10] != "91" {
		t.Fatalf("machine type shape broken: %q", id.MachineType)
	}
	if len(id.MachineCode) != 18 || id.MachineCode[8:10] != "00" {
		t.Fatalf("machine code shape broken: %q", id.MachineCode)
	}
}

// TestSeedProbeNoteMentionsSource: a seed-fired verdict carries the source
// note so the panel never presents a seed claim as a list-derived one.
func TestSeedProbeNoteMentionsSource(t *testing.T) {
	prevLast := roundProbeLast
	prevMemo := roundMemo
	t.Cleanup(func() {
		roundProbeLast = prevLast
		roundMemo = prevMemo
	})
	roundProbeLast = map[string]roundProbeLatch{}
	roundMemo = &campaignRoundMemo{perAccount: map[string]campaignRoundEntry{}, perRegion: map[string]campaignRoundEntry{}}

	seedID := builtinRoundSeeds[regionCN].CampaignID
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":true,"campaigns":[]}`
		},
		"/sash/api/v1/me/campaigns/" + seedID + "/claim": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
	})
	res, err := performCheckinCall(cnAuth())
	if err != nil {
		t.Fatalf("performCheckinCall: %v", err)
	}
	if success, _ := res["success"].(bool); !success {
		t.Fatalf("seed-fired grant must surface as success, got %v", res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "内置已验证轮次") {
		t.Fatalf("seed note must ride the verdict, got %q", msg)
	}
	// The latch must be armed so the seed does not re-POST every tick.
	if _, ok := roundProbeLast["cn::"+seedID]; !ok {
		if len(roundProbeLast) == 0 {
			t.Fatal("seed probe verdict must arm the cooldown latch")
		}
	}
	_ = time.Now() // keep time imported for future assertions
}

// osHostnameLooksReal reports whether the test host has any hostname at all
// (the constant fallback only fires when os.Hostname errors).
func osHostnameLooksReal() bool {
	h, err := os.Hostname()
	return err == nil && strings.TrimSpace(h) != ""
}
