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
//  4. accept pending    → five-state contract, batched, per-task results
//  5. buddy travel      → adopt if no buddy / depart if idle / claim if arrived
//  6. streak redeem     → unlocked tiers (7d/14d/28d), 403 = expected skip
//  7. claim rewards     → every claimable task (completed or progress ≥ target)
//  8. lottery draw      → spend all chances (after claims: rewards grant chances)
//  9. buddy box         → energy sink (energy has no other spend outlet)
//
// 10. energy balance    → display tail
// A credential-level 401/403 aborts the remaining steps (tier-locked 403
// exempt — that one is a normal state, not a dead session).
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

// growthAcceptCandidates picks task codes that should be enrolled. 2026-09
// five-state contract (Coding2API GrowthTaskItem.needs_accept): only ""
// (field absent) and "not_accepted" need enrolling — accepted/in_progress/
// completed/claimed do not. The old three-state guess treated every
// non-empty status as "already accepted", which is exactly how fresh tasks
// end up never enrolling and never counting progress.
func growthAcceptCandidates(tasks []growthTask) []string {
	var codes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if t.AcceptStatus != "" && t.AcceptStatus != "not_accepted" {
			continue
		}
		if strings.TrimSpace(t.TaskCode) == "" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	return codes
}

// growthAcceptBatchSize caps one accept call's body (Coding2API
// ACCEPT_BATCH_SIZE: the array grows with the task list — keep bodies small).
const growthAcceptBatchSize = 20

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
// tasksDailyBonus runs the full daily loop for ONE CN account. Never panics
// upward: upstream failures land in the summary lines, the overall success
// flag only means "loop completed" (not "every step green").
//
// 2026-09 contract alignment (Coding2API f18dc3d + growth_runner):
//   - accept is batched (growthAcceptBatchSize per call) and its per-task
//     results are surfaced — silent per-task rejections ("prerequisite not
//     met") are how five tasks piled up 650 unclaimed credits upstream;
//   - claims run BEFORE the lottery (task rewards grant lottery chances —
//     claim first so they are spendable this run, runner ordering);
//   - a credential-level 401/403 aborts the remaining growth-domain steps
//     (the runner's session_dead discipline: every further call just gets
//     rejected again). The 403 "连续登录天数不足" tier-lock is exempt —
//     it is the normal not-yet-unlocked state, not a dead session;
//   - the buddy box (the energy sink) and an energy-balance tail are new.
func tasksDailyBonus(sa *storedAuth) *tasksBonusResult {
	res := &tasksBonusResult{}
	add := func(format string, args ...any) {
		res.Lines = append(res.Lines, fmt.Sprintf(format, args...))
	}
	// dead aborts every remaining growth-domain step (session rejected us).
	dead := false
	abortDead := func(err error) bool {
		if !isGrowthSessionDead(err) {
			return false
		}
		dead = true
		add("登录态已失效（%s）——中止本轮后续步骤", err)
		return true
	}

	// 1. Activity report: day-idempotent, unlocks first_buddy adoption.
	cid := fmt.Sprintf("wb-%d", time.Now().UnixMilli())
	if err := growthReportActivity(sa, cid, ""); err != nil {
		if !abortDead(err) {
			add("活跃上报失败: %s", err)
		}
	} else {
		add("活跃上报 ok（点亮连登/解锁领养）")
	}

	// 1.5 Buddy adoption BEFORE accept (v0.9.23): fresh accounts have every
	// task gated by first_buddy; report + adopt in the same run lifts the
	// gate so the accept below can enroll on the FIRST run. Previously the
	// adopt ran inside the travel step (after accept) — one full run lost.
	if !dead {
		tasksEnsureBuddy(sa, add)
	}

	// 2. Makeup card: only when yesterday is empty and cards exist.
	if !dead {
		if missed, err := growthYesterdayMissed(sa); err == nil && missed {
			if st, err := growthStreak(sa); err == nil && st.MakeupCards.Balance > 0 {
				yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
				if err := growthUseMakeupCard(sa, yesterday); err != nil {
					if !abortDead(err) {
						add("补签失败: %s", err)
					}
				} else {
					add("补签 %s ok（保连登）", yesterday)
				}
			}
		}
	}

	// 3. One-shot gifts (business error = already claimed → silent).
	if !dead {
		if credit, err := growthClaimGift(sa); err == nil && credit > 0 {
			add("新手礼包 +%d", credit)
		}
		if credit, err := growthClaimCompensation(sa); err == nil && credit > 0 {
			add("活动补偿 +%d", credit)
		}
	}

	// 4. Accept pending tasks (five-state contract: only ""/"not_accepted"
	// enroll) in batches, surfacing per-task results so rejections stay
	// visible instead of masquerading as success.
	if !dead {
		if tasks, err := growthListTasks(sa); err == nil {
			titles := map[string]string{}
			for _, t := range tasks {
				titles[t.TaskCode] = t.Title
			}
			codes := growthAcceptCandidates(tasks)
			accepted, failed := 0, 0
			blocked := map[string][]string{} // 前置任务 code → 受影响任务名（按原因归并）
			for start := 0; start < len(codes) && !dead; start += growthAcceptBatchSize {
				end := start + growthAcceptBatchSize
				if end > len(codes) {
					end = len(codes)
				}
				results, err := growthAcceptTasks(sa, codes[start:end])
				if err != nil {
					if abortDead(err) {
						break
					}
					add("接受任务失败(%d 个): %s", len(codes[start:end]), err)
					break
				}
				for _, r := range results {
					if r.Status == "error" {
						// v0.12.65 三分法（对齐 Coding2API growth_runner 2026-09-20）：
						// ①上游明示无需接单 = 正常应答，静默跳过（下一步照常尝试领奖）；
						// ②前置未满足 = 账号状态常态，按原因归并成一条汇总（十几条逐行报
						// 会掩盖「其实只需做一件事」）；③其余才是真失败，逐条报出。
						if growthAcceptNeedsNoAccept(r.Message) {
							continue
						}
						name := titles[r.TaskCode]
						if name == "" {
							name = r.TaskCode
						}
						if code, isPrereq := growthPrerequisiteOf(r.Message); isPrereq {
							blocked[code] = append(blocked[code], name)
							continue
						}
						failed++
						add("接单失败「%s」: %s", name, r.Message)
						continue
					}
					accepted++
				}
			}
			// 前置受阻汇总：逐原因一条（排序保证输出稳定），不计入失败数。
			prereqCodes := make([]string, 0, len(blocked))
			for code := range blocked {
				prereqCodes = append(prereqCodes, code)
			}
			sort.Strings(prereqCodes)
			for _, code := range prereqCodes {
				add("%d 个任务需先完成前置「%s」（官方客户端操作后自动解除）", len(blocked[code]), growthPrerequisiteLabel(code))
			}
			if accepted > 0 || failed > 0 {
				add("接受任务 %d 个（失败 %d）", accepted, failed)
			}
			// Claimables seen right now — re-checked in step 7 after the
			// loop in case behavior during this run completed something.
			for _, t := range growthClaimableTasks(tasks) {
				add("待领取: %s（%d/%d）", t.TaskCode, t.Current, t.Target)
			}
		} else if !abortDead(err) {
			add("任务列表拉取失败: %s", err)
		}
	}

	// 4.5 Auto-light automatable tasks (v0.9.23): desktop/web/mp fingerprint
	// event chains light up progress upstream (accept only enrolls), then
	// bounded read-back + auto-claim. wb2api autotask parity — before this
	// those tasks never progressed (一键任务完全没能做任务主因).
	if !dead {
		tasksAutoLightOnce(sa, add)
	}

	// 5. Buddy travel state machine (single pass, no waiting/polling).
	if !dead {
		tasksTravelOnce(sa, add)
	}

	// 6. Streak tier redemption (2026-09: tier id form, granted fields,
	// locked-tier 403 = expected skip, unknown-tier 400 = legacy day retry).
	if !dead {
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
				credit, energy, err := growthRedeemTier(sa, tier.Tier)
				if err != nil {
					if abortDead(err) {
						break
					}
					if isGrowthTierLocked(err) {
						add("兑换 %s 未解锁（连登天数不足）", tier.Tier)
					} else {
						add("兑换 %s 失败: %s", tier.Tier, err)
					}
					continue
				}
				if credit > 0 || energy > 0 {
					add("兑换 %s 档（+%d 分 +%d 能）", tier.Tier, credit, energy)
				} else {
					// granted fields absent on this shape — snapshot values
					add("兑换 %s 档（+%d 分 +%d 能 卡×%d 抽×%d）",
						tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
				}
			}
			if !dead {
				add("连登 %d 天", st.Streak.Days)
			}
		} else if !abortDead(err) {
			add("连登状态拉取失败: %s", err)
		}
	}

	// 7. Claim rewards BEFORE the lottery: task rewards often grant lottery
	// chances — claiming first makes them spendable this run.
	if !dead {
		if tasks, err := growthListTasks(sa); err == nil {
		claimLoop:
			for _, t := range growthClaimableTasks(tasks) {
				credit, energy, err := growthClaimReward(sa, t.TaskCode)
				switch {
				case err != nil:
					if abortDead(err) {
						break claimLoop
					}
					add("领取 %s 失败: %s", t.TaskCode, err)
				case credit == 0 && energy == 0:
					add("领取 %s: 已领过", t.TaskCode)
				default:
					add("领取 %s: +%d 分 +%d 能", t.TaskCode, credit, energy)
				}
			}
		} else if !abortDead(err) {
			add("任务列表拉取失败: %s", err)
		}
	}

	// 8. Lottery: spend all chances.
	if !dead {
		if chances, err := growthLotteryChances(sa); err == nil && chances > 0 {
			drawn := 0
			for i := 0; i < chances; i++ {
				prize, err := growthLotteryDraw(sa)
				if err != nil {
					if abortDead(err) {
						break
					}
					add("抽奖失败(第%d次): %s", i+1, err)
					break
				}
				drawn++
				add("抽奖#%d: %s", drawn, compactGrowthJSON(prize))
			}
		} else if err != nil && !abortDead(err) {
			add("抽奖机会查询失败: %s", err)
		}
	}

	// 9. Buddy box: the energy sink — energy has no other spend outlet, so
	// unspent energy just sits there. One open call per run (runner parity:
	// min(affordable, max_open), the rest waits for the next run).
	if !dead {
		if affordable, _, maxOpen, err := growthBuddyQuota(sa); err == nil && affordable > 0 {
			count := affordable
			if count > maxOpen {
				count = maxOpen
			}
			if _, err := growthBuddyOpen(sa, count); err != nil {
				if !abortDead(err) {
					add("能量盲盒失败: %s", err)
				}
			} else {
				add("能量盲盒 ×%d", count)
			}
		}
	}

	// 10. Energy balance tail (display-only; failures never surface).
	if !dead {
		if bal, err := growthEnergyBalance(sa); err == nil && bal > 0 {
			add("能量余额 %d", bal)
		}
	}

	// 11. School-season activity (开学季, time-boxed upstream window). Silent
	// when the activity is not running — post-window runs are unchanged.
	if !dead {
		tasksSchoolOnce(sa, add)
	}

	res.Success = true
	return res
}

