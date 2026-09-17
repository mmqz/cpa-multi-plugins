package main

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func parsePickResponse(t *testing.T, raw []byte) pluginapi.SchedulerPickResponse {
	t.Helper()
	var env struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatal("envelope not ok")
	}
	return env.Result
}

func resetActiveAuth(t *testing.T) {
	t.Helper()
	setActiveAuthID("")
	// Pre-v0.6.31 tests assume plugin handles routing; default mode is now off.
	// Flip to credits mode for the duration of each pick test so behavior stays
	// identical to before the scheduler_mode fix.
	restoreMode := setSchedulerMode(schedulerModeCredits)
	t.Cleanup(func() {
		setActiveAuthID("")
		restoreMode()
	})
}

func TestSchedulerPick_NonWorkbuddy_Defers(t *testing.T) {
	resetActiveAuth(t)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: "other",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "x", Provider: "other"},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if resp.Handled {
		t.Fatal("non-workbuddy candidates should defer")
	}
}

// TestSchedulerPick_OffMode_Defers covers the v0.6.31 fix: scheduler_mode=off
// must make the plugin decline to handle routing, even for workbuddy candidates.
func TestSchedulerPick_OffMode_Defers(t *testing.T) {
	setActiveAuthID("")
	restoreMode := setSchedulerMode(schedulerModeOff)
	t.Cleanup(func() {
		setActiveAuthID("")
		restoreMode()
	})
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-only", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if resp.Handled {
		t.Fatal("scheduler_mode=off should defer to built-in scheduler")
	}
}

func TestSchedulerPick_SingleCandidate_PicksIt(t *testing.T) {
	resetActiveAuth(t)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-only", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-only" {
		t.Fatalf("want wb-only handled, got %+v", resp)
	}
	if getActiveAuthID() != "wb-only" {
		t.Fatalf("active auth should stick to wb-only, got %q", getActiveAuthID())
	}
}

func TestSchedulerPick_PrefersPanelSelection(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-a", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 10, TotalSize: 10}})
	accountCache.Store("wb-b", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 500, TotalSize: 500}})
	defer func() {
		accountCache.Delete("wb-a")
		accountCache.Delete("wb-b")
	}()
	setActiveAuthID("wb-a")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-a", Provider: providerName},
			{ID: "wb-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-a" {
		t.Fatalf("want panel selection wb-a, got %+v", resp)
	}
}

func TestSchedulerPick_StaysOnExhaustedSelection(t *testing.T) {
	resetActiveAuth(t)
	// When selected is exhausted AND a non-exhausted candidate exists,
	// it should switch to the non-exhausted one and update activeAuthID.
	accountCache.Store("wb-exhausted", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 500, TotalSize: 500},
	})
	accountCache.Store("wb-ok", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 300, TotalUsed: 0, TotalSize: 300},
	})
	defer func() {
		accountCache.Delete("wb-exhausted")
		accountCache.Delete("wb-ok")
	}()
	setActiveAuthID("wb-exhausted")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-exhausted", Provider: providerName},
			{ID: "wb-ok", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-ok" {
		t.Fatalf("want switch to wb-ok, got %+v", resp)
	}
	if getActiveAuthID() != "wb-ok" {
		t.Fatalf("active should update to wb-ok, got %q", getActiveAuthID())
	}
}

func TestSchedulerPick_AllExhausted_KeepsCurrent(t *testing.T) {
	resetActiveAuth(t)
	// When ALL candidates are exhausted, keep current selection rather than
	// flip-flopping between exhausted accounts.
	accountCache.Store("wb-a", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100},
	})
	accountCache.Store("wb-b", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 200, TotalSize: 200},
	})
	defer func() {
		accountCache.Delete("wb-a")
		accountCache.Delete("wb-b")
	}()
	setActiveAuthID("wb-a")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-a", Provider: providerName},
			{ID: "wb-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-a" {
		t.Fatalf("want stay on wb-a (all exhausted), got %+v", resp)
	}
}

