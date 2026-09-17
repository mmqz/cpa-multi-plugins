// scheduler.go implements the CPA scheduler.pick capability for workbuddy.
//
// Free vs paid routing: models in freeModelCache bypass credits checks and are
// round-robined across all enabled workbuddy auths, because a free promotional
// model costs no credits so an exhausted account still serves it. All other
// models fall through to the panel-selected active account with the usual
// exhausted/disabled fallback (picker.go semantics).
package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Legacy config values kept for configure() compatibility; pick always uses
// panel active-auth selection now (not credit-max ranking).
const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
)

// freeModelCache lists model IDs that are free while in trial and must NOT pay
// account credits. A free model may be served from an account whose credits are
// exhausted, because calling it does not spend the paid pool. Paid models keep
// the credits-based picker so an exhausted account is skipped.
//
// Add here only models whose upstream idle cost is zero when the account is in
// its free-trial window (e.g. deepseek-v4.1-flash free-for-2-weeks since
// 2026-09-10). Once the trial ends the model stops being free; remove it here
// at the same time you remove the promo from the model catalog.
var freeModels = map[string]bool{
	// DeepSeek V4.1 Flash: free for 2 weeks in WorkBuddy/CodeBuddy since
	// 2026-09-10 launch partner agreement (models.go v0.12.19 note).
	"deepseek-v4.1-flash": true,
	// DeepSeek V4 Flash (non 4.1): still free in the 14-day promo window.
	"deepseek-v4-flash": true,
}

// isFreeModel reports whether a model id is exempt from credits checks.
func isFreeModel(model string) bool {
	return freeModels[model]
}

// freeModelIDs returns the ids in freeModels (map iteration order, which Go
// randomizes) so callers that need a stable advertised list can sort it.
func freeModelIDs() []string {
	ids := make([]string, 0, len(freeModels))
	for id := range freeModels {
		ids = append(ids, id)
	}
	return ids
}

var (
	schedulerMode   = schedulerModeOff
	schedulerModeMu sync.RWMutex
)

// freePicker keeps a per-auth round-robin cursor so free-model traffic spreads
// across all enabled accounts instead of sticking to the panel-selected auth.
// Same-package so the host may read/replace it in tests.
var (
	freeRouletteMu sync.Mutex
	freeRoulette   = &wbrr{mu: sync.Mutex{}}
)

// wbrr is a small round-robin over a stable ordered id list. It is not safe
// for concurrent use by itself; serialize with freeRouletteMu when reading or
// mutating it. The list is only ever rebuilt while holding the mutex.
type wbrr struct {
	mu   sync.Mutex
	ids  []string
	next int
}

// pick rotates to the next id in the current list, rebuilding the list when
// the given ids differ from the cached one. Returns "" when there is nothing
// to pick from. Callers copy the result before releasing the lock.
func (r *wbrr) pick(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	if len(r.ids) != len(ids) {
		changed = true
	} else {
		for i, id := range ids {
			if r.ids[i] != id {
				changed = true
				break
			}
		}
	}
	if changed {
		r.ids = append([]string(nil), ids...)
		r.next = 0
	}
	// Skip over a removed id that was the previous cursor.
	if r.next >= len(r.ids) {
		r.next = 0
	}
	id := r.ids[r.next]
	r.next = (r.next + 1) % len(r.ids)
	return id
}

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a workbuddy auth candidate based on the
// panel-selected active account. Non-workbuddy candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"     → plugin does NOT handle routing; defer everything to built-in.
//   - "credits" → plugin picks via panel-selected active account (sticky, with
//     fallback when that account becomes exhausted/disabled).
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode: credits in plugin config.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// v0.6.31: actually honor the scheduler_mode toggle. Previously the config
	// was parsed but never read, so "off" silently behaved like "credits".
	if loadedSchedulerMode() != schedulerModeCredits {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect workbuddy candidates only.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	log.Printf("scheduler: pick req model=%q provider=%q providers=%v totalCandidates=%d wb=%d all=%v",
		req.Model, req.Provider, req.Providers, len(req.Candidates), len(wbCandidates), candidateIDsOf(req.Candidates))
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Free models (trial promos) bypass credits entirely and are spread across
	// every enabled account. Paid models go through the credits-aware picker
	// so an exhausted account is skipped.
	if isFreeModel(req.Model) {
		ids := make([]string, 0, len(wbCandidates))
		seen := make(map[string]bool, len(wbCandidates))
		for _, c := range wbCandidates {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			ids = append(ids, c.ID)
		}
		freeRouletteMu.Lock()
		picked := freeRoulette.pick(ids)
		freeRouletteMu.Unlock()
		log.Printf("scheduler: free model %q picked %q from %v", req.Model, picked, ids)
		if picked == "" {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  picked,
			Handled: true,
		})
	}

	// Build thin view for active-auth picker.
	cands := make([]activeAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		_, exhausted := cachedCreditsScore(c.ID)
		cands = append(cands, activeAuthCandidate{
			ID:        c.ID,
			Disabled:  false, // already filtered
			Exhausted: exhausted,
		})
	}
	picked := pickActiveAuth(cands)
	if picked == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// candidateIDsOf returns the request's candidate IDs for diagnostics.
func candidateIDsOf(candidates []pluginapi.SchedulerAuthCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	return ids
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// cachedCreditsScore returns (remain, exhausted) from accountCache.
// remain is -1 when unknown; exhausted uses isCreditsExhausted.
// Key is auth.ID (same as SchedulerAuthCandidate.ID and activeAuthID).
func cachedCreditsScore(authID string) (int64, bool) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, false
	}
	return entry.credits.TotalRemain, isCreditsExhausted(entry.credits)
}