// tasksEnsureBuddy ensures the account has adopted its Buddy: the report in
// step 1 already unlocked the first_buddy adoption gate; agreement is
// idempotent; a 400 on first is the (expected) not-yet-eligible answer.
// Returns true when the account has (or just got) a Buddy. Running BEFORE
// accept is the point: adoption completes first_buddy — the shared
// prerequisite of every other task on fresh accounts.
func tasksEnsureBuddy(sa *storedAuth, add func(string, ...any)) bool {
	buddy, err := growthBuddyInfo(sa)
	if err != nil {
		add("猫档案查询失败: %s", err)
		return false
	}
	if buddy != nil {
		return true
	}
	if err := growthBuddyAgreement(sa); err != nil {
		add("同意猫协议失败: %s", err)
		return false
	}
	if err := growthBuddyFirst(sa); err != nil {
		add("领养未过门槛（下轮重试）: %s", err)
		return false
	}
	add("领养 ok（+300 分）")
	return true
}

// tasksTravelOnce advances the travel state machine exactly one step.
func tasksTravelOnce(sa *storedAuth, add func(string, ...any)) {
	buddy, err := growthBuddyInfo(sa)
	switch {
	case err != nil:
		add("猫档案查询失败: %s", err)
	case buddy == nil:
		// Adoption moved to tasksEnsureBuddy (step 1.5, BEFORE accept —
		// fresh-account gate must lift before enrollment). Reaching here
		// means adoption failed earlier this run; report and move on.
		add("旅行跳过: 尚未领养 Buddy")
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
	// Scan budget (v0.12.64): same rationale as handleSchoolVouchers — the
	// read-only 任务 scan must always return a complete JSON envelope instead
	// of pinning the host management bridge past its timeout (the panel then
	// parsed an empty body). Accounts starting past the deadline report as
	// skipped; the explicit 任务 run-all keeps unbounded semantics on purpose.
	tasksScanDeadline := time.Now().Add(45 * time.Second)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !time.Now().Before(tasksScanDeadline) {
				mu.Lock()
				out = append(out, map[string]any{
					"auth_index": f.AuthIndex,
					"skipped":    true,
					"reason":     "scan budget exceeded（扫描超时，稍后重试）",
				})
				mu.Unlock()
				return
			}
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
			// School-season block: only emitted while the activity is live
			// (in_period=true from the API). Keys are absent otherwise so
			// consumers can treat "no school" as "no keys".
			if sTasks, inPeriod, err := schoolTasksList(sa); err == nil && inPeriod {
				entry["school_in_period"] = true
				entry["school_tasks_total"] = len(sTasks)
				sClaimable := 0
				for _, t := range sTasks {
					if schoolTaskClaimable(t) {
						sClaimable++
					}
				}
				entry["school_claimable"] = sClaimable
				if chances, err := schoolChances(sa); err == nil {
					entry["school_chances"] = chances
				}
				if vs, err := schoolVouchers(sa); err == nil {
					vView := make([]map[string]any, 0, len(vs))
					for _, v := range vs {
						vView = append(vView, map[string]any{
							"prize_name": v.PrizeName,
							"sku_code":   v.SKUCode,
							"code":       v.Code,
							"valid_to":   v.ValidTo,
						})
					}
					entry["vouchers"] = vView
					entry["voucher_count"] = len(vView)
				}
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
