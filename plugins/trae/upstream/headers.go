// headers.go SOLO 三类请求头：对话（SOLOHeaders）/ ug（UgHeaders）/ oauth（OAuthHeaders）。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

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
// v0.12.45: 身份画像随 ugBaseHeaders 对齐官方抓包（VSCode 插件进程 UA）。
func UgHeaders(req *http.Request, a *auth.Auth) {
	ugBaseHeaders(req, a)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
}

// UgCheckinHeaders 已移除（v0.12.38）：签到鉴权改为双方案探测
// （Cloud-IDE-JWT 优先、Bearer 回退），实现见 client.go ugCheckinRequest。

// v0.12.45 ug（签到/积分）请求头 = 2026-09-03 抓包【成功签到请求】逐头复刻
// （证据：dsh-router-traework ugHeaders，trae-capture/parsed_flows.jsonl；
// 用户账号 37396015402 实测：旧头 9074 多日，新头当目 claim code=0）：
//   - UA 必须是 VSCode 插件进程 "VSCode 1.107.1 (TRAE SOLO CN)"——签到/积分
//     走 VSCode 插件进程，与 IDE 主进程 Trae/0.1.x（clientUA）、
//     TraeClient/TTNet 均不同；同一账号混用三套客户端身份 = 风控画像对不上。
//   - 无 Origin/Referer/x-app-type（此前是 WebView 画像，真实客户端不携带）。
//   - Accept 实测 "*/*"；Sec-Fetch-Mode 实测 "no-cors"（不是 cors）。
//   - App-Version 抓包为 IDE 版本 0.1.61；不用 v0.12.40 从 out/main.js fb()
//     反编译的桌面壳版本 x-app-version=2.3.81345（IDE 主进程画像，已废弃）。
//   - X-Request-Id（uuid-v4）+ X-TT-Trace-Id（00-<32hex>-<16hex>-01，16hex 取
//     自请求 ID）每请求新生成，与真实客户端一致。
const (
	// ugMarketClientID X-Market-Client-Id 与 ug UA 同源但不带平台后缀（抓包值）。
	ugMarketClientID = "VSCode 1.107.1"
	// ugIdeVersion 抓包请求 App-Version 携带的 IDE 版本。
	ugIdeVersion = "0.1.61"
)

// ugUserAgentFor ug 请求 UA：VSCode 插件进程身份，平台名随账号谱系
// （抓包实证 solo→"VSCode 1.107.1 (TRAE SOLO CN)"；其余谱系按官方产品名
// PlatformNameFor 同构映射，未逐一抓包验证）。
func ugUserAgentFor(variant string) string {
	return ugMarketClientID + " (" + PlatformNameFor(variant) + ")"
}

// ugPackageTypeFor CN 谱系 stable_cn（抓包值）；intl 谱系无抓包样本，按官方
// 渠道命名惯例给 stable（如与实测冲突以抓包为准）。
func ugPackageTypeFor(variant string) string {
	if IsIntlVariant(variant) {
		return "stable"
	}
	return "stable_cn"
}

// newRequestID 生成 uuid-v4 形请求标识（抓包格式 8-4-4-4-12）。
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ttTraceID 生成 tt-trace-id（抓包格式 00-<32hex>-<16hex>-01，16hex 取自请求 ID）。
func ttTraceID(requestID string) string {
	t := make([]byte, 16)
	_, _ = rand.Read(t)
	return "00-" + hex.EncodeToString(t) + "-" + strings.ReplaceAll(requestID, "-", "")[:16] + "-01"
}

// ugBaseHeaders 是 UgHeaders/ugCheckinRequest 的公共头集（v0.12.45 抓包指纹，
// 逐头说明见上方 block 注释）。Authorization 由调用方按方案设置。
func ugBaseHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", ugUserAgentFor(a.Variant))
	req.Header.Set("X-User-Region", "CN") // 抓包值；intl 维持存量行为（未验证差异）
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Package-Type", ugPackageTypeFor(a.Variant))
	req.Header.Set("X-Lgw-Req-Sdk-Type", "3")
	req.Header.Set("X-Market-Client-Id", ugMarketClientID)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("App-Version", ugIdeVersion)
	rid := newRequestID()
	req.Header.Set("X-Request-Id", rid)
	req.Header.Set("X-TT-Trace-Id", ttTraceID(rid))
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", "none")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}
