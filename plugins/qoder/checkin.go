// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. CN accounts are excluded — they use one-shot trial claims instead.
package main

import (
        "encoding/json"
        "fmt"
        "strings"
        "sync"
        "time"

        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
        schedulerStop chan struct{}
        schedulerMu   sync.Mutex
)

func ensureScheduler() {
        schedulerMu.Lock()
        defer schedulerMu.Unlock()
        if schedulerStop != nil {
                return // already running
        }
        schedulerStop = make(chan struct{})
        go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func nextCheckinTime(now time.Time) time.Time {
        var earliest time.Time
        // Consider both checkin and keepalive schedules so the timer wakes up for
        // whichever fires first (e.g. 21:00 checkin vs 22:00 keepalive → 21:00 wins,
        // then 22:00 keepalive fires on the next tick).
        hours := append([]int{}, checkinHours...)
        hours = append(hours, keepaliveHours...)
        for _, h := range hours {
                t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
                if !t.After(now) {
                        t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
                }
                if earliest.IsZero() || t.Before(earliest) {
                        earliest = t
                }
        }
        return earliest
}

func schedulerLoop(stop chan struct{}) {
        for {
                next := nextCheckinTime(time.Now())
                timer := time.NewTimer(time.Until(next))
                select {
                case <-stop:
                        timer.Stop()
                        return
                case <-timer.C:
                        runAutoCheckin()
                        // Fire keepalive if the current tick falls within its scheduled
                        // window (e.g. 22:00 keepalive fires on the 22:00 tick even though
                        // the previous checkin tick was 21:00).
                        if shouldRunKeepaliveNow(time.Now()) {
                                runTokenKeepalive()
                        }
                }
        }
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// CN: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
        checkinAutoMu.RLock()
        doCheckin := checkinAuto
        checkinAutoMu.RUnlock()
        // Lifecycle may still run when check-in is off (credit gate).
        if !doCheckin && !lifecycleEnabled() {
                return
        }
        files, err := hostAuthList()
        if err != nil {
                return
        }
        var wg sync.WaitGroup
        sem := make(chan struct{}, 4)
        for _, f := range files {
                f := f
                wg.Add(1)
                go func() {
                        defer wg.Done()
                        sem <- struct{}{}
                        defer func() { <-sem }()
                        processAutoCheckinAccount(f, doCheckin)
                }()
        }
        wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
        // A-24: only fetch sa when needed (checkin). For lifecycle-only paths,
        // let reconcileOneAccount do the single hostAuthGetBundle internally.
        if doCheckin {
                sa, err := hostAuthGet(f.AuthIndex)
                if err != nil {
                        return
                }
                // CN: daily check-in when enabled.
                ci, err := fetchCheckinStatus(sa)
                if err == nil && ci != nil && ci.Active && !ci.TodayCheckedIn {
                        if _, callErr := performCheckinCall(sa); callErr == nil {
                                // v0.8.8: 记录签到时刻（与手动签到一致），让紧随
                                // 其后的 reconcile 处于 lifecycle 宽限窗口内。
                                rememberCheckinMoment(f.ID)
                                // Refresh once after a successful checkin call so cache reflects
                                // the post-call state. If the status call fails keep the pre-call
                                // snapshot rather than dropping it (v0.6.31: avoid shadowing ci
                                // with a second fetch that could race with concurrent readers).
                                if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
                                        ci = ci2
                                }
                                // P1-5: checkin grants new credits — refresh the credits cache
                                // immediately so the panel shows the updated balance without
                                // waiting for the async reconcile pass.
                                if cr2, crErr := fetchUserResource(sa); crErr == nil && cr2 != nil {
                                        if v, ok := accountCache.Load(f.ID); ok {
                                                if prev, ok2 := v.(*accountCacheEntry); ok2 {
                                                        fresh := *prev
                                                        fresh.credits = cr2
                                                        fresh.fetched = time.Now()
                                                        accountCache.Store(f.ID, &fresh)
                                                }
                                        }
                                }
                        }
                }
                // Refresh cache with latest checkin status (merge, don't wipe credits/plan).
                if ci != nil {
                        var prev *accountCacheEntry
                        if v, ok := accountCache.Load(f.ID); ok {
                                prev, _ = v.(*accountCacheEntry)
                        }
                        entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
                        if prev != nil {
                                entry.credits = prev.credits
                                entry.plan = prev.plan
                        }
                        accountCache.Store(f.ID, entry)
                }
                if lifecycleEnabled() {
                        _, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
                }
                return
        }
        // Lifecycle-only (checkin off): reconcile handles its own get.
        if lifecycleEnabled() {
                _, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
        }
}

