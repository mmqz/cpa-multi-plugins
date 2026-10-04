// task_chat.go 真实对话类成长任务——task_auto.go 指纹事件层覆盖不到的剩余
// 6 项（Model_chat_GLM5.2 / skill_1 / expert_5 / Expert_team_use_3 /
// Expert_lighthouse / black_cat）。判据共同点：需要与上游真实发生对话。
//
// 两类机制（wb2api 2026-09 实测口径）：
//  1. glm-5.2 类（Model_chat_GLM5.2、black_cat）：真实对话发生（服务端留痕）
//     + 上报 chat_request_send 且模型字段与对话一致（对话 glm-5.2、上报却带
//     默认 flash 会被判不匹配）。report 的 conversationId 为调用方生成即可。
//  2. JOIN 类（skill_1、expert_5、Expert_team_use_3、Expert_lighthouse）：
//     桌面指纹真实对话取 SSE 流里的服务端 requestId（自造 UUID 不计数，
//     Sunny row 2113 实证）→ chat 链 + expert_actual_use / skill_info 事件
//     JOIN 该 id。专家 id 必须来自真实市场列表。
//
// 对话内容刻意极短（"hi，请回复一句话" / "1+1等于几？直接回答。"），单条消耗
// 可忽略。全部动作幂等：已达标/已领取由编排层跳过，本层再按差额收口。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"
)

// growthMarketExpert 专家市场条目（/portal/operation-platform/market/expert/list）。
type growthMarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// growthServerIDRe 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var growthServerIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// growthChatSystemPrompt 桌面指纹对话的 system 段（wb2api DesktopChatWithExpert 同款）。
const growthChatSystemPrompt = "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"

// growthExpertSummonGap 专家召唤→对话→使用的节奏间隔（wb2api panel 同款 6s，
// 其 v11 实测 8s 成功率 100%）。growthNightChatGap 夜间对话间隔（wb2api 同款
// 4s）。var 以便测试注入归零。
var (
	growthExpertSummonGap = 6 * time.Second
	growthNightChatGap    = 4 * time.Second
)

// growthNightWindowFn 夜间窗口判定（默认 growthInNightWindow(time.Now())，
// var 以便测试注入固定结果）。

// growthChatBaseOverride 任务对话端点测试注入点（与 setGrowthBase 同型）。
var growthChatBaseOverride string

// setGrowthChatBase 重定向任务对话端点，返回恢复函数（测试用）。
func setGrowthChatBase(s string) func() {
	old := growthChatBaseOverride
	growthChatBaseOverride = s
	return func() { growthChatBaseOverride = old }
}

func growthChatBaseFor(sa *storedAuth) string {
	if growthChatBaseOverride != "" {
		return growthChatBaseOverride
	}
	return upstreamBaseFor(sa)
}

// growthMarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 /
// "team" 专家团）。expert_actual_use 判据校验专家 id 必须真实存在——编造 id
// 不计数（三账号实测）。桌面指纹头族（与 growthReportDesktopEvent 同族）。
func growthMarketExpertList(sa *storedAuth, expertType string) ([]growthMarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	base := growthBase()
	req, err := http.NewRequest(http.MethodPost, base+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", growthDesktopUA)
	req.Header.Set("X-Domain", base)
	req.Header.Set("X-Product", "SaaS")
	data, err := growthDo(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []growthMarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// growthChatStream 发起一次真实对话（stream:true）并返回打开的 SSE 流。
// withSystem=true 时携带桌面同款 system 段；desktop=true 时走桌面客户端指纹
// 头族（X-IDE-*，JOIN 类判据要求），可带 X-Expert-Id；否则走插件常规 chat
// 头族（backendHeaders，glm-5.2 类实测可用）。conversationID 为调用方生成并
// 随 X-Conversation-ID 下发。HTTP >= 400 返回 *growthHTTPError。
func growthChatStream(sa *storedAuth, model, expertID, userPrompt string, withSystem, desktop bool) (*hostStreamReader, string, error) {
	if sa == nil || sa.Auth.AccessToken == "" {
		return nil, "", fmt.Errorf("no access token")
	}
	conversationID := fmt.Sprintf("wb-task-%d", time.Now().UnixNano())
	messages := []map[string]any{{"role": "user", "content": userPrompt}}
	if withSystem {
		messages = append([]map[string]any{{"role": "system", "content": growthChatSystemPrompt}}, messages...)
	}
	body := map[string]any{
		"model":          model,
		"messages":       messages,
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	base := growthChatBaseFor(sa)
	req, err := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	if desktop {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
		req.Header.Set("User-Agent", growthDesktopUA)
		req.Header.Set("X-Domain", base)
		req.Header.Set("X-Product", "SaaS")
		if sa.Account.UID != "" {
			req.Header.Set("X-User-Id", sa.Account.UID)
		}
		req.Header.Set("X-Agent-Intent", "craft")
		req.Header.Set("X-Agent-Type", "main")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Version", "5.5.6")
		req.Header.Set("x-codebuddy-request", "1")
		if expertID != "" {
			req.Header.Set("X-Expert-Id", expertID)
		}
	} else {
		backendHeaders(req, sa)
	}
	stream, status, _, err := hostHTTPDoStream(req)
	if err != nil {
		return nil, conversationID, err
	}
	reader := newHostStreamReader(stream)
	if status >= 400 {
		payload, _ := io.ReadAll(io.LimitReader(reader, 4096))
		_ = reader.Close()
		return nil, conversationID, &growthHTTPError{status: status, msg: truncateRedacted(string(payload), 160)}
	}
	return reader, conversationID, nil
}

// growthChatPlain 一次极短真实对话（插件常规头族），读完整个流避免残留连接。
// glm-5.2 类判据只要求对话事实 + 上报模型对齐——服务端留痕即可，无需解析 id。
func growthChatPlain(sa *storedAuth, model, prompt string) error {
	rc, _, err := growthChatStream(sa, model, "", prompt, false, false)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	return rc.Close()
}

// growthDesktopChatID 桌面指纹真实对话，从 SSE 流解析服务端 requestId（首个
// 匹配 growthServerIDRe 的 data.id）。命中即关流（JOIN 类只需要 id，wb2api
// 同款语义）；扫描带水位线避免跨块键值与非匹配 id 遮挡后续匹配。
func growthDesktopChatID(sa *storedAuth, expertID, prompt string) (string, string, error) {
	rc, conv, err := growthChatStream(sa, "fast-model", expertID, prompt, true, true)
	if err != nil {
		return "", "", err
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 8192)
	scanned := 0 // 已扫过的无匹配前缀水位（保留尾部 8 字节处理跨块键名）
	for {
		n, rerr := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.Index(buf[scanned:], []byte(`"id":"`))
				if i < 0 {
					if len(buf)-scanned > 8 {
						scanned = len(buf) - 8
					}
					break
				}
				key := scanned + i
				rest := buf[key+len(`"id":"`):]
				end := bytes.IndexByte(rest, '"')
				if end < 0 {
					scanned = key // 键已见、值未收完整：水位停在键起点等下一块
					break
				}
				if id := string(rest[:end]); growthServerIDRe.MatchString(id) {
					_ = rc.Close()
					return conv, id, nil
				}
				scanned = key + len(`"id":"`) + end + 1
			}
			if len(buf) > 1<<20 {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = rc.Close()
	return conv, "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// growthTaskDeficit 查单任务剩余差额（target-current）；任务不存在返回 (0,0,false)。
// 动作层自查用（编排层已做达标跳过，这里收口并发/时序窗口内的重复计数）。
func growthTaskDeficit(sa *storedAuth, code string) (int64, int64, bool, error) {
	tasks, err := growthListTasks(sa)
	if err != nil {
		return 0, 0, false, err
	}
	for _, t := range tasks {
		if t.TaskCode == code {
			if t.Claimed {
				return 0, t.Target, true, nil
			}
			return t.Current, t.Target, false, nil
		}
	}
	return 0, 0, false, nil
}

// -----------------------------------------------------------------------------
// 六个真实对话类动作（挂 growthAutoActions 表）
// -----------------------------------------------------------------------------

// runAutoModelChat Model_chat_GLM5.2：glm-5.2 真实对话一条 → 模型对齐上报
// （report 的 conversationId 调用方生成，wb2api runModelChat 同款）。
func runAutoModelChat(sa *storedAuth) (string, error) {
	const modelID, modelName = "glm-5.2", "GLM-5.2"
	if err := growthChatPlain(sa, modelID, "hi，请回复一句话"); err != nil {
		return "", fmt.Errorf("glm-5.2 对话: %w", err)
	}
	if err := growthReportActivityModel(sa, fmt.Sprintf("wb-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并对齐上报", nil
}

// runAutoSkillFresh skill_1：桌面指纹真实对话（服务端 requestId）→ chat 链
// finishReason=tool_calls（技能加载语义）→ skill_info 事件 JOIN。技能条目取
// 紫川手动完成抓包（wb2api row 209）真实 id——服务端不校验具体技能，但校验
// 事件结构与真实会话 JOIN。
func runAutoSkillFresh(sa *storedAuth) (string, error) {
	conv, req, err := growthDesktopChatID(sa, "", "1+1等于几？直接回答。")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := growthDesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, growthDesktopSkillInfoEvent(
		"skill_2097350077599879168", "润泽小馆·日报撰写", conv, req, msgID))
	if err := growthReportDesktopEvent(sa, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runAutoExpertUse expert_5：使用 5 个平台专家（agent 类型，差额感知）。
func runAutoExpertUse(sa *storedAuth) (string, error) {
	return growthExpertBatch(sa, "expert_5", "agent")
}

// runAutoExpertTeamUse Expert_team_use_3：使用 3 个专家团（team 类型，差额感知）。
func runAutoExpertTeamUse(sa *storedAuth) (string, error) {
	return growthExpertBatch(sa, "Expert_team_use_3", "team")
}

// growthExpertBatch 公共实现：差额 → 真实专家列表 → 逐个（召唤链 → 6s →
// X-Expert-Id 真实对话取服务端 requestId → chat 链 + expert_actual_use JOIN）
// → 6s。单项失败继续，全失败才报错；完成数不足时如实回报。
func growthExpertBatch(sa *storedAuth, code, expertType string) (string, error) {
	cur, target, _, err := growthTaskDeficit(sa, code)
	if err != nil {
		return "", err
	}
	need := target - cur
	if need <= 0 {
		return "进度已达标", nil
	}
	experts, err := growthMarketExpertList(sa, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	var lastErr error
	for i, e := range experts {
		if int64(ok) >= need {
			break
		}
		if err := growthReportDesktopEvent(sa, growthDesktopExpertSummonSequence(e)...); err != nil {
			fail++
			lastErr = err
			continue
		}
		time.Sleep(growthExpertSummonGap)
		conv, req, cerr := growthDesktopChatID(sa, e.ExpertID, "1+1等于几？直接回答。")
		if cerr != nil {
			fail++
			lastErr = cerr
			continue
		}
		events := append(growthDesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			growthDesktopExpertActualUseEvent(e, conv, req))
		if err := growthReportDesktopEvent(sa, events...); err != nil {
			fail++
			lastErr = err
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(growthExpertSummonGap)
		}
	}
	if ok == 0 && fail > 0 {
		return "", fmt.Errorf("0/%d 完成（最后错误: %v）", need, lastErr)
	}
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s，失败 %d）", ok, expertType, fail), nil
}

// growthLighthouseExpertID 轻量云专家固定 id（wb2api row 868 判据样本）。
const growthLighthouseExpertID = "ex_2cvvUZQhDyeJ"

// runAutoExpertLighthouse Expert_lighthouse：召唤轻量云专家 → X-Expert-Id 真实
// 对话 → chat 链 agent_task_created 带 has_expert:true + expert 字段 →
// expert_actual_use LOCAL 变体（type 空、cost 0，真实样本细节）。
func runAutoExpertLighthouse(sa *storedAuth) (string, error) {
	lh := growthMarketExpert{
		ExpertID:      growthLighthouseExpertID,
		ExpertType:    "agent",
		DisplayNameZH: "腾讯轻量云专家",
		ProfessionZH:  "腾讯轻量云专家",
		Version:       "1.0.2",
	}
	// 市场列表命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := growthMarketExpertList(sa, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == growthLighthouseExpertID {
				lh = e
				break
			}
		}
	}
	if err := growthReportDesktopEvent(sa, growthDesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	time.Sleep(growthExpertSummonGap)
	conv, req, err := growthDesktopChatID(sa, lh.ExpertID, "1+1等于几？直接回答。")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := growthDesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	use := growthDesktopExpertActualUseLocal(lh, conv, req)
	use["type"] = "" // 对齐真实样本：轻量云专家 actual_use 的 type 为空、cost=0
	use["cost"] = 0
	events = append(events, use)
	if err := growthReportDesktopEvent(sa, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runAutoBlackCat black_cat（夜猫子）：23:00–08:00 本地窗口内 glm-5.2 真实
// 对话 + 模型对齐上报，差额补足。窗口外不消耗对话额度（行为不计分），提示等
// 每日 23 点补跑（checkin.go 夜间调度位）。
func runAutoBlackCat(sa *storedAuth) (string, error) {
	if !growthNightWindowFn() {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；每日 23 点自动补跑", nil
	}
	cur, target, _, err := growthTaskDeficit(sa, "black_cat")
	if err != nil {
		return "", err
	}
	need := target - cur
	if need <= 0 {
		return "进度已达标", nil
	}
	ok := 0
	for i := int64(0); i < need; i++ {
		if err := growthChatPlain(sa, "glm-5.2", "1+1等于几？直接回答。"); err != nil {
			return "", fmt.Errorf("第 %d/%d 次对话: %w", i+1, need, err)
		}
		if err := growthReportActivityModel(sa, fmt.Sprintf("wb-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return "", fmt.Errorf("第 %d/%d 次上报: %w", i+1, need, err)
		}
		ok++
		if i < need-1 {
			time.Sleep(growthNightChatGap)
		}
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// growthInNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区，
// wb2api WorkBuddy-Daily 实测口径：窗口外对话行为不计分）。
func growthInNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

var growthNightWindowFn = func() bool { return growthInNightWindow(time.Now()) }

// growthNightTaskHours 夜间补跑位（本地时）：black_cat 只在 23:00–08:00 窗口
// 计分，调度器在 23 点为其补跑（checkin.go schedulerLoop 同 tick 判定）。
var growthNightTaskHours = []int{23}

// shouldRunNightTaskNow 当前 tick 是否落在夜间补跑位后的 1 小时窗口内
// （与 shouldRunKeepaliveNow 同型）。
func shouldRunNightTaskNow(now time.Time) bool {
	for _, h := range growthNightTaskHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(t) && now.Before(t.Add(time.Hour)) {
			return true
		}
	}
	return false
}

// growthNightTaskTick 每日 23 点补跑位：对所有 CN 账号尝试 black_cat（窗口内
// 才真正消耗对话）。与用户触发的每日循环共用 per-account checkin 锁互斥。
// 单账号失败只记日志，不影响其他账号。
func growthNightTaskTick() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	for _, f := range files {
		f := f
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil || !tasksSupportsGrowth(sa) {
			continue
		}
		mu := checkinLockFor(f.AuthIndex)
		mu.Lock()
		msg, err := runAutoBlackCat(sa)
		mu.Unlock()
		switch {
		case err != nil:
			log.Printf("workbuddy: 夜间补跑 black_cat 失败（%s）: %v", sa.Account.Nickname, err)
		case msg != "" && msg != "进度已达标":
			log.Printf("workbuddy: 夜间补跑 black_cat（%s）: %s", sa.Account.Nickname, msg)
		}
	}
}