func TestSchedulerPick_SwitchesOnlyWhenSelectionGone(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-ok", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 300, TotalUsed: 0, TotalSize: 300},
	})
	defer accountCache.Delete("wb-ok")
	// Selected auth is NOT in candidates (host disabled it) → should switch.
	setActiveAuthID("wb-gone")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-ok", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-ok" {
		t.Fatalf("want switch to wb-ok, got %+v", resp)
	}
	if getActiveAuthID() != "wb-ok" {
		t.Fatalf("active should update to wb-ok, got %q", getActiveAuthID())
	}
}

func TestSchedulerPick_SkipsDisabledCandidates(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-live", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 50, TotalSize: 50},
	})
	defer accountCache.Delete("wb-live")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-off", Provider: providerName, Status: "disabled"},
			{ID: "wb-live", Provider: providerName, Status: "active"},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-live" {
		t.Fatalf("want wb-live, got %+v", resp)
	}
	// All disabled → defer
	raw2, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-off", Provider: providerName, Status: "disabled", Metadata: map[string]any{"disabled": true}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp2 := parsePickResponse(t, raw2)
	if resp2.Handled {
		t.Fatalf("all disabled should defer, got %+v", resp2)
	}
}

func resetFreeRoulette(t *testing.T) {
	t.Helper()
	freeRouletteMu.Lock()
	freeRoulette = &wbrr{mu: sync.Mutex{}}
	freeRouletteMu.Unlock()
	t.Cleanup(func() {
		freeRouletteMu.Lock()
		freeRoulette = &wbrr{mu: sync.Mutex{}}
		freeRouletteMu.Unlock()
	})
}

// Free models bypass credits entirely: an exhausted account may still receive
// traffic (promo models cost no paid credits), and the picker spreads across
// all enabled accounts.
func TestSchedulerPick_FreeModel_SpreadsAcrossAccounts(t *testing.T) {
	resetActiveAuth(t)
	resetFreeRoulette(t)
	// The exhausted account has no credits at all — should still be eligible.
	accountCache.Store("wb-exhausted", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 500, TotalSize: 500},
	})
	accountCache.Store("wb-ok", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 300, TotalSize: 300},
	})
	defer func() {
		accountCache.Delete("wb-exhausted")
		accountCache.Delete("wb-ok")
	}()

	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
			Model:    "deepseek-v4.1-flash",
			Provider: providerName,
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "wb-exhausted", Provider: providerName},
				{ID: "wb-ok", Provider: providerName},
				{ID: "wb-empty", Provider: providerName},
			},
		}))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		resp := parsePickResponse(t, raw)
		if !resp.Handled {
			t.Fatalf("free model should be handled, got %+v", resp)
		}
		counts[resp.AuthID]++
	}
	// Three accounts: round-robin means each appears twice across 6 picks.
	for _, id := range []string{"wb-exhausted", "wb-ok", "wb-empty"} {
		if counts[id] != 2 {
			t.Fatalf("round-robin: %s should be picked 2/6, got %d (%v)", id, counts[id], counts)
		}
	}
}

// Paid models keep the credits-aware picker: an exhausted account must not be
// used while a funded candidate exists.
func TestSchedulerPick_PaidModel_SkipsExhausted(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-exhausted", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100},
	})
	accountCache.Store("wb-ok", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 300, TotalSize: 300},
	})
	defer func() {
		accountCache.Delete("wb-exhausted")
		accountCache.Delete("wb-ok")
	}()

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Model:    "gpt-5.6-sol",
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-exhausted", Provider: providerName},
			{ID: "wb-ok", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-ok" {
		t.Fatalf("paid model wants wb-ok (only funded), got %+v", resp)
	}
}