// handleManualCheckin checks in one account (auth_index) or all qoderwork
// accounts. Unlike the workbuddy three-phase classify/execute/summarize flow,
// QoderWork's checkin is a simple Bearer GET+POST — we run it directly per
// account under an 8s timeout, no classify stage.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
        t0 := time.Now()
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

        // Run checkins concurrently (one goroutine per account, bounded).
        type result struct {
                idx int
                out map[string]any
        }
        outCh := make(chan result, len(targets))
        var wg sync.WaitGroup
        sem := make(chan struct{}, 4)
        for i, f := range targets {
                wg.Add(1)
                go func(i int, f pluginapi.HostAuthFileEntry) {
                        defer wg.Done()
                        sem <- struct{}{}
                        defer func() { <-sem }()
                        outCh <- result{idx: i, out: checkinOneAccount(f)}
                }(i, f)
        }
        wg.Wait()
        close(outCh)

        results := make([]map[string]any, len(targets))
        for r := range outCh {
                results[r.idx] = r.out
        }
        successN, alreadyN, failN := 0, 0, 0
        for _, r := range results {
                if r["error"] != nil {
                        failN++
                        continue
                }
                if r["skipped"] == true {
                        alreadyN++
                        continue
                }
                if r["success"] == true {
                        successN++
                } else {
                        failN++
                }
        }
        return map[string]any{
                "results": results,
                "summary": map[string]any{
                        "total":      len(targets),
                        "success":    successN,
                        "already":    alreadyN,
                        "fail":       failN,
                        "elapsed_ms": time.Since(t0).Milliseconds(),
                },
        }
}

