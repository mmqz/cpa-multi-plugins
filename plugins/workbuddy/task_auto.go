// task_auto.go growth 任务自动点亮编排（对齐 wb2api panel/autotask.go 2026-09
// 实测口径）——把"接单"推进到"计分"再收敛到"领奖"。
//
// 背景：tasks/accept 只是报名，进度由服务端行为事件点亮；不同任务认不同客户端
// 指纹（桌面/web/mp，task_events.go）。此前插件只做基础活跃上报（1 条/天），
// 需要桌面/web/mp 指纹的任务永远 0 进度——即"一键任务完全没能做任务"的主因。
//
// 本编排挂接在每日循环 accept 之后（见 taskcenter.go）：对每个可自动化且未
// 完成的任务发射对应指纹的判据事件链，回读进度（上游异步计分，有界轮询），
// 达标即自动领奖。所有动作幂等：已 claimed/已达标任务自动跳过。
//
// 真实对话类任务（Model_chat_GLM5.2 / skill_1 / expert_5 / Expert_team_use_3 /
// Expert_lighthouse / black_cat）由 task_chat.go 承接：桌面指纹 SSE 真实对话取
// 服务端 requestId + 模型对齐上报。black_cat 另有每日 23 点补跑位
// （growthNightTaskTick，窗口外行为不计分）。
package main

import (
	"fmt"
	"net/http"
	"time"
)

// growthAutoGap 相邻动作之间的节流间隔（wb2api 实测 1.05s 口径，防风控）。
// growthClaimPollGap 异步计分回读轮询间隔。var 以便测试注入归零。
var (
	growthAutoGap      = 1050 * time.Millisecond
	growthClaimPollGap = 3 * time.Second
)

// growthAutoAction 一个可自动化的任务动作。
type growthAutoAction struct {
	Code    string // 目标任务 code
	Desc    string // 展示用说明
	Attempt bool   // true = 尝试型（上游未完全证实，跑了可能不点亮）
	run     func(sa *storedAuth) (string, error)
	mp      bool // 小程序口径专属任务（列表/accept/claim 走 mp 变体）
}

// growthAutoActions 已实现的任务动作表（顺序即执行顺序；wb2api 实测依赖序）。
var growthAutoActions = []growthAutoAction{
	{Code: "chat_5", Desc: "补报对话活跃事件", run: runAutoChat5},
	{Code: "first_buddy", Desc: "上报解锁+领养", run: runAutoFirstBuddy},
	{Code: "RichMeow_Chat", Desc: "桌面对话事件链", run: runAutoRichMeow},
	{Code: "Buddy_App", Desc: "桌面应用进入事件链", run: runAutoBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "桌面应用进入事件链", run: runAutoBuddyApp},
	{Code: "automation_1", Desc: "定时任务创建事件", run: runAutoAutomationCreate},
	{Code: "Library_read", Desc: "web 资料库阅读事件", run: runAutoLibraryRead},
	{Code: "template_5", Desc: "模板使用事件组 ×5", run: runAutoTemplateUse},
	{Code: "playbook_prompt", Desc: "灵感案例事件组", run: runAutoPlaybookPrompt},
	{Code: "create_canvas", Desc: "设计画布事件组", run: runAutoCreateCanvas},
	{Code: "Hp_Appearance", Desc: "主题设置+皮肤事件", run: runAutoAppearance},
	{Code: "school_season", Desc: "校园日（mp 口径）", run: runAutoSchoolSeason, mp: true},
	{Code: "Sequential_Tasks_1", Desc: "小程序首对话（mp 口径）", run: runAutoSequentialChat, mp: true},
	// 真实对话类（task_chat.go）：消耗极短对话额度，幂等差额收口。
	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话+对齐上报", run: runAutoModelChat},
	{Code: "skill_1", Desc: "真实对话+skill_info JOIN", run: runAutoSkillFresh},
	{Code: "expert_5", Desc: "专家召唤+真实使用链", run: runAutoExpertUse},
	{Code: "Expert_team_use_3", Desc: "专家团召唤+真实使用链", run: runAutoExpertTeamUse},
	{Code: "Expert_lighthouse", Desc: "轻量云专家 LOCAL 链", run: runAutoExpertLighthouse},
	{Code: "black_cat", Desc: "夜猫子（23-08 窗口）", run: runAutoBlackCat},
}

