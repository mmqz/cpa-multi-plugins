// taskcenter.go orchestrates the CN growth-center daily loop and exposes it
// to the management API:
//
//	GET  /tasks       — read-only scan: task list + streak + travel status
//	POST /tasks/run   — run the daily bonus loop for one (auth_index) or
//	                    every CN account
//
// The daily loop consolidates what wb2api runs across five separate
// schedulers into one pass per account (order matters — the report unlocks
// first_buddy, the accept call makes upstream count progress, the claim
// runs last so rewards earned mid-loop are collected):
//
//  1. activity report   → lights streak, unlocks first_buddy (1/day)
//  2. makeup card       → yesterday missed + card available → repair streak
//  3. gift/compensation → one-shot bonuses, business-error = silent skip
//  4. accept pending    → upstream counts progress only for accepted tasks
//  5. buddy travel      → adopt if no buddy / depart if idle / claim if arrived
//  6. streak redeem     → unlocked tiers (7d/14d/28d), 403 = expected skip
//  7. lottery draw      → spend all chances
//  8. claim rewards     → every claimable task (progress ≥ target, unclaimed)
//
// Every step is best-effort: one failing step logs into the summary and the
// loop continues — a broken lottery endpoint must never block check-in-style
// credit collection.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// tasksAutoEnabled reports whether the growth-center bonus loop should ride
// the scheduled check-in ticks (config tasks_auto, default on).
func tasksAutoEnabled() bool {
	tasksAutoMu.RLock()
	defer tasksAutoMu.RUnlock()
	return tasksAuto
}

// -----------------------------------------------------------------------------
// Pure helpers (unit-tested)
// -----------------------------------------------------------------------------

// growthAcceptCandidates picks task codes that should be enrolled: not yet
// accepted, not claimed, not locked. Tasks already accepted/complete are
// skipped; upstream answers idempotently anyway but we keep the request
// list short.
func growthAcceptCandidates(tasks []growthTask) []string {
	var codes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if strings.TrimSpace(t.TaskCode) == "" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	return codes
}

// growthClaimableTasks lists tasks whose progress reached the target and
// whose reward has not been collected yet.
func growthClaimableTasks(tasks []growthTask) []growthTask {
	var out []growthTask
	for _, t := range tasks {
		if t.Claimable {
			out = append(out, t)
		}
	}
	return out
}

// growthTravelAction decides the single next travel action for a status
// snapshot (wb2api travelOne state machine, extracted for testing).
// Returns ("depart"|"claim"|"skip", reason).
func growthTravelAction(st *growthTravel) (action, reason string) {
	if st == nil {
		return "skip", "no status"
	}
	switch st.State {
	case travelStateArrived:
		if st.RecordID == 0 {
			return "skip", "arrived but no record_id"
		}
		return "claim", ""
	case travelStateIdle:
		if st.DailyLimitReached {
			return "skip", "daily limit reached"
		}
		return "depart", ""
	case travelStateTraveling:
		return "skip", "traveling"
	default:
		return "skip", "unknown state " + st.State
	}
}

// -----------------------------------------------------------------------------
// Daily bonus loop
// -----------------------------------------------------------------------------