// checkinOneAccount performs the actual GET status + POST claim flow for one
// account. 8s overall budget — never blocks past that.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
        out := map[string]any{
                "auth_index": f.AuthIndex,
                "name":       f.Name,
        }
        mu := checkinLockFor(f.AuthIndex)
        mu.Lock()
        defer mu.Unlock()

        sa, err := hostAuthGet(f.AuthIndex)
        if err != nil {
                out["error"] = "get auth: " + err.Error()
                return out
        }
        out["nickname"] = sa.Account.Nickname

        // Step 1: GET status (5s budget).
        ci, err := fetchCheckinStatus(sa)
        if err != nil {
                out["error"] = "status: " + err.Error()
                return out
        }
        if ci.TodayCheckedIn {
                out["success"] = true
                out["skipped"] = true
                out["reason"] = "already"
                out["message"] = "今日已签到"
                out["streak_days"] = ci.StreakDays
                out["total_credits"] = ci.TotalCredits
                return out
        }
        // v0.8.18 (campaign dialect): claimed Intl campaigns drop out of
        // /me/campaigns entirely, so an inactive summary means "nothing left to
        // claim today" — already claimed earlier or no campaign running. Surface
        // it as a no-op, not a failure (reason=none, neutral panel toast).
        // v0.12.80: CN joined the campaign dialect (legacy daily-check-in is
        // DISABLED upstream), so this guard now covers both regions — an
        // inactive CN summary likewise means "nothing claimable today", never
        // an error.
        if capabilitiesForRegion(authRegion(sa)).Contract == checkinContractCampaign && !ci.Active {
                out["success"] = true
                out["skipped"] = true
                out["reason"] = "none"
                out["message"] = "今日暂无可领取权益"
                return out
        }

        // Step 2: POST claim (5s budget).
        res, err := performCheckinCall(sa)
        if err != nil {
                out["error"] = "claim: " + err.Error()
                return out
        }
        // QoderWork returns {"result":"ALREADY_CLAIMED"} with HTTP 409 — surface
        // as already rather than error. (v0.8.8: performCheckinCall 现在会把
        // 409/ALREADY_CLAIMED 归一化到这里；旧代码把它吞成 http 409 字符串，
        // 此分支永远打不中。)
        if result, _ := res["result"].(string); result == "ALREADY_CLAIMED" {
                out["success"] = true
                out["skipped"] = true
                out["reason"] = "already"
                out["message"] = "今日已签到"
                if rc, ok := res["rewardCredits"].(float64); ok {
                        out["reward_credits"] = int64(rc)
                }
                rememberCheckinMoment(f.ID)
                return out
        }
        if success, _ := res["success"].(bool); success {
                out["success"] = true
                if rc, ok := res["rewardCredits"].(float64); ok {
                        out["reward_credits"] = int64(rc)
                }
                out["message"] = "签到成功"
                // Refresh the credits snapshot so the panel shows the post-checkin
                // balance immediately (check-in grants new credits). Best-effort:
                // a failure here must not flip the check-in result to error.
                if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
                        out["credits"] = cr
                }
                // v0.8.8: 记录签到时刻 —— reconcile 的 lifecycle 在宽限窗口内
                // 不做 disable，防止签到后积分池刷新延迟时被误禁用。
                rememberCheckinMoment(f.ID)
                return out
        }
        // v0.8.8: claim 失败时复查一次 status —— 若上游已记账（并发/竞态），
        // 改判为“今日已签”而不是失败（与上游 workbuddy_auto_checkin 的
        // “先确认状态再定论”精神一致）。
        if ci2, err2 := fetchCheckinStatus(sa); err2 == nil && ci2 != nil && ci2.TodayCheckedIn {
                out["success"] = true
                out["skipped"] = true
                out["reason"] = "already"
                out["message"] = "今日已签到（复查确认）"
                rememberCheckinMoment(f.ID)
                return out
        }
        // v0.8.15: upstream answers 200 with success:false plus STRUCTURED
        // detail (error/result objects). Stuffing the raw map into "message"
        // made the panel render "u345ee231：[object Object]" — a string
        // concatenation of a JS object (2026-09-20 field report). The plugin
        // owns the rendering now: one readable line, raw body kept under
        // "upstream" for diagnostics.
        out["success"] = false
        out["message"] = summarizeCheckinClaimRes(res)
        out["upstream"] = res
        // Soft-fail reclass (mirror workbuddy): an "already checked in" family
        // message is done, not a failure — CN duplicate claims (browser tab
        // racing the plugin scheduler) must not flap the panel into an error.
        if msg, _ := out["message"].(string); msg != "" {
                low := strings.ToLower(msg)
                if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
                        out["success"] = true
                        out["skipped"] = true
                        out["reason"] = "already"
                }
        }
        return out
}

// summarizeCheckinClaimRes renders a non-success claim response as one
// human-readable line. QoderWork failure bodies carry structured fields
// (result / message / error as objects) — the panel's string concatenation of
// such a map produced "[object Object]" (2026-09-20 field report), so the
// summary is built server-side. Known scalar fields surface directly;
// unknown shapes fall back to bounded redacted JSON.
func summarizeCheckinClaimRes(res map[string]any) string {
        parts := make([]string, 0, 4)
        if r, ok := res["result"].(string); ok && r != "" {
                parts = append(parts, "result="+r)
        }
        for _, k := range []string{"message", "msg", "error"} {
                switch v := res[k].(type) {
                case string:
                        if strings.TrimSpace(v) != "" {
                                parts = append(parts, k+"="+v)
                        }
                case map[string]any:
                        if b, err := json.Marshal(v); err == nil {
                                parts = append(parts, k+"="+truncateRedacted(string(b), 160))
                        }
                }
        }
        if c, ok := res["code"].(float64); ok {
                parts = append(parts, fmt.Sprintf("code=%d", int64(c)))
        }
        if len(parts) == 0 {
                if b, err := json.Marshal(res); err == nil && string(b) != "{}" {
                        return "上游未确认签到成功：" + truncateRedacted(string(b), 240)
                }
                return "上游未确认签到成功（无明细）"
        }
        return "上游未确认签到成功：" + strings.Join(parts, " · ")
}

// checkinGraceWindow 是签到成功后 lifecycle 禁用动作的宽限期。签到发放的
// 积分在 quota/usage 接口上有刷新延迟，紧随签到之后的 force refresh
// （面板 load(true)）可能读到旧的 remain=0 快照 —— 没有宽限就会把刚
// 签到的 CN 账号写入 disabled:true（“点签到，账号被禁用且刷新不回来”）。
const checkinGraceWindow = 10 * time.Minute

