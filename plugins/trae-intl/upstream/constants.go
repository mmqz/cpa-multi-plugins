// constants.go Trae Intl 上游技术常量。
// Trae Intl 走 Web SOLO remote 协议（core-normal.trae.ai），与 CN 的 IDE SOLO
// 协议（trae-api-cn.mchost.guru）不同。OAuth 走 api.marscode.com（Intl）而非
// api.trae.cn（CN）。
package upstream

const (
	// Chat / models 走 Web SOLO remote endpoint（Intl 专属，CN 没有）
	AgentHost = "https://core-normal.trae.ai"
	// 用户/计费走 grow-normal.trae.ai（Intl v1，CN 用 v2）
	UgHost = "https://grow-normal.trae.ai"
	// OAuth 走 api.marscode.com（Intl）/ api.trae.ai（候选）
	OAuthHost = "https://api.marscode.com"
	// 控制台前端
	ConsoleHost = "https://www.trae.ai"
	// Intl non-solo client_id（与 CN non-solo 共享）
	ClientID = "ono9krqynydwx5"
	// AppID（Intl 与 CN 共享）
	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// IDE 版本（cockpit-tools 用 3.5.66，traework2api 用 0.1.43）
	IdeVersion     = "3.5.66"
	IdeVersionCode = "20260811"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	// Function 在 Intl Web SOLO 协议里不用（这是 CN IDE 协议的字段）
	// 保留空字符串以兼容代码引用
	Function = ""

	// 端点 — Intl Web SOLO remote 协议
	EpChat          = "/api/remote/v1/chat_sessions" // POST 创建 session
	EpChatEvents    = "/api/remote/v1/chat_sessions/{id}/events" // GET 拉 SSE
	EpModels        = "/api/remote/v1/models"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpAuthCode      = "/trae/api/v3/oauth/ExchangeToken" // AuthCode → Token
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpLoginGuidance = "/cloudide/api/v3/trae/GetLoginGuidance"
	// Intl 用 v1 pay 接口（CN 用 v2）
	EpPayStatus    = "/trae/api/v1/pay/ide_user_pay_status"
	EpEntUsage     = "/trae/api/v1/pay/ide_user_ent_usage"
	// Intl 无签到接口
	EpCheckinStatus = ""
	EpCheckinClaim  = ""
)
