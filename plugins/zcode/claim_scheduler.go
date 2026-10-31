// claim_scheduler.go — v0.2.0: the auto-claim scheduler (issue #23), ported
// from TriDefender/zcode-api src/claim/scheduler.ts semantics onto the
// plugin's multi-account surface.
//
// Loop (default every 5 min, config claim_poll_seconds):
//
//	enumerate credentials with a plan JWT → skip ones on hold →
//	read BOTH discovery lists — the banner feed (marketing/touch, touch.go)
//	and billing/preview → claim every discovered plan (claim_plan_id config
//	narrows it to that one) → POST billing/claim each → fold the outcomes.
//
// A claim is fully automatic when the captcha pool (captcha_pool.go) has a
// panel-minted token, or when the campaign does not gate claims. Without a
// token the scheduler still PROBES captcha-less once per hold window — a
// 3007 answer is the definitive "this campaign is captcha-gated" signal and
// lights the panel badge; a success is a free claim no browser was needed
// for. Backoff semantics mirror the upstream scheduler:
//
//	ok / already_claimed → hold until the plan's ends_at (else +24h)
//	quota_exhausted      → hold 1h (daily cap; re-check after it rolls)
//	captcha              → badge + cooldown (pool may refill by then)
//	login_required       → badge + 30 min (needs a re-login, not a retry storm)
//	ineligible/unavailable/not_found/invalid_request/unknown/http_error
//	                     → cooldown
//
// A 404 preview means no campaign is deployed yet — the expected idle state;
// it records nothing and the next poll just runs at normal cadence.
package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultClaimPollSeconds     = 300
	defaultClaimCooldownSeconds = 600
)

var (
	claimAuto          = true
	claimPlanID        = ""
	claimPollSeconds   = defaultClaimPollSeconds
	claimCooldownSecs  = defaultClaimCooldownSeconds
	claimConfigMu      sync.RWMutex
	claimSchedulerOnce sync.Once
)

func loadedClaimAuto() bool {
	claimConfigMu.RLock()
	defer claimConfigMu.RUnlock()
	return claimAuto
}

func loadedClaimPlanID() string {
	claimConfigMu.RLock()
	defer claimConfigMu.RUnlock()
	return claimPlanID
}

