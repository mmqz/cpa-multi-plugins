// scheduler.go implements the CPA scheduler.pick capability for zcode.
//
// When scheduler_mode=credits is configured, routing uses the panel-selected
// active account with fallback when that account becomes exhausted/disabled.
// scheduler_mode=off (default) defers everything to the built-in scheduler.
package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
)

var (
	schedulerMode   = schedulerModeOff
	schedulerModeMu sync.RWMutex
)

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a zcode auth candidate based on the
// panel-selected active account. Non-zcode candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if loadedSchedulerMode() != schedulerModeCredits {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect zcode candidates only. A candidate whose (account, request-model)
	// pair is cooling is skipped so the opt-in plugin-side picker agrees with
	// the catalog filter used by host routing.
	reqModel := requestModelForCooldown(req.Model, req.Options.Metadata)
	var zcCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		if reqModel != "" && modelIsCooling(c.ID, reqModel) {
			continue
		}
		zcCandidates = append(zcCandidates, c)
	}
	if len(zcCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	cands := make([]activeAuthCandidate, 0, len(zcCandidates))
	for _, c := range zcCandidates {
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