// growthMPTaskCodes 小程序口径专属下发的成长任务：默认列表不出现，
// accept/claim 均要求 mp 头（task_events.go growthCallMP）。
var growthMPTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
}

// growthAutoCtx 单账号自动点亮上下文：默认口径任务列表拉一次复用，
// mp 专属码按需走 mp 列表（默认列表查不到它们）。
type growthAutoCtx struct {
	sa    *storedAuth
	tasks map[string]*growthTask
}

// growthAutoCtxFor 拉取默认口径任务列表构建上下文；列表失败返回错误
// （整个点亮环节跳过，不影响循环其他步骤）。
func growthAutoCtxFor(sa *storedAuth) (*growthAutoCtx, error) {
	tasks, err := growthListTasks(sa)
	if err != nil {
		return nil, err
	}
	c := &growthAutoCtx{sa: sa, tasks: map[string]*growthTask{}}
	for i := range tasks {
		c.tasks[tasks[i].TaskCode] = &tasks[i]
	}
	return c, nil
}

// task 定位单个任务：默认列表命中即返回；mp 专属码回落 mp 列表。
// 未找到返回 (nil, nil)——账号没有该任务属正常状态。
func (c *growthAutoCtx) task(code string) (*growthTask, error) {
	if t, ok := c.tasks[code]; ok {
		return t, nil
	}
	if !growthMPTaskCodes[code] {
		return nil, nil
	}
	raw, err := growthCallMP(c.sa, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	tasks := parseGrowthTasks(raw)
	for i := range tasks {
		if tasks[i].TaskCode == code {
			c.tasks[code] = &tasks[i]
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// taskFresh 绕过缓存重查任务（异步计分回读必须真 GET；与 task 的差异仅在缓存）。
func (c *growthAutoCtx) taskFresh(code string) (*growthTask, error) {
	raw, err := growthCall(c.sa, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	tasks := parseGrowthTasks(raw)
	for i := range tasks {
		if tasks[i].TaskCode == code {
			c.tasks[code] = &tasks[i]
			return &tasks[i], nil
		}
	}
	if !growthMPTaskCodes[code] {
		return nil, nil
	}
	return growthCallMPTask(c.sa, code)
}

// taskWaiting 有界轮询回读（上游异步计分：事件上报后进度要数秒才刷新，
// 一次性回读会误判未达标从而跳过领奖）。预算 2 轮 × 3s（紧凑版——run-all
// 单账号还有后续步骤，wb2api 的 4 轮预算在编排层折半）。
func (c *growthAutoCtx) taskWaiting(code string) (*growthTask, error) {
	t, err := c.task(code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 0; i < 2; i++ {
		time.Sleep(growthClaimPollGap)
		t2, err2 := c.taskFresh(code)
		if err2 != nil {
			return t, nil
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// claim 按口径领奖：mp 任务走 mp 变体（chat 域 mp 头，400 回落 web 域），
// 其余走 Web 域领奖端点（growthClaimReward）。
func (c *growthAutoCtx) claim(code string) (int64, int64, error) {
	if growthMPTaskCodes[code] {
		return growthClaimRewardMP(c.sa, code)
	}
	return growthClaimReward(c.sa, code)
}

// tasksAutoLightOnce 自动点亮单账号全部可自动化任务（每日循环第 4.5 步入口）。
// 单项失败只记一行，绝不阻塞后续任务与循环。
func tasksAutoLightOnce(sa *storedAuth, add func(string, ...any)) {
	c, err := growthAutoCtxFor(sa)
	if err != nil {
		if !isGrowthSessionDead(err) {
			add("任务列表拉取失败（跳过自动点亮）: %s", err)
		}
		return
	}
	for _, act := range growthAutoActions {
		before, err := c.task(act.Code)
		if err != nil {
			add("点亮「%s」查询失败: %s", act.Code, err)
			continue
		}
		if before == nil {
			continue // 账号没有该任务（上游未下发/已过期），静默
		}
		if before.Claimed || (before.Target > 0 && before.Current >= before.Target) {
			continue // 已完成，幂等跳过
		}
		msg, err := act.run(sa)
		if err != nil {
			add("点亮「%s」失败: %s", act.Code, err)
			continue
		}
		line := fmt.Sprintf("点亮「%s」: %s", act.Code, msg)
		// 回读 + 达标自动领奖（上报 200 ≠ 计分，以回读为准）。
		after, aerr := c.taskWaiting(act.Code)
		if aerr == nil && after != nil && after.Claimable && !after.Claimed {
			credit, energy, cerr := c.claim(act.Code)
			switch {
			case cerr != nil:
				line += fmt.Sprintf("；达标但领奖失败: %s", cerr)
			case credit > 0 || energy > 0:
				line += fmt.Sprintf("；已领 +%d 分 +%d 能", credit, energy)
			default:
				line += "；奖励此前已领取"
			}
		}
		add("%s", line)
		time.Sleep(growthAutoGap)
	}
}

// -----------------------------------------------------------------------------
// 各任务动作实现（载荷构造在 task_events.go）
// -----------------------------------------------------------------------------

// runAutoChat5 补足 chat_5 进度：按差额上报 chat_request_send（无需真实会话）。
func runAutoChat5(sa *storedAuth) (string, error) {
	tasks, err := growthListTasks(sa)
	if err != nil {
		return "", err
	}
	var cur, target int64 = 0, 5
	for _, t := range tasks {
		if t.TaskCode == "chat_5" {
			cur, target = t.Current, t.Target
			break
		}
	}
	if target <= 0 {
		target = 5
	}
	need := target - cur
	if need <= 0 {
		return "进度已达标", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := growthReportActivity(sa, cid, ""); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条后中断", i+1, need), err
		}
		if i < need-1 {
			time.Sleep(growthAutoGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runAutoFirstBuddy 领养：report（前置解锁）→ agreement → first。
// 每日循环第 1.5 步已领养过时本动作通常被幂等跳过——保留作独立兜底。
func runAutoFirstBuddy(sa *storedAuth) (string, error) {
	if err := growthReportActivity(sa, fmt.Sprintf("wb-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(growthAutoGap)
	if err := growthBuddyAgreement(sa); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := growthBuddyFirst(sa); err != nil {
		if growthBuddyGateErr(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分）", nil
}

// growthBuddyGateErr 领养门槛未达标的业务错误（HTTP 400 + first_buddy 任务
// 未完成——上游要求当日真实活跃上报先行）。
func growthBuddyGateErr(err error) bool {
	he, ok := err.(*growthHTTPError)
	return ok && he.status == 400 && containsLower(he.msg, "first_buddy task not completed")
}

// containsLower 大小写不敏感的子串判定（errors 侧小工具，避免 strings 重复转换）。
func containsLower(s, sub string) bool {
	n := len(sub)
	if n == 0 {
		return true
	}
	for i := 0; i+n <= len(s); i++ {
		m := true
		for j := 0; j < n; j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				m = false
				break
			}
		}
		if m {
			return true
		}
	}
	return false
}

// runAutoRichMeow 桌面端对话 1 次：完整桌面对话事件链（三账号实测点亮）。
func runAutoRichMeow(sa *storedAuth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb-rm-%d", ms)
	req := fmt.Sprintf("wb-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	return "", growthReportDesktopEvent(sa,
		growthDesktopChatSequence(conv, req, msg, "fast-model", "fast-model")...)
}

// runAutoBuddyApp 进入 Buddy 应用五连事件——同一组事件同时覆盖 Buddy_App
// 与 Buddy_App_QQ（判据应用固定企鹅教师助手，服务端不校验真实授权）。
func runAutoBuddyApp(sa *storedAuth) (string, error) {
	return "", growthReportDesktopEvent(sa,
		growthDesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")...)
}

// runAutoAutomationCreate 定时任务创建成功事件（无需真实创建）。
func runAutoAutomationCreate(sa *storedAuth) (string, error) {
	return "", growthReportDesktopEvent(sa, growthDesktopAutomationCreateEvent("wb 自动化"))
}

// runAutoLibraryRead 体验资料库：web 域 web_element_click(library_doc_intro_click)。
func runAutoLibraryRead(sa *storedAuth) (string, error) {
	return "", growthReportWebEvent(sa, "web_element_click",
		"https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm",
		"library_doc_intro_click", "WorkBuddy资料库介绍")
}

// runAutoTemplateUse 使用 5 个模板：5 组事件链（不同模板 id）一次上报 5/5 点亮。
func runAutoTemplateUse(sa *storedAuth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb-tpl-req-%d-%d", ms, i)
		if err := growthReportDesktopEvent(sa,
			growthDesktopTemplateUseSequence(conv, req, tp[0], tp[1])...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败", i+1), err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runAutoPlaybookPrompt 灵感案例做同款：playbook_prompt_send 事件组。
func runAutoPlaybookPrompt(sa *storedAuth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb-pb-%d", ms)
	req := fmt.Sprintf("wb-pb-req-%d", ms)
	return "", growthReportDesktopEvent(sa,
		growthDesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
}

// runAutoCreateCanvas 设计创意画布：wbx_design_canvas_* 事件组（+300 分）。
func runAutoCreateCanvas(sa *storedAuth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb-canvas-%d", ms)
	req := fmt.Sprintf("wb-canvas-req-%d", ms)
	return "", growthReportDesktopEvent(sa, growthDesktopDesignCanvasSequence(conv, req)...)
}

// runAutoAppearance 换主题：appearance/set API 留痕 + appearance_skin_apply
// 事件（只 set 不发事件不计分——wb2api 实测修正）。
func runAutoAppearance(sa *storedAuth) (string, error) {
	const themeKey = "theme-tkmw7j" // Hp_Appearance 判据主题（和平精英激战金秋）
	if err := growthSetAppearanceTheme(sa, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	return "", growthReportDesktopEvent(sa, growthDesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	})
}

// runAutoSchoolSeason 校园日（mp 口径闭环，+100c+5e）。
func runAutoSchoolSeason(sa *storedAuth) (string, error) {
	return runAutoMPMiniChat(sa, "school_season", true)
}

// runAutoSequentialChat 小程序首对话（mp 口径闭环，+100c+5e）。
func runAutoSequentialChat(sa *storedAuth) (string, error) {
	return runAutoMPMiniChat(sa, "Sequential_Tasks_1", false)
}

// runAutoMPMiniChat mp 限定任务通用闭环：mp 查询 → accept（带登记回读验证：
// 上游存在 200+OK 但未落账形态，未生效自动重试一次）→ mini chat 事件按差额
// 补报 → 回读 → 达标领奖。
func runAutoMPMiniChat(sa *storedAuth, code string, withActivityId bool) (string, error) {
	t, err := growthCallMPTask(sa, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		ok := false
		for attempt := 0; attempt < 2 && !ok; attempt++ {
			if _, err := growthCallMP(sa, http.MethodPost, growthTasksAcceptPath,
				map[string]any{"task_codes": []string{code}}); err != nil {
				continue
			}
			time.Sleep(growthAutoGap)
			t2, err := growthCallMPTask(sa, code)
			if err == nil && t2 != nil && t2.AcceptStatus != "" && t2.AcceptStatus != "not_accepted" {
				ok = true
			}
		}
		if !ok {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := growthClaimRewardMP(sa, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%d 分 +%d 能）", credit, energy), nil
	}
	need := target - t.Current
	for i := int64(0); i < need; i++ {
		conv := fmt.Sprintf("wb-mp-%d-%d", time.Now().UnixMilli(), i)
		if err := growthReportMPEvent(sa, growthMPChatEvent(conv, withActivityId)); err != nil {
			return fmt.Sprintf("完成 %d/%d 次上报后中断", i, need), err
		}
		time.Sleep(growthAutoGap)
	}
	// 回读（异步计分，两轮 × 3s）。
	for i := 0; i < 2; i++ {
		time.Sleep(growthClaimPollGap)
		t2, err := growthCallMPTask(sa, code)
		if err != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账", nil
	}
	if t.Current < target {
		return fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target), nil
	}
	credit, energy, err := growthClaimRewardMP(sa, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%d 分 +%d 能）", credit, energy), nil
}

// growthCallMPTask mp 口径任务列表定位单个任务；未找到返回 (nil, nil)。
func growthCallMPTask(sa *storedAuth, code string) (*growthTask, error) {
	raw, err := growthCallMP(sa, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	tasks := parseGrowthTasks(raw)
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}
