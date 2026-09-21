// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	ClientID    = "ono9krqynydwx5" // non-solo (Trae Code CN)
	AppID       = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// v0.12.49: 0.1.61/20260820 = 真实客户端 0.1.61 抓包值（dsh-router-traework
	// constants.ts 2026-09-03 实测）。版本码决定上游返回哪张模型配置表：
	// 20260716 → 35 模型无 glm-5.3，调 glm-5.3 得 HTTP 200 + 流内 4001；
	// 20260820 → 36 模型含 glm-5.3，列表与实际可用性是同一张表。
	IdeVersion     = "0.1.61"
	IdeVersionCode = "20260820"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	// v0.12.79 (issue #9): historical cn default, NO longer used by any
	// request — payload builders and FetchModels all go through
	// FunctionFor(variant), which now returns solo_work_lite for every
	// variant (llm_utils_chat rejects everything else with 4001).
	Function = "inline_chat" // dead value, kept for compat only

	// 端点
	EpChat     = "/api/agent/v3/llm_utils_chat"
	EpModels   = "/api/ide/v1/get_detail_param"
	EpExchange = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo = "/cloudide/api/v3/trae/GetUserInfo"
	// v0.12.44: 登录态/设备绑定探测（cockpit-tools trae_account_core_refresh.rs
	// TRAE_CHECK_LOGIN_PATH）——响应 Result 携带 IsLogin / BoundDeviceID /
	// DeviceBindStatus / Host / Region / AIRegion，是凭证自证与 9074 设备
	// 绑定诊断的数据源（官方客户端本地存储的 trae_server_raw 同形状）。
	EpCheckLogin    = "/cloudide/api/v3/trae/CheckLogin"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
	// v0.12.29: 上游 cockpit-tools 刷新链路的另一数据源（trae_account_core_refresh.rs
	// TRAE_PAY_STATUS_PATH）——detail.fast_request_per（快请求/月）、
	// quota.solo_agent_parallel_limit（SOLO 并发）、user_pay_identity_str（计划标识）。
	// Free CN/SOLO 账户在 ent_usage 里没有可解析 quota，剩余只能靠这里补真实数值。
	EpPayStatus = "/trae/api/v1/pay/ide_user_pay_status"
)