// rememberCheckinMoment stamps the cache entry (creating one when absent) with
// the check-in time so reconcileOneAccount can honor the grace window.
func rememberCheckinMoment(authID string) {
        if authID == "" {
                return
        }
        now := time.Now()
        if v, ok := accountCache.Load(authID); ok {
                if e, ok2 := v.(*accountCacheEntry); ok2 {
                        fresh := *e
                        fresh.checkinAt = now
                        accountCache.Store(authID, &fresh)
                        return
                }
        }
        accountCache.Store(authID, &accountCacheEntry{checkinAt: now, fetched: now})
}

// checkinGraceActive reports whether authID checked in within the grace window.
func checkinGraceActive(authID string) bool {
        if v, ok := accountCache.Load(authID); ok {
                if e, ok2 := v.(*accountCacheEntry); ok2 {
                        return !e.checkinAt.IsZero() && time.Since(e.checkinAt) < checkinGraceWindow
                }
        }
        return false
}

func checkinLockFor(authIndex string) *sync.Mutex {
        v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
        return v.(*sync.Mutex)
}

// v0.8.34 (upstream forensics): the standalone pro-upgrade endpoints this
// plugin called since v0.8.18 — /sash/api/v1/me/pro-upgrade/{eligibility,claim}
// — DO NOT EXIST upstream. Field report ("领取失败 eligibility: http 404",
// CN account ud2d62d72) verified against the OFFICIAL CN desktop client
// v0.4.3 (x-oss-meta-commit e921dfbf, sha256 a796a175…05084f5): the shipped
// app.asar contains ZERO matches for "pro-upgrade"; its entire /sash/api/v1
// surface is campaigns / achievements / ai-conversations / organizations.
// The eligibility→claim flow was endpoint guesswork (v0.8.33's dual-prefix
// probe only guessed two prefixes instead of removing the guess); the 404
// was upstream truthfully answering "no such route".
//
// The campaigns system is the only verifiable benefits channel (the same one
// daily check-in rides): GET /sash/api/v1/me/campaigns lists the account's
// rows — eligibility IS the row's claimStatus — and POST
// .../campaigns/{id}/claim claims one (campaign.go claimCampaignByID).
// handleClaimPro now claims rows that look like the Pro upgrade pack and,
// when none is present, answers with a diagnostic listing instead of a bare
// http error. Upstream gates campaign rows behind the client session (#27):
// an account must open the activity once inside the official client before
// its rows appear — the most likely reason a healthy account sees no Pro row
// at all.

// campaignLooksLikeProUpgrade reports whether a campaign row looks like the
// one-time Pro upgrade pack. Upstream never published the exact key (live
// keys observed in the act-YYYYMMDD-NNN form, which no name substring can
// match), so the match combines two evidence sources: a pro/upgrade-looking
// key or actionType, or a big one-shot CREDITS benefit (the Pro pack's
// verified face value is +1800 — qoder2api-hub). It never fabricates a
// campaign id, and unmatched rows are surfaced in the diagnostics instead of
// being claimed blind.
func campaignLooksLikeProUpgrade(c *campaign) bool {
        for _, s := range []string{c.CampaignKey, c.ActionType} {
                l := strings.ToLower(s)
                if strings.Contains(l, "pro") || strings.Contains(l, "upgrade") {
                        return true
                }
        }
        if c.Benefit != nil && strings.EqualFold(c.Benefit.Kind, "CREDITS") && c.Benefit.Amount >= 1000 {
                return true
        }
        return false
}

