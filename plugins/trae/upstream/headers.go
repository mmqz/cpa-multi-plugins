// headers.go SOLO 三类请求头：对话（SOLOHeaders）/ ug（UgHeaders）/ oauth（OAuthHeaders）。
package upstream

import (
	"net/http"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

const clientUA = "Trae/" + IdeVersion

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头。
// 规则来自 SPEC §1 SOLO headers（实测必须）。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// UgHeaders 设置积分/权益（api.trae.cn pay/usage）所需头，Cloud-IDE-JWT 方案。
// 对齐 cockpit-tools request_trae_pay_json（trae_account_token_injection.rs:1595）：
// ide_user_pay_status / ide_user_ent_usage 上游用 Cloud-IDE-JWT。
func UgHeaders(req *http.Request, a *auth.Auth) {
	ugBaseHeaders(req, a)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
}

// UgCheckinHeaders 设置签到（checkin_credits/*）所需头，Bearer 方案。
// 对齐 cockpit-tools get_trae_checkin_status / claim_trae_checkin（:2761,2859）：
// 同一 token 上游在签到端点用 Bearer 而非 Cloud-IDE-JWT。
// v0.12.35 实证：Cloud-IDE-JWT 方案下 status 读接口放行、claim 写接口
// 被服务端拒绝并报 biz_code=9074（"当前参与用户太多"），改 Bearer 后对齐。
func UgCheckinHeaders(req *http.Request, a *auth.Auth) {
	ugBaseHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.JWT()) // 读锁快照
}

// ugBaseHeaders 是 UgHeaders/UgCheckinHeaders 的公共头集：
//   - x-app-type: trae
//   - Origin/Referer: https://www.trae.cn
//   - x-device-id
func ugBaseHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-User-Region", "CN")
	req.Header.Set("x-app-type", "trae")
	req.Header.Set("Origin", "https://www.trae.cn")
	req.Header.Set("Referer", "https://www.trae.cn/")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
		req.Header.Set("x-device-id", a.DeviceID)
	}
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}