// tasksBonusResult is the per-account outcome of one daily loop.
type tasksBonusResult struct {
	AuthIndex string   `json:"auth_index"`
	Nickname  string   `json:"nickname,omitempty"`
	Success   bool     `json:"success"`
	Skipped   bool     `json:"skipped,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Lines     []string `json:"lines,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// tasksDailyBonus runs the full daily loop for ONE CN account. Never panics
// upward: upstream failures land in the summary lines, the overall success
// flag only means "loop completed" (not "every step green").
func tasksDailyBonus(sa *storedAuth) *tasksBonusResult {
	res := &tasksBonusResult{}
	add := func(format string, args ...any) {
		res.Lines = append(res.Lines, fmt.Sprintf(format, args...))
	}

	// 1. Activity report: day-idempotent, unlocks first_buddy adoption.
	cid := fmt.Sprintf("wb-%d", time.Now().UnixMilli())
	if err := growthReportActivity(sa, cid, ""); err != nil {
		add("活跃上报失败: %s", err)
	} else {
		add("活跃上报 ok（点亮连登/解锁领养）")
	}

	// 2. Makeup card: only when yesterday is empty and cards exist.
	if missed, err := growthYesterdayMissed(sa); err == nil && missed {
		if st, err := growthStreak(sa); err == nil && st.MakeupCards.Balance > 0 {
			yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
			if err := growthUseMakeupCard(sa, yesterday); err == nil {
				add("补签 %s ok（保连登）", yesterday)
			} else {
				add("补签失败: %s", err)
			}
		}
	}

	// 3. One-shot gifts (business error = already claimed → silent).
	if credit, err := growthClaimGift(sa); err == nil && credit > 0 {
		add("新手礼包 +%d", credit)
	}
	if credit, err := growthClaimCompensation(sa); err == nil && credit > 0 {
		add("活动补偿 +%d", credit)
	}

	// 4. Accept pending tasks so upstream counts behavior progress.
	if tasks, err := growthListTasks(sa); err == nil {
		if codes := growthAcceptCandidates(tasks); len(codes) > 0 {
			if err := growthAcceptTasks(sa, codes); err != nil {
				add("接受任务失败(%d 个): %s", len(codes), err)
			} else {
				add("接受任务 %d 个", len(codes))
			}
		}
		// Pre-collect claimables seen right now — claimed again after the
		// loop below in case behavior during this run completed something.
		for _, t := range growthClaimableTasks(tasks) {
			add("待领取: %s（%d/%d）", t.TaskCode, t.Current, t.Target)
		}
	} else {
		add("任务列表拉取失败: %s", err)
	}

	// 5. Buddy travel state machine (single pass, no waiting/polling).
	tasksTravelOnce(sa, add)

	// 6. Streak tier redemption (locked tiers answer 403 → skip quietly).
	if st, err := growthStreak(sa); err == nil {
		statuses := map[string]string{
			"7d":  st.RedemptionStatus.Tier7dStatus,
			"14d": st.RedemptionStatus.Tier14dStatus,
			"28d": st.RedemptionStatus.Tier28dStatus,
		}
		for _, tier := range st.RedemptionStatus.Tiers {
			if statuses[tier.Tier] == "locked" || statuses[tier.Tier] == "claimed" {
				continue
			}
			if err := growthRedeemTier(sa, tier.Tier); err != nil {
				add("兑换 %s 失败: %s", tier.Tier, err)
				continue
			}
			add("兑换 %s 档（+%d 分 +%d 能 卡×%d 抽×%d）",
				tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
		}
		add("连登 %d 天", st.Streak.Days)
	} else {
		add("连登状态拉取失败: %s", err)
	}

	// 7. Lottery: spend all chances.
	if chances, err := growthLotteryChances(sa); err == nil && chances > 0 {
		drawn := 0
		for i := 0; i < chances; i++ {
			prize, err := growthLotteryDraw(sa)
			if err != nil {
				add("抽奖失败(第%d次): %s", i+1, err)
				break
			}
			drawn++
			add("抽奖#%d: %s", drawn, compactGrowthJSON(prize))
		}
	}

	// 8. Claim rewards for every claimable task (list re-fetched: the loop
	// above may have pushed progress past the target).
	if tasks, err := growthListTasks(sa); err == nil {
		for _, t := range growthClaimableTasks(tasks) {
			credit, energy, err := growthClaimReward(sa, t.TaskCode)
			switch {
			case err != nil:
				add("领取 %s 失败: %s", t.TaskCode, err)
			case credit == 0 && energy == 0:
				add("领取 %s: 已领过", t.TaskCode)
			default:
				add("领取 %s: +%d 分 +%d 能", t.TaskCode, credit, energy)
			}
		}
	}

	res.Success = true
	return res
}

// tasksTravelOnce advances the travel state machine exactly one step.
func tasksTravelOnce(sa *storedAuth, add func(string, ...any)) {
	buddy, err := growthBuddyInfo(sa)
	switch {
	case err != nil:
		add("猫档案查询失败: %s", err)
	case buddy == nil:
		// Adoption needs the day's report first (first_buddy gate). The loop
		// already reported in step 1; agreement is idempotent; a 400 here is
		// the (expected) not-yet-eligible answer — report it and move on.
		if err := growthBuddyAgreement(sa); err != nil {
			add("同意猫协议失败: %s", err)
			return
		}
		if err := growthBuddyFirst(sa); err != nil {
			add("领养未过门槛（明日重试）: %s", err)
			return
		}
		add("领养 ok（+300 分）")
	default:
		st, err := growthTravelStatus(sa)
		if err != nil {
			add("旅行状态查询失败: %s", err)
			return
		}
		action, reason := growthTravelAction(st)
		switch action {
		case "depart":
			if err := growthTravelDepart(sa, growthTravelLocation); err != nil {
				add("派出失败: %s", err)
				return
			}
			add("派出 ok（地点 %d）", growthTravelLocation)
		case "claim":
			reward, err := growthTravelClaim(sa, st.RecordID)
			if err != nil {
				add("旅行领奖失败(record=%d): %s", st.RecordID, err)
				return
			}
			add("旅行领奖 +%d 分", reward)
		default:
			add("旅行跳过: %s", reason)
		}
	}
}

// compactGrowthJSON truncates a raw prize payload for one-line display.
func compactGrowthJSON(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// -----------------------------------------------------------------------------
// Management API
// -----------------------------------------------------------------------------

// tasksSupportsGrowth gates the feature by realm: CN only. Global accounts
// have no growth center upstream; Intl (codebuddy.ai) has never exposed one
// either (wb2api — our only protocol reference — runs CN-only for growth).
func tasksSupportsGrowth(sa *storedAuth) bool {
	return sa != nil && accountRegion(sa) == "cn"
}

// handleTasksQuery serves GET /tasks: read-only scan of every CN account —
// streak, travel status, and the pending (unclaimed) task list. Failures
// are per-account, never fatal for the whole scan.
func handleTasksQuery(req pluginapi.ManagementRequest) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := make([]map[string]any, 0, len(files))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				mu.Lock()
				out = append(out, map[string]any{"auth_index": f.AuthIndex, "error": err.Error()})
				mu.Unlock()
				return
			}
			entry := map[string]any{
				"auth_index": f.AuthIndex,
				"nickname":   sa.Account.Nickname,
			}
			if !tasksSupportsGrowth(sa) {
				entry["skipped"] = true
				entry["reason"] = "non-cn"
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if st, err := growthStreak(sa); err == nil {
				entry["streak_days"] = st.Streak.Days
				entry["makeup_cards"] = st.MakeupCards.Balance
			} else {
				entry["streak_error"] = err.Error()
			}
			if tv, err := growthTravelStatus(sa); err == nil {
				action, reason := growthTravelAction(tv)
				entry["travel_state"] = tv.State
				entry["travel_action"] = action
				if reason != "" {
					entry["travel_reason"] = reason
				}
			} else {
				entry["travel_error"] = err.Error()
			}
			if tasks, err := growthListTasks(sa); err == nil {
				pending := growthClaimableTasks(tasks)
				entry["tasks_total"] = len(tasks)
				pendingView := make([]map[string]any, 0, len(pending))
				for _, t := range pending {
					pendingView = append(pendingView, map[string]any{
						"task_code": t.TaskCode,
						"title":     t.Title,
						"current":   t.Current,
						"target":    t.Target,
						"credit":    t.RewardCredit,
					})
				}
				entry["claimable"] = pendingView
				entry["claimable_count"] = len(pendingView)
			} else {
				entry["tasks_error"] = err.Error()
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Stable order for the panel.
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["auth_index"].(string)
		b, _ := out[j]["auth_index"].(string)
		return a < b
	})
	return map[string]any{"accounts": out}
}

// handleTasksRun serves POST /tasks/run. Body {auth_index} runs one account;
// empty runs every CN account (sem=4, same fan-out shape as manual check-in).
func handleTasksRun(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	results := make([]*tasksBonusResult, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		i, f := i, f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				results[i] = &tasksBonusResult{AuthIndex: f.AuthIndex, Error: err.Error()}
				return
			}
			results[i] = &tasksBonusResult{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname}
			if !tasksSupportsGrowth(sa) {
				results[i].Skipped = true
				results[i].Reason = "non-cn"
				results[i].Lines = []string{"任务中心仅 CN 账号支持"}
				return
			}
			mu := checkinLockFor(f.AuthIndex)
			mu.Lock()
			defer mu.Unlock()
			results[i] = tasksDailyBonus(sa)
			results[i].AuthIndex = f.AuthIndex
			results[i].Nickname = sa.Account.Nickname
		}()
	}
	wg.Wait()

	okN, skipN, errN := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Error != "":
			errN++
		case r.Skipped:
			skipN++
		default:
			okN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":   len(targets),
			"success": okN,
			"skipped": skipN,
			"fail":    errN,
		},
	}
}
