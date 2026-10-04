// claim_scheduler.go — v0.2.0: the auto-claim scheduler (issue #23), ported
// from TriDefender/zcode-api src/claim/scheduler.ts semantics onto the
// plugin's multi-account surface.
//
// Loop (default every 5 min, config claim_poll_seconds):
//
//	enumerate credentials with a plan JWT → skip ones on hold →
//	GET billing/preview → pick the target plan (claim_plan_id config or the
//	highest-priority preview) → POST billing/claim.
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

// pickClaimTarget resolves the plan to claim: an explicit claim_plan_id
// match wins, otherwise the highest-priority preview (ties keep the first —
// the preview list order is the server's own ranking).
func pickClaimTarget(plans []previewPlan, wantID string) *previewPlan {
	if wantID != "" {
		for i := range plans {
			if plans[i].PlanID == wantID {
				return &plans[i]
			}
		}
		return nil
	}
	var best *previewPlan
	for i := range plans {
		if best == nil || plans[i].Priority > best.Priority {
			best = &plans[i]
		}
	}
	return best
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
		plans, perr := fetchClaimablePreview(sa)
		if perr != nil {
			claimSchedulerState.Lock()
			claimSchedulerState.lastError = "preview " + key + ": " + perr.Error()
			claimSchedulerState.Unlock()
			continue
		}
		target := pickClaimTarget(plans, loadedClaimPlanID())
		if target == nil {
			claimSchedulerState.Lock()
			st.Kind, st.Message, st.UpdatedAt = "", "", now
			claimSchedulerState.Unlock()
			continue
		}
		param, region := captchaPoolTake()
		out := postClaim(sa, target.PlanID, param, region)
		applyClaimOutcome(st, out, cooldown, now)
		if out.OK {
			invalidateAccountCredits(f.AuthIndex, sa.Account.UID)
		}
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
