// task_events.go 行为指纹上报协议层（对齐 wb2api 2026-09-12 桌面指纹逆向与
// 小程序口径）——growth 任务中心"行为事件即判据"域的发射端。
//
// 同一个 POST /v2/report 端点凭客户端指纹区分四条通道：
//
//	CLI    billing 域 www.codebuddy.cn   （growthBillingCall，v0.9.13 已有）
//	桌面   chat 域   copilot.tencent.com + WorkBuddy/5.5.6 UA + 事件内
//	       ideName/ideType=WorkBuddy、extName=workbuddy-desktop 指纹族
//	web    www.workbuddy.cn + x-client-platform: web + 浏览器形状事件
//	mp     billing 域 + X-Client-Platform: mp-weixin 头族 + workbuddy-mp 指纹
//
// 判据任务（全部多账号实测，wb2api autotask.go 2026-09 口径）：
//
//	RichMeow_Chat  桌面六事件链（agent_task_created→chat_request_response）
//	Buddy_App(_QQ) 桌面 buddyapp 五连；automation_1 桌面单事件
//	Library_read   web 域 web_element_click(library_doc_intro_click)
//	template_5 / playbook_prompt / create_canvas  桌面事件组 JOIN chat 链
//	Hp_Appearance  appearance/set API + appearance_skin_apply 事件
//	school_season / Sequential_Tasks_1  mp chat 事件（前者必带 activityId）
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// growthDesktopUA 实测桌面客户端 UA（5.5.6 内嵌 CLI 2.137.1）。
const growthDesktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

// growthDesktopAppearanceSet 桌面外观设置端点（chat 域）。
const growthDesktopAppearanceSet = "/v2/user-asset/appearance/set"

// growthMPOpenDayID 校园日（school_season）判据 activityId——与 school 域开学季
// 同活动关联；实测无 activityId 的 mp 事件不点亮。
const growthMPOpenDayID = "school_open_day_2026"

// growthDesktopEvent 桌面端事件：业务字段任意（map），公共指纹由
// growthReportDesktopEvent 注入（业务键优先，可覆盖指纹做真实设备对齐）。
type growthDesktopEvent map[string]any