func loadedClaimPollInterval() time.Duration {
	claimConfigMu.RLock()
	secs := claimPollSeconds
	claimConfigMu.RUnlock()
	if secs < 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

func loadedClaimCooldown() time.Duration {
	claimConfigMu.RLock()
	secs := claimCooldownSecs
	claimConfigMu.RUnlock()
	if secs < 60 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

// claimAccountState is one credential's scheduler bookkeeping.
type claimAccountState struct {
	PlanID    string
	Kind      string
	Message   string
	HoldUntil time.Time
	Badge     bool
	UpdatedAt time.Time
}

var claimSchedulerState struct {
	sync.Mutex
	lastTick  time.Time
	lastError string
	accounts  map[string]*claimAccountState
}

func claimSchedulerAccount(key string) *claimAccountState {
	claimSchedulerState.Lock()
	defer claimSchedulerState.Unlock()
	if claimSchedulerState.accounts == nil {
		claimSchedulerState.accounts = map[string]*claimAccountState{}
	}
	st := claimSchedulerState.accounts[key]
	if st == nil {
		st = &claimAccountState{}
		claimSchedulerState.accounts[key] = st
	}
	return st
}

// claimTargets resolves the plans to claim this tick (issue #69: the
// discovery set spans the banner feed and billing/preview, so one tick may
// hold several plans — magpie's claimDaily claims every listed plan). An
// explicit claim_plan_id match narrows the set to that plan alone (the
// configured single-plan mode); the default claims every plan, ordered by
// priority descending with discovery order kept on ties (stable) — the
// highest-priority plan is claimed first when a captcha budget matters.
func claimTargets(plans []previewPlan, wantID string) []previewPlan {
	if wantID != "" {
		for _, p := range plans {
			if p.PlanID == wantID {
				return []previewPlan{p}
			}
		}
		return nil
	}
	if len(plans) == 0 {
		return nil
	}
	out := make([]previewPlan, len(plans))
	copy(out, plans)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out
}

// applyClaimOutcome folds one claim attempt into the account state (pure
// apart from the state write — unit-tested directly, ported from the
// upstream scheduler's backoff table).
func applyClaimOutcome(st *claimAccountState, out claimOutcome, cooldown time.Duration, now time.Time) {
	st.Kind = out.Kind
	st.Message = out.Message
	st.PlanID = out.PlanID
	st.UpdatedAt = now
	st.Badge = false
	switch {
	case out.OK || out.Kind == "already_claimed":
		hold := now.Add(24 * time.Hour)
		if out.EndsAt > 0 && time.Unix(out.EndsAt, 0).After(now) {
			hold = time.Unix(out.EndsAt, 0)
		}
		st.HoldUntil = hold
		if out.OK {
			st.Kind = "claimed"
		}
	case out.Kind == "quota_exhausted":
		st.HoldUntil = now.Add(time.Hour) // daily cap — re-check after it rolls
	case out.Kind == "captcha":
		st.Badge = true
		st.HoldUntil = now.Add(cooldown)
	case out.Kind == "login_required":
		st.Badge = true
		st.HoldUntil = now.Add(30 * time.Minute)
	default:
		st.HoldUntil = now.Add(cooldown)
	}
}

// claimSeverity ranks one outcome's hold urgency for the fold: the earliest
// row present across a tick's outcomes decides the account's hold, so a
// captcha beside a success still retried soon (the success then answers
// already_claimed and costs nothing but the round trip).
var claimSeverity = map[string]int{
	"login_required":  0,
	"captcha":         1,
	"quota_exhausted": 2,
	"already_claimed": 4,
}

func claimOutcomeSeverity(out claimOutcome) int {
	if out.OK {
		return 5
	}
	if s, ok := claimSeverity[out.Kind]; ok {
		return s
	}
	return 3 // http_error / unknown / not_found / … — cooldown
}

// foldClaimOutcomes folds one tick's claim outcomes (and the discovery
// set's unread lists) into the account state: the most-urgent outcome
// drives hold/badge via applyClaimOutcome, the message names everything
// (claimed X; Y: claim it in the panel (…); Z: why; unread lists), and an
// unread list caps the hold at one hour — its plans are unknown, so the
// account must come back and look again within the hour (issue #69,
// magpie's "asked again later in the day").
func foldClaimOutcomes(st *claimAccountState, set claimableSet, outs []claimOutcome, names []string, cooldown time.Duration, now time.Time) {
	if len(outs) == 0 {
		return
	}
	worst := 0
	for i := range outs {
		if claimOutcomeSeverity(outs[i]) < claimOutcomeSeverity(outs[worst]) {
			worst = i
		}
	}
	applyClaimOutcome(st, outs[worst], cooldown, now)
	var parts []string
	var claimed []string
	for i := range outs {
		if outs[i].OK {
			claimed = append(claimed, names[i])
		}
	}
	if len(claimed) > 0 {
		parts = append(parts, "claimed "+strings.Join(claimed, ", "))
	}
	for i := range outs {
		if outs[i].Kind == "captcha" {
			parts = append(parts, names[i]+": claim it in the panel ("+outs[i].Message+")")
		}
	}
	for i := range outs {
		if !outs[i].OK && outs[i].Kind != "captcha" && outs[i].Kind != "already_claimed" {
			parts = append(parts, names[i]+": "+outs[i].Message)
		}
	}
	parts = append(parts, set.Unread...)
	st.Message = strings.Join(parts, "; ")
	if len(set.Unread) > 0 {
		if limit := now.Add(time.Hour); st.HoldUntil.After(limit) {
			st.HoldUntil = limit
		}
	}
}

// claimSchedulerTick runs one poll→claim cycle over every JWT credential.
func claimSchedulerTick(now time.Time) {
	if !loadedClaimAuto() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		claimSchedulerState.Lock()
		claimSchedulerState.lastError = "host auth list: " + err.Error()
		claimSchedulerState.Unlock()
		return
	}
	cooldown := loadedClaimCooldown()
	for _, f := range files {
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil || sa == nil || strings.TrimSpace(sa.Auth.JWT) == "" {
			continue
		}
		key := firstNonEmpty(strings.TrimSpace(f.AuthIndex), strings.TrimSpace(f.Name))
		st := claimSchedulerAccount(key)
		claimSchedulerState.Lock()
		onHold := now.Before(st.HoldUntil)
		claimSchedulerState.Unlock()
		if onHold {
			continue
		}
		set := fetchClaimablePlans(sa)
		if len(set.Plans) == 0 {
			if len(set.Unread) > 0 {
				// issue #69: a list that didn't read is unknown, not none —
				// record it and retry next tick (no hold change).
				claimSchedulerState.Lock()
				claimSchedulerState.lastError = "claim lists " + key + ": " + strings.Join(set.Unread, "; ")
				claimSchedulerState.Unlock()
				continue
			}
			claimSchedulerState.Lock()
			st.Kind, st.Message, st.UpdatedAt = "", "", now
			claimSchedulerState.Unlock()
			continue
		}
		targets := claimTargets(set.Plans, loadedClaimPlanID())
		if len(targets) == 0 {
			claimSchedulerState.Lock()
			st.Kind, st.Message, st.UpdatedAt = "", "", now
			claimSchedulerState.Unlock()
			continue
		}
		outs := make([]claimOutcome, 0, len(targets))
		names := make([]string, 0, len(targets))
		for _, target := range targets {
			name := firstNonEmpty(target.Name, target.PlanID)
			param, region := captchaPoolTake()
			out := postClaim(sa, target.PlanID, param, region)
			outs = append(outs, out)
			names = append(names, name)
			if out.OK {
				invalidateAccountCredits(f.AuthIndex, sa.Account.UID)
			}
		}
		foldClaimOutcomes(st, set, outs, names, cooldown, now)
	}
	claimSchedulerState.Lock()
	claimSchedulerState.lastTick = now
	claimSchedulerState.lastError = ""
	claimSchedulerState.Unlock()
}

// startClaimSchedulerOnce launches the background loop. The first tick is
// delayed so plugin-load test bursts never see network traffic; later ticks
// run at the configured cadence and re-read the config each round.
func startClaimSchedulerOnce() {
	claimSchedulerOnce.Do(func() {
		go func() {
			time.Sleep(30 * time.Second)
			for {
				claimSchedulerTick(time.Now())
				time.Sleep(loadedClaimPollInterval())
			}
		}()
	})
}

// handleClaimStatus exposes the scheduler state to the panel.
func handleClaimStatus() map[string]any {
	claimSchedulerState.Lock()
	defer claimSchedulerState.Unlock()
	accounts := make([]map[string]any, 0, len(claimSchedulerState.accounts))
	badges := 0
	for key, st := range claimSchedulerState.accounts {
		entry := map[string]any{"auth_index": key}
		if st.PlanID != "" {
			entry["plan_id"] = st.PlanID
		}
		if st.Kind != "" {
			entry["kind"] = st.Kind
		}
		if st.Message != "" {
			entry["message"] = st.Message
		}
		if !st.HoldUntil.IsZero() && st.HoldUntil.After(time.Now()) {
			entry["hold_until"] = st.HoldUntil.UTC().Format(time.RFC3339)
		}
		entry["badge"] = st.Badge
		if !st.UpdatedAt.IsZero() {
			entry["updated_at"] = st.UpdatedAt.UTC().Format(time.RFC3339)
		}
		accounts = append(accounts, entry)
		if st.Badge {
			badges++
		}
	}
	fresh, capacity, newest := captchaPoolSnapshot()
	out := map[string]any{
		"ok":               true,
		"enabled":          loadedClaimAuto(),
		"plan_id":          loadedClaimPlanID(),
		"poll_seconds":     loadedClaimPollInterval() / time.Second,
		"cooldown_seconds": loadedClaimCooldown() / time.Second,
		"pool":             map[string]any{"size": fresh, "capacity": capacity},
		"badge_accounts":   badges,
		"accounts":         accounts,
	}
	if !claimSchedulerState.lastTick.IsZero() {
		out["last_tick"] = claimSchedulerState.lastTick.UTC().Format(time.RFC3339)
	}
	if claimSchedulerState.lastError != "" {
		out["last_error"] = claimSchedulerState.lastError
	}
	if !newest.IsZero() {
		out["pool"].(map[string]any)["newest_mint"] = newest.UTC().Format(time.RFC3339)
	}
	return out
}