// Non-workbuddy candidates still defer, even for a free model id.
func TestSchedulerPick_FreeModel_NonWorkbuddyDefers(t *testing.T) {
	resetActiveAuth(t)
	resetFreeRoulette(t)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Model:    "deepseek-v4.1-flash",
		Provider: "antigravity",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "ag", Provider: "antigravity"},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if resp.Handled {
		t.Fatal("non-workbuddy free-model request should defer")
	}
}

// unknown free-model candidate list rebuilds the roulette once and keeps
// rotating deterministically afterward.
func TestFreeRoulette_RebuildOnChange(t *testing.T) {
	r := &wbrr{}
	if got := r.pick(nil); got != "" {
		t.Fatalf("expected empty pick, got %q", got)
	}
	if got := r.pick([]string{"a", "b", "c"}); got != "a" {
		t.Fatalf("first pick wants a, got %q", got)
	}
	if got := r.pick([]string{"a", "b", "c"}); got != "b" {
		t.Fatalf("second pick wants b, got %q", got)
	}
	// List changed → rebuilt from scratch.
	if got := r.pick([]string{"x", "y"}); got != "x" {
		t.Fatalf("rebuilt pick wants x, got %q", got)
	}
	if got := r.pick([]string{"x", "y"}); got != "y" {
		t.Fatalf("next rebuilt pick wants y, got %q", got)
	}
}

func TestCandidateDisabled(t *testing.T) {
	if !candidateDisabled(pluginapi.SchedulerAuthCandidate{Status: "disabled"}) {
		t.Fatal("status disabled")
	}
	if !candidateDisabled(pluginapi.SchedulerAuthCandidate{Metadata: map[string]any{"disabled": true}}) {
		t.Fatal("meta disabled")
	}
	if candidateDisabled(pluginapi.SchedulerAuthCandidate{Status: "active"}) {
		t.Fatal("active should not be disabled")
	}
}

func TestEnsureDefaultActiveAuth(t *testing.T) {
	resetActiveAuth(t)
	id := ensureDefaultActiveAuth([]wbAccount{
		{AuthIndex: "a1", AuthID: "a1", Disabled: true},
		{AuthIndex: "a2", AuthID: "a2", Exhausted: false},
		{AuthIndex: "a3", AuthID: "a3"},
	})
	if id != "a2" {
		t.Fatalf("want first ready a2, got %q", id)
	}
	if getActiveAuthID() != "a2" {
		t.Fatalf("stuck active %q", getActiveAuthID())
	}
	// Already set + still live + not exhausted → keep
	id2 := ensureDefaultActiveAuth([]wbAccount{
		{AuthIndex: "a2", AuthID: "a2"},
		{AuthIndex: "a3", AuthID: "a3"},
	})
	if id2 != "a2" {
		t.Fatalf("should keep a2, got %q", id2)
	}
}

func TestEnsureDefaultActiveAuth_SwitchesWhenExhausted(t *testing.T) {
	resetActiveAuth(t)
	// Selected a1 is exhausted → should switch to first non-exhausted.
	setActiveAuthID("a1")
	id := ensureDefaultActiveAuth([]wbAccount{
		{AuthIndex: "a1", AuthID: "a1", Exhausted: true},
		{AuthIndex: "a2", AuthID: "a2", Exhausted: false},
		{AuthIndex: "a3", AuthID: "a3", Exhausted: false},
	})
	if id != "a2" {
		t.Fatalf("want switch to a2, got %q", id)
	}
	if getActiveAuthID() != "a2" {
		t.Fatalf("active should be a2, got %q", getActiveAuthID())
	}
}

func TestEnsureDefaultActiveAuth_AllExhausted_KeepsCurrent(t *testing.T) {
	resetActiveAuth(t)
	setActiveAuthID("a1")
	id := ensureDefaultActiveAuth([]wbAccount{
		{AuthIndex: "a1", AuthID: "a1", Exhausted: true},
		{AuthIndex: "a2", AuthID: "a2", Exhausted: true},
	})
	if id != "a1" {
		t.Fatalf("all exhausted should keep a1, got %q", id)
	}
}