// growthDeriveID 由 uid 稳定派生 36 位 hex 设备标识（machineId/sessionId 复用），
// 幂等：同一账号每次生成相同值，模拟固定设备。
func growthDeriveID(sa *storedAuth, salt string) string {
	uid := ""
	if sa != nil {
		uid = sa.Account.UID
	}
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// growthDesktopFingerprint 公共桌面指纹字段（注入每个事件，覆盖同名业务键之前
// 先铺底；业务字段后注入优先）。
func growthDesktopFingerprint(sa *storedAuth) map[string]any {
	now := time.Now().UnixMilli()
	nickname := ""
	uid := ""
	if sa != nil {
		nickname = sa.Account.Nickname
		uid = sa.Account.UID
	}
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       uid,
		"username":     nickname,
		"userNickname": nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    growthDeriveID(sa, "machine"),
		"sessionId":    growthDeriveID(sa, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// growthReportDesktopEvent 以桌面客户端指纹向 copilot.tencent.com/v2/report
// 批量上报事件。events 为业务载荷（eventCode 等由调用方给出）；公共指纹铺底、
// 业务字段覆盖。通道走 growthBase（chat 域）+ billing 头族，UA/X-Domain/X-Product
// 按桌面客户端实测形状覆盖。
func growthReportDesktopEvent(sa *storedAuth, events ...growthDesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	fp := growthDesktopFingerprint(sa)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, growthBase()+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	billingHeaders(req, sa)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", growthDesktopUA)
	req.Header.Set("X-Domain", growthBase())
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", growthDeriveID(sa, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000))
	_, err = growthDo(req)
	return err
}

// growthDesktopChatSequence 构造一次「桌面端成功对话」完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// 实测该链点亮 RichMeow_Chat。conversationID/requestID/messageID 由调用方生成。
func growthDesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []growthDesktopEvent {
	mk := func(code string, extra map[string]any) growthDesktopEvent {
		ev := growthDesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []growthDesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// growthDesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测两账号纯
// API 点亮 Buddy_App 与 Buddy_App_QQ）：discover → show → enter_click →
// auth_confirm → bindaccount_skip。服务端不校验真实授权。
func growthDesktopBuddyAppSequence(buddyID, buddyName string) []growthDesktopEvent {
	mk := func(code string, extra map[string]any) growthDesktopEvent {
		ev := growthDesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []growthDesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// growthDesktopAutomationCreateEvent 构造「定时任务创建成功」事件（实测两账号
// 纯 API 点亮 automation_1），无需真实创建定时任务。
func growthDesktopAutomationCreateEvent(name string) growthDesktopEvent {
	return growthDesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// growthDesktopTemplateUseSequence 构造「使用模板创建任务」事件组（template_5
// 计数）：agent_task_created_with_template + template_used JOIN 一条完整 chat 链。
// 三账号实测 5 组（不同模板）一次上报 → 5/5 点亮。
func growthDesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []growthDesktopEvent {
	events := growthDesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		growthDesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		growthDesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// growthDesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（playbook_prompt
// 计数）：web_element_click(playbook_ctaClick) + playbook_cta_click +
// playbook_prompt_send（Dialog 发送，JOIN chat 链）。三账号实测 1/1 点亮。
func growthDesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []growthDesktopEvent {
	events := growthDesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	click := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
	for k, v := range payload {
		click[k] = v
	}
	send := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
	for k, v := range payload {
		send[k] = v
	}
	return append(events,
		growthDesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		growthDesktopEvent(click),
		growthDesktopEvent(send),
	)
}

// growthDesktopDesignCanvasSequence 构造「设计创意画布」事件组（create_canvas
// 计数）：wbx_design_canvas_task_create + wbx_design_canvas_open。三账号实测 1/1。
func growthDesktopDesignCanvasSequence(conversationID, requestID string) []growthDesktopEvent {
	events := growthDesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	return append(events,
		growthDesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		growthDesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + requestID[len(requestID)-8:],
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// growthDesktopExpertSummonSequence 构造「召唤平台专家」事件组，载荷对齐真实
// 抓包样本（wb2api Sunny row 2644）：web_element_click(expert_summon_click) →
// expert_summon_click → expert_summoned。需配合真实对话（服务端 requestId）+
// expert_actual_use 才计数——编造专家 id 或自造 requestId 均不计数（三账号实测）。
func growthDesktopExpertSummonSequence(e growthMarketExpert) []growthDesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []growthDesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// growthDesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5 /
// Expert_team_use_3 计数）。requestID 必须是真实对话返回的服务端 requestId。
func growthDesktopExpertActualUseEvent(e growthMarketExpert, conversationID, requestID string) growthDesktopEvent {
	ev := growthDesktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// growthDesktopExpertActualUseLocal mode:"LOCAL" 变体（Expert_lighthouse 判据
// 要求 LOCAL，对齐真实样本 Sunny row 868：轻量云专家使用时 type 为空、cost=0
// ——调用方在返回值上覆写这两个键）。
func growthDesktopExpertActualUseLocal(e growthMarketExpert, conversationID, requestID string) growthDesktopEvent {
	ev := growthDesktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// growthDesktopExpertActualUse expert_actual_use 公共载荷。
func growthDesktopExpertActualUse(e growthMarketExpert, conversationID, requestID string) growthDesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return growthDesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// growthDesktopSkillInfoEvent 构造 skill_info 技能加载事件（skill_1 判据，
// 紫川手动完成抓包 row 209）：真实技能 id + toolStatus=success + JOIN 真实会话
// （conversationId/requestId 为服务端 id）。此前的 skill_request_send /
// skill_installed / skill_action 均是错误方向（不计数）。
func growthDesktopSkillInfoEvent(skillID, skillName, conversationID, requestID, messageID string) growthDesktopEvent {
	return growthDesktopEvent{
		"eventCode": "skill_info",
		"id":        skillName, "skillId": skillID, "skillVersion": "1.0.0",
		"toolStatus": "success", "fileCount": 56, "source": "workbuddy-desktop",
		"conversationId": conversationID, "requestId": requestID, "messageId": messageID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": requestID,
	}
}

// growthSetAppearanceTheme 应用外观主题（chat 域 /v2/user-asset/appearance/set，
// {kind:"theme",resource_key}）。Hp_Appearance 判据 = set API 留痕 + 随后的
// appearance_skin_apply 事件（只 set 不发事件不计分——wb2api 实测修正）。
func growthSetAppearanceTheme(sa *storedAuth, resourceKey string) error {
	raw, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, growthBase()+growthDesktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	billingHeaders(req, sa)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", growthDesktopUA)
	req.Header.Set("X-Product", "SaaS")
	_, err = growthDo(req)
	return err
}

// growthReportWebEvent 以 Web 端指纹向 www.workbuddy.cn/v2/report 上报单事件。
// 浏览器形状（os/machineId/userAgent），Library_read 等页面行为类任务判据
// （library_doc_intro_click 实测 4 秒点亮）。
func growthReportWebEvent(sa *storedAuth, eventCode, pageURL, elementID, elementName string) error {
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	uid, nickname, enterprise := "", "", ""
	if sa != nil {
		uid = sa.Account.UID
		nickname = sa.Account.Nickname
		enterprise = sa.Account.EnterpriseID
	}
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": growthDeriveID(sa, "webmachine"), "userId": uid,
		"userNickname": nickname, "enterpriseId": enterprise,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, growthWebBaseCN+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	billingHeaders(req, sa)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", growthWebBaseCN)
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", ua)
	_, err = growthDo(req)
	return err
}

// growthMPEventBase 小程序埋点公共指纹（appservice wQ()+Ao() 对齐）。
func growthMPEventBase(sa *storedAuth) map[string]any {
	uid, nickname := "", ""
	if sa != nil {
		uid = sa.Account.UID
		nickname = sa.Account.Nickname
	}
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone":     "Asia/Shanghai",
		"userId":       uid,
		"userNickname": nickname,
	}
}

// growthReportMPEvent 以小程序指纹向 billing 域 /v2/report 批量上报事件
// （mp 专属任务的判据通道；头族 X-Client-Platform: mp-weixin 等）。
func growthReportMPEvent(sa *storedAuth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := growthMPEventBase(sa)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, billingBaseFor(sa)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	billingHeaders(req, sa)
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = growthDo(req)
	return err
}

// growthMPChatEvent 构造一条 mp 指纹 chat_request_send（小程序口径任务判据）。
// withActivityId=true 追加校园日 activityId（school_season 必带；Sequential_Tasks_1
// 不带——服务端按 source=mini_program 指纹关联）。
func growthMPChatEvent(conversationID string, withActivityId bool) map[string]any {
	rid := "wb-" + growthClientToken()
	ev := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
	if withActivityId {
		ev["activityId"] = growthMPOpenDayID
	}
	return ev
}

// growthCallMP 发 growth 域请求（小程序口径：billing 头族叠加
// X-Client-Platform: miniprogram）。mp 专属任务（school_season /
// Sequential_Tasks_1）的列表下发、accept、claim 全链路要求该头，缺头 accept
// 返回 task not found（wb2api 实测）。语义同 growthCall。
func growthCallMP(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, growthBase()+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	req.Header.Set("X-Client-Platform", "miniprogram")
	return growthDo(req)
}

// growthClaimRewardMP 领取小程序限定任务奖励：chat 域 mp 口径
// /activity/growth/tasks/{code}/claim，400 时降级 Web 域领奖端点（部分租户
// 形态 chat 域 400，web 域可领——wb2api 实测）。返回 (credit, energy, err)。
func growthClaimRewardMP(sa *storedAuth, taskCode string) (int64, int64, error) {
	req, err := http.NewRequest(http.MethodPost,
		growthBase()+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", bytes.NewReader(nil))
	if err != nil {
		return 0, 0, err
	}
	billingHeaders(req, sa)
	req.Header.Set("X-Client-Platform", "miniprogram")
	data, err := growthDo(req)
	if err != nil {
		if he, ok := err.(*growthHTTPError); ok && he.status == http.StatusBadRequest {
			return growthClaimReward(sa, taskCode)
		}
		return 0, 0, err
	}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil
	}
	return resp.Credit, resp.Energy, nil
}