// claimProViaCampaigns claims the Pro upgrade pack through the campaigns
// channel. Results:
//   - a pro-looking CLAIMABLE row exists → claim it, panel shape out
//   - pro-looking row already CLAIMED → success=false, 已领取 message
//   - no pro-looking row → success=false + diagnostic listing of the rows
//     the server actually returned (plus the client-session hint)
func claimProViaCampaigns(sa *storedAuth) (map[string]any, error) {
        // v0.8.18: Intl has no Pro-upgrade contract (capability fact, not a
        // transient failure) — skip without firing a request that can only 404.
        if !supportsProUpgrade(sa) {
                return map[string]any{
                        "success": false,
                        "skipped": true,
                        "reason":  "unsupported",
                        "message": "国际版暂无 Pro 升级活动",
                }, nil
        }
        status, err := fetchCampaignStatus(sa)
        if err != nil {
                return nil, err
        }
        var claimable, claimed *campaign
        for i := range status.Campaigns {
                c := &status.Campaigns[i]
                if !campaignLooksLikeProUpgrade(c) {
                        continue
                }
                if strings.EqualFold(c.ClaimStatus, "CLAIMABLE") && claimable == nil {
                        claimable = c
                }
                if strings.EqualFold(c.ClaimStatus, "CLAIMED") && claimed == nil {
                        claimed = c
                }
        }
        if claimable != nil {
                return claimCampaignByID(sa, claimable)
        }
        out := map[string]any{"success": false}
        if claimed != nil {
                out["message"] = "Pro 升级包已领取过（活动行状态 CLAIMED）"
                return out, nil
        }
        // No pro-looking row at all. Diagnose from the actual response instead
        // of guessing an endpoint: list what the server returned and point at
        // the client-session gate.
        out["reason"] = "no_pro_row"
        if len(status.Campaigns) == 0 {
                out["message"] = "此账号活动列表为空——活动资格挂官方客户端会话，请先在官方客户端「福利中心」打开一次活动，再回来领取"
                return out, nil
        }
        keys := make([]string, 0, len(status.Campaigns))
        for i := range status.Campaigns {
                c := &status.Campaigns[i]
                k := fmt.Sprintf("%s(%s/%s", c.CampaignKey, c.ActionType, c.ClaimStatus)
                // v0.8.35: surface the upstream's own "why not" verdicts —
                // achievement-gated rows and device-targeted filtering are the
                // two live reasons a healthy account sees no claimable Pro row.
                if c.UnavailableReason != "" {
                        k += ", reason=" + c.UnavailableReason
                }
                if c.RequiredAchievementKey != "" && !c.AchievementCompleted {
                        k += ", achievement=" + c.RequiredAchievementKey + " 未完成"
                }
                if c.Benefit != nil && c.Benefit.Amount > 0 {
                        k += fmt.Sprintf(", +%d", c.Benefit.Amount)
                }
                keys = append(keys, k+")")
        }
        out["message"] = "此账号活动列表暂无 Pro 升级项（现有：" + strings.Join(keys, "、") + "）。若刚注册，请先在官方客户端打开一次活动页同步资格后再试"
        return out, nil
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
        files, err := hostAuthList()
        if err != nil {
                return
        }
        live := make(map[string]struct{}, len(files))
        for _, f := range files {
                live[f.ID] = struct{}{}
                live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
        }
        checkinLocks.Range(func(key, _ any) bool {
                idx, _ := key.(string)
                if _, ok := live[idx]; !ok {
                        checkinLocks.Delete(key)
                }
                return true
        })
}

// handleClaimPro claims the one-time Pro pack for one account through the
// campaigns channel (v0.8.34 — the standalone pro-upgrade endpoints never
// existed upstream; see the forensics note above claimProViaCampaigns).
// Surfaced as a panel per-card button — NOT called automatically during
// login (login writes the auth file first; this is a user-triggered
// post-login action).
func handleClaimPro(req pluginapi.ManagementRequest) map[string]any {
        var body struct {
                AuthIndex string `json:"auth_index"`
        }
        _ = json.Unmarshal(req.Body, &body)
        authIndex := strings.TrimSpace(body.AuthIndex)
        if authIndex == "" {
                return map[string]any{"error": "auth_index is required"}
        }
        sa, err := hostAuthGet(authIndex)
        if err != nil {
                return map[string]any{"error": "get auth: " + err.Error()}
        }
        out := map[string]any{
                "auth_index": authIndex,
                "nickname":   sa.Account.Nickname,
        }
        // Intl skip / diagnostics / claim all live in claimProViaCampaigns
        // so the flow semantics have exactly one home.
        res, err := claimProViaCampaigns(sa)
        if err != nil {
                out["error"] = "claim-pro: " + err.Error()
                return out
        }
        for k, v := range res {
                out[k] = v
        }
        if _, ok := out["success"]; !ok {
                out["success"] = true
        }
        // Refresh credits snapshot so panel shows updated balance immediately.
        if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
                out["credits"] = cr
        }
        return out
}
