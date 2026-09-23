// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 成功
	ErrPlanLimit                    // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                     // 429 → 短冷却 60s
	ErrSessionDead                  // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                     // 404 → 短冷却 60s 不累计 errCount
	ErrServer                       // 5xx
	ErrClient                       // 其他 4xx
	ErrInputTooLarge                // v0.12.50: 413/过大文案 → 请求级问题，不冷却账号
	// v0.12.79 (issue #9): 4001 = 模型在当前 function 通道不可用
	//（trae2api-more IsModelConfigMismatch 语义）。请求级失败：换任何
	// 账号结果相同，不冷却、不累计 errCount，避免死模型拖垮健康账号。
	ErrModelUnavailable
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrInputTooLarge:
		return "input_too_large"
	case ErrModelUnavailable:
		return "model_unavailable"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// BizCode 携带上游业务码（如签到 9074 限流、非零 code=token 失效），
	// 与 HTTP Status 互不影响。0 = 非 HTTP 类错误。
	BizCode int32
}

func (e *Error) Error() string {
	if e.BizCode != 0 {
		return fmt.Sprintf("upstream biz_code=%d: %s", e.BizCode, e.Msg)
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// bizError 构造业务码错误。v0.12.38 起优先透传上游响应的 message 原文——
// 此前照抄 cockpit-tools 的兜底文案（rs:2788 对一切非零码硬编码
// "Token 已过期，请重新登录"）并把上游真实 message 丢弃，导致 v0.12.35
// 的 1001（实为 Bearer 方案拒绝我们的 token 类别）被误读成 token 过期。
// 上游 message 为空时才回退到本地兜底文案。9074 归类软限流，
// 其余非零码按会话失效处理（pool 据此禁用）。
func bizError(code int32, prefix, upstreamMsg string) *Error {
	detail := strings.TrimSpace(upstreamMsg)
	if detail == "" {
		if code == 9074 {
			detail = "当前参与用户太多，请稍后再试"
		} else {
			detail = "Token 已过期，请重新登录"
		}
	}
	msg := fmt.Sprintf("%s (code=%d): %s", prefix, code, detail)
	kind := ErrSessionDead
	if code == 9074 {
		kind = ErrSoftRate
	}
	return &Error{Kind: kind, Msg: msg, BizCode: code}
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// inputTooLargeMarkers 大输入判定的子串词表（大小写不敏感；中文原样匹配）。
var inputTooLargeMarkers = []string{
	"too long",
	"too many tokens",
	"context length",
	"maximum context",
	"context window",
	"request entity too large",
	"input too large",
	"输入过长",
	"内容过长",
	"上下文过长",
	"上下文长度",
	"超出模型上限",
}

// MsgIndicatesInputTooLarge 判定上游错误文案是否为「输入过大」。
// v0.12.50: 大输入（上下文超出模型窗口/请求体超网关限制）是请求级问题，
// 与账号健康无关——命中词表的错误不冷却账号并给明确指引（对齐
// qoder 0.8.11 chatSizeMarkers；Coding2API 400→INVALID 不冷却同哲学）。
func MsgIndicatesInputTooLarge(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range inputTooLargeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// v0.12.50: 输入过大（上下文超模型窗口/请求体超网关限制）是请求级
	// 问题——同一 body 在任何账号上都会被拒，冷却账号只会误伤（原路径
	// 400→ErrClient→NoteError 累计 3 次→冷却 10 分钟，大输入连撞会拖垮
	// 健康账号）。413 语义唯一；其余 4xx 命中过大文案同样归此类。
	// v0.12.51: 413 判定提到最顶——body 恰好带 "1005…plan" 字样时不得被
	// 宽松 plan 匹配劫持成 ErrPlanLimit（那会硬冷却健康账号 12h），
	// 让「413 语义唯一」真正落实到代码顺序。
	if status == http.StatusRequestEntityTooLarge {
		return ErrInputTooLarge
	}
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// v0.12.48: 4008 quota 也可能裹在非 2xx 响应体里——与流内
	// SOLOStreamError.Kind 共用同一张判定表（soloPlanLimitCodes），
	// 避免两条路径对一个码给出两种语义（对齐 dsh-router-traework 2026-09-15）。
	if strings.Contains(body, `"code":4008`) {
		return ErrPlanLimit
	}
	if status >= 400 && status < 500 && MsgIndicatesInputTooLarge(body) {
		return ErrInputTooLarge
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// v0.12.79 (issue #9): body 带 code=4001（非过大/401/429/404 语义）=
	// 模型在当前 function 通道不可用（trae2api-more IsModelConfigMismatch
	// 同语义）。请求级失败：与账号健康无关，不冷却。放在账号级状态
	// （401/429/404）之后做兜底——鉴权失败永远优先按会话失效处理；
	// 过大判定在其上方，{"code":4001,"msg":"prompt is too long…"} 仍归
	// ErrInputTooLarge。
	if status >= 400 && status < 500 && strings.Contains(body, `"code":4001`) {
		return ErrModelUnavailable
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) && !auth.IssuedTooLongLocked(a.AccessToken) {
		// v0.12.49: 临到期判之外加签发龄判——iat 距今超 15 天也轮换
		//（服务端吊销旧凭据风险，见 auth.issuedRotateMax 注释）。
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
// 多源 fallback（对齐 cockpit-tools build_api_urls）：依次尝试
// a.APIHost → OAuthHost → 备用 CN 源，避免单一 host 不可达时刷新失败。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	hosts := exchangeHosts(a.APIHost, c.OAuthHost)
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range hosts {
		req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		OAuthHeaders(req)
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
		// 404/5xx/网络错误 → 换下一个源；业务错误（如 token 无效）也换源重试一次，
		// 因为部分源只对特定账号域生效。
	}
	if lastErr != nil {
		return lastErr
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// issue #13 诊断：出站前的指纹行（TRAE_DEBUG_PAYLOAD=1 时输出），记录
	// 上游真正收到的白名单产物——与执行器入口的 raw 行按指纹对齐即可判断
	// 两条执行链的出站请求是否逐字节一致。
	prepared := PrepareBody(body, a.Variant)
	LogPreparedHead(a.UID, a.Variant, body, prepared)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断。
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64 // = context_window_tokens.dev（目录未给则 0）
	MaxTokens     int64 // 目录不透出输出上限，恒 0（model_detail_list 为加密参数）
}

// soloAgentOnlyDeadNames 目录里的死模型精确名单（issue #9，2026-09-21 双账号
// 实测；issue #10 修正，2026-09-22 三账号实测）：这些 config 在 solo_work_lite
// 通道必定流内 4001 —— 它们由 IDE 加密 agent 通道 / llm_raw_chat（solo_agent
// function）服务，llm_utils_chat 不提供。过滤优于注册后报错：客户端永远选
// 不到必然失败的模型。
//
// v0.12.83 (issue #10)：原前缀匹配（"agnes"、"deepseek-v4"）过宽，把实测可用的
// DeepSeek-V4-{Flash,Pro}-Official 一起误杀 —— 两者在同一 solo_work_lite 通道
// 200 正常出话（报告者三凭据复现；判别依据：目录里 -Official 段是与非正式
// 死键不同的独立 config，命名即"正式版"条目）。改为精确 config_name 集合，
// 只收当场验证过的死键。代价是新死模型会先放行，而 PR #11 之后流内 4001 已是
// 可降级的 404 失败（不再是 200 空壳），放行风险可接受；确认新的死模型后在
// 这里补名字。大小写不敏感（目录同时出现 DeepSeek-V4-Flash 与 deepseek-v4-flash
// 两种写法）。
var soloAgentOnlyDeadNames = map[string]struct{}{
	"agnes-2.0-flash":   {},
	"agnes-agent-x":     {},
	"deepseek-v4-flash": {},
	"deepseek-v4-pro":   {},
}

// configIsSoloAgentOnly reports whether a catalog config_name is served only
// by the solo_agent/llm_raw_chat lane and therefore dead on llm_utils_chat.
func configIsSoloAgentOnly(configName string) bool {
	_, dead := soloAgentOnlyDeadNames[strings.ToLower(configName)]
	return dead
}

// FetchModels 拉 SOLO/CN 模型表（get_detail_param），只返回用户可见的正式条目。
// 目录同时携带三类非可选配置，必须过滤（v0.12.46，2026-09-12 对 38 条实测目录校准）：
//   - is_invisible_to_user=true：内部 subagent/实验通道（browser_use_subagent、
//     file_search_agent、sagitta/aquila、seed-code-pro-0430 等内部别名）；
//   - display_name 为空：租户自定义模型占位模板（custom_model_* / summary 等）；
//   - config_switch=false：已下线开关。
//
// 其余含 is_beta / is_custom_model=true 且有正式显示名的条目照常透出（官方
// 模型选择器同样展示它们）。可布尔字段用指针判缺失：上游省略时按
// 可见/启用处理，避免字段缺省把整个目录过滤空。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            FunctionFor(a.Variant),
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName          string `json:"config_name"`
			ConfigSwitch        *bool  `json:"config_switch"`
			IsInvisibleToUser   *bool  `json:"is_invisible_to_user"`
			ContextWindowTokens struct {
				Dev int64 `json:"dev"`
			} `json:"context_window_tokens"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		if cfg.ConfigSwitch != nil && !*cfg.ConfigSwitch {
			continue // 已下线开关
		}
		if cfg.IsInvisibleToUser != nil && *cfg.IsInvisibleToUser {
			continue // 内部配置，非用户可选模型
		}
		if cfg.DisplayConfig.DisplayName == "" {
			continue // 租户自定义占位模板
		}
		// v0.12.79 (issue #9): solo_agent-only 死模型 —— 在本插件的唯一
		// 聊天通道上必 4001，注册出来只会让客户端选到死模型。
		if configIsSoloAgentOnly(cfg.ConfigName) {
			continue
		}
		out = append(out, ModelInfo{
			ID:            cfg.ConfigName,
			Name:          cfg.DisplayConfig.DisplayName,
			ContextWindow: cfg.ContextWindowTokens.Dev,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned no user-facing models")
	}
	return out, nil
}

// CheckinStatus 查询签到状态。
// v0.12.41: 对齐官方客户端契约（反编译 TraeCode CN 2.3.79946 deb 与 TraeWork
// CN 2.3.81345 exe（装出 "TRAE SOLO CN"）两版 out/main.js 交叉实证）：
// POST /trae/api/v2/ug/checkin_credits/status，body {"req_source":N}
// （N 走探测序列 ugCheckinReqSources），无 did query。响应为扁平结构：
//
//	{enable, checked_in, did_checked_in, credits, extra_credits}
//
// 其中 credits = 每日签到奖励数额（官方卡片 "Daily check-in: {credits} credits"），
// 并非可花余额——此前误标"签到钱包"（v0.12.30 时代的误读）。
// 旧 GET + did query（cockpit-tools rs:2761）在 09-04 上游收紧前可用，现已弃用。
// 鉴权双方案（v0.12.38）：Cloud-IDE-JWT 优先，非 9074 失败回退 Bearer 一次
// （ugCheckinSchemes/ugCheckinOnce）。
// 返回完整字段：CheckedIn / Credits / Enable + 业务码 Code（用于 9074 限流识别）。
// 对齐上游 code!=0 语义（trae_account_token_injection.rs:2786-2791）：
// 非零业务码 → 返回错误（上游 message 透传），绝不能当成 "未签到" 静默通过。
// 9074（活动校验拒绝）作为 *Error{BizCode:9074} 返回，调用方可识别重试。
type CheckinStatusResult struct {
	CheckedIn bool `json:"checked_in"`
	// DidCheckedIn 官方字段 did_checked_in：设备维度的"今日已签"
	// （官方 UI 文案 "This device has checked in today. Come back tomorrow."，
	// 反编译 TraeWork CN 2.3.81345 webcomponents 用户卡）。官方 claim 前置：
	// checked_in===false 且 did_checked_in!==true 才可领——同账号同设备每日
	// 仅一次，设备维度由服务端按 did 去重。
	DidCheckedIn bool  `json:"did_checked_in"`
	Credits      int64 `json:"credits"`
	// ExtraCredits 官方字段 extra_credits：会员/活动加码奖励（官方卡片
	// "Member bonus: +{extraCredits} daily"）。到账总额 = credits + extra_credits。
	// v0.12.31 "官方依次给 200 面板却是 150"悬案即源于此（150 基础 + 50 加码）。
	ExtraCredits int64  `json:"extra_credits"`
	Enable       bool   `json:"enable"`
	Code         int32  `json:"code"` // 业务码：0=成功，9074=活动校验拒绝，其他=会话类失败
	Message      string `json:"message"`
	// SchemeUsed 实际成功使用的鉴权方案（v0.12.38 双方案探测，面板诊断用）。
	SchemeUsed string `json:"-"`
	// ReqSourceUsed 实际成功使用的 req_source body（v0.12.41 双探测，日志/诊断用）。
	ReqSourceUsed string `json:"-"`
}

// v0.12.38: 签到鉴权双方案。证据链复盘：
//   - 官方客户端 claim = POST {} + Cloud-IDE-JWT + x-device-id（BlueChonk
//     FINDINGS §五 实测到账；trae-mate/traework2api/trae-work-checkin 同构）。
//   - 我方 v0.12.34 Cloud-IDE-JWT 下 status 读接口实测 code=0（积分显示正确）。
//   - v0.12.35 照搬 cockpit-tools（rs:2761,2859）改 Bearer 后 status 反报
//     biz_code=1001。cockpit-tools 的 token 来自官方客户端托管会话，与我们
//     自走 OAuth（ClientID ono9krqynydwx5）ExchangeToken 的 token 类别不同，
//     其 Bearer 经验不可平移；同期聊天/积分查询（Cloud-IDE-JWT）均正常，
//     排除 token 过期。
//
// 策略：Cloud-IDE-JWT 优先（官方客户端实证方案）；非 9074 失败回退 Bearer
// 一次（兼容 cockpit-tools 所代表的 token 类别）。每次尝试的 biz code 与
// 上游 message 全量落日志，SchemeUsed 记入结果供面板诊断。
// v0.12.40 曾在此另设设备头（x-device-brand/type/os-version + x-app-version
// 2.3.81345，out/main.js fb() 反编译值）—— v0.12.45 起全部下沉到
// ugBaseHeaders 的抓包指纹头集（App-Version 改用抓包值 0.1.61，身份改为
// VSCode 插件进程），此处仅保留鉴权方案选择。
const (
	UgSchemeCloudIDEJWT = "Cloud-IDE-JWT"
	UgSchemeBearer      = "Bearer"
)

// ugCheckinReqSourcesFor 签到类请求（status/claim）的 req_source 探测序列。
// v0.12.43 起按账号谱系（auth variant → OAuth ClientID）选序，并补入官方
// 空 body。证据链（两版官方包反编译交叉实证，out/main.js）：
//   - TraeCode CN 2.3.79946（09-01 build，deb，ClientID ono9krqynydwx5）：
//     status/claim body 均为 {}（不携带 req_source 字段）。
//   - TraeWork CN 2.3.81345（09-04 build，exe "TRAE SOLO CN"，ClientID
//     en1oxy7wnw8j9n）：body = {req_source: Dr(P) ? 2 : 1}，
//     Dr(P) = Su(P)==SOLO_Lite || packageType==SOLO_CN_ENTERPRISE，
//     Su(P): packageType∈{SOLO_CN,SOLO_I18N,SOLO_CN_ENTERPRISE}→SOLO_Lite，否则 TRAE。
//     product.json 实证该包 packageType="SOLO_CN" → Dr=true → 官方 TraeWork/SOLO
//     客户端发 2；普通 Trae CN IDE（packageType=TRAE_CN）Dr=false → 发 1。
//   - req_source 是【客户端产品谱系】而非用户套餐：TRAE 谱系配 OAuth appId
//     ono9krqynydwx5、SOLO 谱系配 en1oxy7wnw8j9n（iCubeApp.authConfig），
//     官方客户端 jb() 按 Dr(P) 二选一，两者从未交叉。
//
// v0.12.40 误读：把"用户是 SOLO 套餐"当成了 req_source=2 的依据，对我方
// TRAE 谱系 token（ClientID=ono9krqynydwx5，constants.go）发 req_source=2 →
// 请求体与 token 谱系自相矛盾；09-04 上游收紧活动校验后 claim 被通用活动
// 错误 9074 拒绝（status 只读不受校验，所以 v0.12.40 面板状态可读、claim 被拒）。
// v0.12.41 修正：req_source=1 优先（TRAE 谱系实测可用契约）+ 9074 回退 2。
// v0.12.43 补齐：官方 TraeCode CN 的原始契约是空 body {}——此前从未探测，
// 上游 09-04 收紧后 1/2 均拒的 CN 账号多一条官方同款出路；SOLO 谱系则把
// 官方同款 2 提到首位，避免跨谱系探测先打空炮。
//
//	cn（TRAE 谱系，默认）：1（实测可用）→ {}（官方 TraeCode 原始契约）→ 2（跨谱系末位兑底）
//	solo（SOLO 谱系）  ：2（官方 TraeWork 同款）→ 1（TraeWork+TRAE 包分支）→ {}（跨谱系末位兑底）
func ugCheckinReqSourcesFor(variant string) []string {
	if strings.ToLower(strings.TrimSpace(variant)) == "solo" {
		return []string{
			`{"req_source":2}`,
			`{"req_source":1}`,
			`{}`,
		}
	}
	return []string{
		`{"req_source":1}`,
		`{}`,
		`{"req_source":2}`,
	}
}

// ugBodyLabel 探测序列的短标签（诊断字符串用）。
func ugBodyLabel(body string) string {
	switch body {
	case `{"req_source":1}`:
		return "req_source=1"
	case `{"req_source":2}`:
		return "req_source=2"
	case `{}`:
		return "empty"
	}
	return body
}

// ugProbeDetail v0.12.43: 全组合被拒时追加到错误里的诊断后缀 —— 面板/日志
// 直接可见试过哪些组合、以什么身份发的请求，用于区分"契约/设备缺失"与
// "真限流"（此前两者混为一句 9074 文案，掩盖了实现侧线索）。
// v0.12.45 修正：只列实际尝试过的组合（body×scheme 标签由调用方循环收集）。
// 旧实现传 bodies 全集 + 固定 schemes 拼接，9074-break 跳过剩余 scheme 后
// 仍宣称"Cloud-IDE-JWT,Bearer 全部被拒"，夸大探测范围误导判读。
func ugProbeDetail(a *auth.Auth, attempted []string) string {
	if len(attempted) == 0 {
		return fmt.Sprintf("[无已尝试组合; variant=%s device_id_set=%v]",
			strings.TrimSpace(a.Variant), strings.TrimSpace(a.DeviceID) != "")
	}
	return fmt.Sprintf("[已尝试 %d 组合 %s 全部被拒; variant=%s device_id_set=%v]",
		len(attempted), strings.Join(attempted, ","),
		strings.TrimSpace(a.Variant), strings.TrimSpace(a.DeviceID) != "")
}

// ugCheckinSchemes 返回签到请求的鉴权方案优先级。
func ugCheckinSchemes() []string {
	return []string{UgSchemeCloudIDEJWT, UgSchemeBearer}
}

// checkinDeviceDigits 签到设备号十进制位数。
// 证据矩阵（Coding2API credential.py 2026-09-20 定稿观测，此前被推翻两次）：
// 同一账号 claim 结果——登录 deviceId(hex32) ✗、派生定值 16 位数字 ✗×4、
// 随机 16 位数字 ✓、登录 machineId(hex32) ✗、空串 → 9004 参数错误。
// 数字串是必要条件而非充分条件（9074 与具体取值关系未定），故不猜格式，
// 改为"每次生成全新随机数字串 + 失败当日退避重试"。
const checkinDeviceDigits = 16

// NewCheckinDeviceID 生成一个全新的签到设备 ID：16 位随机数字串（crypto/rand）。
// 每次调用都不同——设备号复用是 9074 的可疑诱因（"同 uid 永远同样的号"用久后
// 连续失败，换没用过的号当次即成功）。不持久化：设备号只是签到 API 的校验参数，
// 上游按 uid 记账，换号不影响发放；X-Machine-Id 仍保持登录配对（非签到校验项）。
// 注意幂等陷阱：账号当日签到成功后，任何 device_id 的 claim 都返回 code:0——
// "成功"判定必须配合 status.checked_in 回查（调用方责任，见 scheduler/management）。
func NewCheckinDeviceID() string {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(checkinDeviceDigits), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		// crypto/rand 失败极罕见；退化时间戳填充，仍保证 16 位数字形状。
		return fmt.Sprintf("%0*d", checkinDeviceDigits, time.Now().UnixMilli())
	}
	return fmt.Sprintf("%0*d", checkinDeviceDigits, n.Int64())
}

// checkinDeviceArg variadic 归一：显式传入取首值（同一轮 attempt 的
// status→claim→回查配对同一设备号，两者是配对的校验参数），否则现生成。
func checkinDeviceArg(deviceID []string) string {
	if len(deviceID) > 0 && deviceID[0] != "" {
		return deviceID[0]
	}
	return NewCheckinDeviceID()
}

// ugCheckinRequest 构造签到请求（抓包指纹公共头 + 指定方案的 Authorization）。
// v0.12.65: deviceID 非空时覆盖 ugBaseHeaders 的 X-Device-Id（登录 hex32
// 直传）——签到族请求专用，其余 ug 端点（积分查询等）不受影响。
func ugCheckinRequest(a *auth.Auth, method, url, body, scheme, deviceID string) (*http.Request, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	ugBaseHeaders(req, a)
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	}
	if scheme == UgSchemeBearer {
		req.Header.Set("Authorization", "Bearer "+a.JWT()) // 读锁快照
	} else {
		req.Header.Set("Authorization", UgSchemeCloudIDEJWT+" "+a.JWT()) // 读锁快照
	}
	return req, nil
}

// ugCheckinOnce 执行一次签到类请求，返回业务码/消息与原始 body。
// 网络层错误（doJSON）与解析错误原样上抛，不做方案回退。
func (c *Client) ugCheckinOnce(a *auth.Auth, method, url, body, scheme, deviceID string) (int32, string, []byte, error) {
	req, err := ugCheckinRequest(a, method, url, body, scheme, deviceID)
	if err != nil {
		return 0, "", nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return 0, "", nil, err
	}
	var reply struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return 0, "", nil, fmt.Errorf("checkin %s parse: %w", strings.ToLower(method), err)
	}
	return reply.Code, reply.Message, data, nil
}

func (c *Client) CheckinStatus(a *auth.Auth, deviceID ...string) (*CheckinStatusResult, error) {
	did := checkinDeviceArg(deviceID)
	bodies := ugCheckinReqSourcesFor(a.Variant)
	attempted := make([]string, 0, len(bodies)*2) // v0.12.45: 实际尝试的 body×scheme
	var lastBiz *Error
	for _, body := range bodies {
		for _, scheme := range ugCheckinSchemes() {
			code, msg, data, err := c.ugCheckinOnce(a, http.MethodPost, c.ugBase()+EpCheckinStatus, body, scheme, did)
			if err != nil {
				return nil, err
			}
			attempted = append(attempted, ugBodyLabel(body)+"×"+scheme)
			if code == 0 {
				var resp CheckinStatusResult
				if err := json.Unmarshal(data, &resp); err != nil {
					return nil, fmt.Errorf("checkin status parse: %w", err)
				}
				resp.SchemeUsed = scheme
				resp.ReqSourceUsed = body
				return &resp, nil
			}
			log.Printf("checkin status: %s scheme %s -> biz_code=%d msg=%q", ugBodyLabel(body), scheme, code, msg)
			lastBiz = bizError(code, "获取签到状态失败", msg)
			if code == 9074 {
				break // 活动校验拒绝：换 req_source 再试（v0.12.41），同源换鉴权方案无意义
			}
		}
	}
	// v0.12.43: 全组合被拒 → 错误里带上探测过的组合与身份，区分契约/设备问题与真限流。
	if lastBiz != nil {
		lastBiz.Msg += " " + ugProbeDetail(a, attempted)
	}
	return nil, lastBiz
}

// CheckinClaim 执行签到。返回业务码 Code 用于 9074 限流识别。
// v0.12.41: 对齐官方客户端契约（两版官方包 out/main.js claimCheckinCredits()
// 交叉实证）：POST /trae/api/v2/ug/checkin_credits/claim，body 走
// ugCheckinReqSources 双探测（req_source=1 优先 = TRAE 谱系 token 的正确
// 契约，9074 回退 2；谱系错配即遭 9074，见 ugCheckinReqSources 注释），
// Authorization: Cloud-IDE-JWT + x-device-id（真实绑定 did）+ 设备头。
// 响应 {code, message}，code!=0 → 错误；领取成功后由调用方重查状态。
// 响应中的 credits/add_credits/reward 等数值字段作为"入账证据"带回
// （ClaimCredits），便于诊断"code=0 但未到账"。
// v0.12.35 曾改 Bearer（照搬 cockpit-tools rs:2859），实测 status 反遭
// biz_code=1001——其 token 来自官方客户端托管会话，与自走 OAuth 的 token
// 类别不同，Bearer 经验不可平移（详见 CheckinStatus 上方证据链）。
// v0.12.38: 鉴权随 CheckinStatus 统一走双方案探测（Cloud-IDE-JWT 优先，
// Bearer 回退）；v0.12.41 起 9074 触发 req_source 换源重试（同源不换方案）。
// v0.12.65: x-device-id 改为每轮全新 16 位随机数字串（不再直传登录 hex32）；
// 证据矩阵见 NewCheckinDeviceID。claim code:0 不再被调用方直接采信为成功：
// 当日已签账号任何 device_id 的 claim 都幂等回 code:0（幂等假成功陷阱——
// v0.12.41 "req_source=1 实测可用"的旧结论可能正踩于此），成功判定必须由
// 调用方回查 status.checked_in 翻转。
type CheckinClaimResult struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
	// ClaimCredits 服务端响应里携带的积分入账数额（best-effort 提取，nil=响应未携带）。
	ClaimCredits *int64 `json:"-"`
	// SchemeUsed 实际成功使用的鉴权方案（v0.12.38 双方案探测，面板诊断用）。
	SchemeUsed string `json:"-"`
	// ReqSourceUsed 实际成功使用的 req_source body（v0.12.41 双探测，日志/诊断用）。
	ReqSourceUsed string `json:"-"`
}

func (c *Client) CheckinClaim(a *auth.Auth, deviceID ...string) (*CheckinClaimResult, error) {
	did := checkinDeviceArg(deviceID)
	bodies := ugCheckinReqSourcesFor(a.Variant)
	attempted := make([]string, 0, len(bodies)*2) // v0.12.45: 实际尝试的 body×scheme
	var lastBiz *Error
	for _, body := range bodies {
		for _, scheme := range ugCheckinSchemes() {
			code, msg, data, err := c.ugCheckinOnce(a, http.MethodPost, c.ugBase()+EpCheckinClaim, body, scheme, did)
			if err != nil {
				return nil, err
			}
			attempted = append(attempted, ugBodyLabel(body)+"×"+scheme)
			if code == 0 {
				var resp CheckinClaimResult
				if err := json.Unmarshal(data, &resp); err != nil {
					return nil, fmt.Errorf("checkin claim parse: %w", err)
				}
				resp.ClaimCredits = pickCreditField(data)
				resp.SchemeUsed = scheme
				resp.ReqSourceUsed = body
				return &resp, nil
			}
			log.Printf("checkin claim: %s scheme %s -> biz_code=%d msg=%q", ugBodyLabel(body), scheme, code, msg)
			lastBiz = bizError(code, "签到领取失败", msg)
			if code == 9074 {
				break // 活动校验拒绝：换 req_source 再试（v0.12.41），同源换鉴权方案无意义
			}
		}
	}
	// v0.12.43: 全组合被拒 → 错误里带上探测过的组合与身份，区分契约/设备问题与真限流。
	if lastBiz != nil {
		lastBiz.Msg += " " + ugProbeDetail(a, attempted)
	}
	return nil, lastBiz
}

// pickCreditField 从签到 claim 响应中 best-effort 提取入账数额。
// 已知响应只保证 code/message；官方客户端展示的 +N 积分若在响应里，
// 会落在 credits / add_credits / reward_credits / credited 等字段（顶层或 data 包一层）。
func pickCreditField(data []byte) *int64 {
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	candidates := []string{"credits", "add_credits", "reward_credits", "credited", "award_credits", "obtain_credits"}
	for _, prefix := range []string{"", "data"} {
		var m map[string]any
		if prefix == "" {
			m = probe
		} else {
			inner, ok := probe[prefix].(map[string]any)
			if !ok {
				continue
			}
			m = inner
		}
		for _, k := range candidates {
			if v, ok := m[k].(float64); ok {
				n := int64(v)
				return &n
			}
		}
	}
	return nil
}

// EntitlementPack represents one entry in user_entitlement_pack_list.
// 对齐 cockpit-tools src/types/trae.ts 的用量模型：quota 字段位于
// entitlement_base_info.quota 或 product_extra.{subscription_extra,package_extra}.quota
// 三层中的任意一层（getPackQuota 的多路径探测）；usage 位于 pack.usage。
// v0.12.34 注：quota 里还有 credits_limit（SOLO 积分计费额度，官方 cashier
// 与 traework2api 以它为准；cockpit-tools 上游不读，因其面向订阅制）。
type EntitlementPack struct {
	EntitlementBaseInfo struct {
		ProductType  int       `json:"product_type"` // 0=Free, 1=Pro, 4=Pro+, 5=Pro+CN, 6=Ultra, 8=Lite, 9=Trial, 100=CNExpress
		EndTime      int64     `json:"end_time"`
		IsHide       bool      `json:"is_hide"`
		Status       *int      `json:"status"` // nil 或 1=active，3=cancelled
		Quota        PackQuota `json:"quota"`
		ProductExtra struct {
			SubscriptionExtra struct {
				Quota PackQuota `json:"quota"`
			} `json:"subscription_extra"`
			PackageExtra struct {
				Quota PackQuota `json:"quota"`
			} `json:"package_extra"`
		} `json:"product_extra"`
	} `json:"entitlement_base_info"`
	Usage       PackUsage `json:"usage"`
	DisplayDesc string    `json:"display_desc"` // 上游 identityStr 优先取 display_desc（CN 选中包）
}

// PackQuota 是 quota 层的全部已知数值字段。指针区分"字段缺失"与 0。
type PackQuota struct {
	BasicUsageLimit              *int64 `json:"basic_usage_limit"`
	BonusUsageLimit              *int64 `json:"bonus_usage_limit"`
	PremiumModelFastRequestLimit *int64 `json:"premium_model_fast_request_limit"` // -1=unlimited
	// v0.12.34: SOLO 积分计费的额度字段——官方 cashier 用它减 credits_amount
	// 得"剩余积分"（-1=不限）。cockpit-tools 上游不读它，但官方 Web
	// main.js 与 traework2api 都以它为准，SOLO 积分制账户必须读。
	CreditsLimit *int64 `json:"credits_limit"`
}

// PackUsage 是 pack.usage 的已知数值字段。
type PackUsage struct {
	BasicUsageAmount       *int64 `json:"basic_usage_amount"`
	BonusUsageAmount       *int64 `json:"bonus_usage_amount"`
	PremiumModelFastAmount *int64 `json:"premium_model_fast_amount"`
	IsFlashConsuming       bool   `json:"is_flash_consuming"`
	// v0.12.34: 积分池已用量（浮点，官方 Math.round 后再聚合）。
	CreditsAmount *float64 `json:"credits_amount"`
}

// EffectiveQuota 返回三层 quota 中第一层带任何已知字段的值（对齐上游
// getPackQuota: entitlement_base_info.quota ?? subscription_extra.quota ?? package_extra.quota）。
func (p *EntitlementPack) EffectiveQuota() PackQuota {
	base := p.EntitlementBaseInfo.Quota
	if base.hasAny() {
		return base
	}
	sub := p.EntitlementBaseInfo.ProductExtra.SubscriptionExtra.Quota
	if sub.hasAny() {
		return sub
	}
	return p.EntitlementBaseInfo.ProductExtra.PackageExtra.Quota
}

func (q PackQuota) hasAny() bool {
	return q.BasicUsageLimit != nil || q.BonusUsageLimit != nil || q.PremiumModelFastRequestLimit != nil || q.CreditsLimit != nil
}

// PackRemain 返回该 pack 的剩余额度（basic_quota - basic_usage，考虑 bonus）。
// quota 缺失时 ok=false（"剩余未知"——上游对 Free/未知包显示 "--"，不猜测 0）。
func (p *EntitlementPack) PackRemain() (remain int64, ok bool) {
	q := p.EffectiveQuota()
	if q.BasicUsageLimit == nil {
		return 0, false
	}
	used := int64(0)
	if p.Usage.BasicUsageAmount != nil {
		used = *p.Usage.BasicUsageAmount
	}
	left := *q.BasicUsageLimit - used
	// bonus 仅对可见 pack 计入（上游 isPackExhausted 的 bonus 语义）。
	if q.BonusUsageLimit != nil {
		bonusUsed := int64(0)
		if p.Usage.BonusUsageAmount != nil {
			bonusUsed = *p.Usage.BonusUsageAmount
		}
		if bonusLeft := *q.BonusUsageLimit - bonusUsed; bonusLeft > 0 {
			left += bonusLeft
		}
	}
	if left < 0 {
		left = 0
	}
	return left, true
}

// EntUsageResult is the parsed v2 credit API response.
type EntUsageResult struct {
	IsCreditsBilling        bool              `json:"is_credits_billing"`
	UserEntitlementPackList []EntitlementPack `json:"user_entitlement_pack_list"`
}

// UserEntUsage 聚合积分 + 识别当前生效 pack。
// 对齐 cockpit-tools trae_account_token_injection.rs apply_usage_response：
//  1. 过滤废弃 pack (product_type == 3 PROMO_CODE)
//  2. 过滤隐藏/已取消 pack (is_hide || status==3)
//  3. 按 CN/Intl 优先级选最高 pack（100→6→5/4→1/9→8→0）
//  4. 选中的 pack 的 credits_limit 作为 remain
//  5. fastRequestLimit/fastRequestUsed 来自选中 pack
func (c *Client) UserEntUsage(a *auth.Auth) (*EntUsageResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp EntUsageResult
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("ent usage parse: %w", err)
	}
	return &resp, nil
}

// FastRequestUsage 对齐 cockpit-tools src/types/trae.ts getFastRequestUsage：
// 对可见 active pack 求 premium_model_fast_request_limit 总和与
// premium_model_fast_amount 总和；available = limit - used（limit 含 -1 → 全局无限）。
// hasEvidence=false 表示 pack 里完全没有 fast-request 字段（不能当作 0 次）。
// dashboardPayload=true 时（user_current_entitlement_list 源）即使全 0 也算有证据。
func FastRequestUsage(packs []EntitlementPack, dashboardPayload bool) (available, limit, used int64, hasEvidence bool) {
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.EntitlementBaseInfo.ProductType == 3 {
			continue
		}
		if p.EntitlementBaseInfo.IsHide {
			continue
		}
		if p.EntitlementBaseInfo.Status != nil && *p.EntitlementBaseInfo.Status == 3 {
			continue
		}
		filtered = append(filtered, p)
	}
	if len(filtered) == 0 {
		return 0, 0, 0, false
	}
	hasEvidence = dashboardPayload
	unlimited := false
	for _, p := range filtered {
		q := p.EffectiveQuota()
		if q.PremiumModelFastRequestLimit != nil {
			hasEvidence = true
			if *q.PremiumModelFastRequestLimit == -1 {
				unlimited = true
			} else {
				limit += *q.PremiumModelFastRequestLimit
			}
		}
		if p.Usage.PremiumModelFastAmount != nil {
			hasEvidence = true
			used += *p.Usage.PremiumModelFastAmount
		}
	}
	if !hasEvidence {
		return 0, 0, 0, false
	}
	if unlimited {
		return -1, -1, used, true
	}
	available = limit - used
	if available < 0 {
		available = 0
	}
	return available, limit, used, true
}

// ---- v0.12.34: 官方 cashier 同口径积分池（SOLO 积分制真实余额）----

// CreditsPoolInfo 汇总 ide_user_ent_usage 的积分池口径余额。
// 官方 Web main.js cashier：Σ max(credits_limit - credits_amount, 0)，
// 任一 credits_limit==-1 → 整体不限（-1）；credits_limit 缺失按 0 计。
// is_credits_billing=true 即积分计费账户（官方以此决定渲染积分池）。
// 注意与 checkin_credits/status 的 credits（签到钱包）是两笔钱。
type CreditsPoolInfo struct {
	Remain    int64
	Known     bool
	Unlimited bool
}

// CreditsPoolUsage 按官方公式聚合积分池。可见性过滤与 FastRequestUsage
// 一致（去 PROMO_CODE/隐藏/已取消）。isCreditsBilling=true 时即使 pack
// 未携带 credits_limit 也视为已知（官方此时按 0 展示）。
func CreditsPoolUsage(packs []EntitlementPack, isCreditsBilling bool) CreditsPoolInfo {
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.EntitlementBaseInfo.ProductType == 3 {
			continue
		}
		if p.EntitlementBaseInfo.IsHide {
			continue
		}
		if p.EntitlementBaseInfo.Status != nil && *p.EntitlementBaseInfo.Status == 3 {
			continue
		}
		filtered = append(filtered, p)
	}
	hasField := false
	for _, p := range filtered {
		if p.EffectiveQuota().CreditsLimit != nil {
			hasField = true
			break
		}
	}
	if !isCreditsBilling && !hasField {
		return CreditsPoolInfo{}
	}
	if len(filtered) == 0 {
		return CreditsPoolInfo{Known: isCreditsBilling}
	}
	unlimited := false
	var total int64
	for _, p := range filtered {
		q := p.EffectiveQuota()
		if q.CreditsLimit == nil {
			continue // 官方 `?? 0`：无字段贡献 0
		}
		if *q.CreditsLimit == -1 {
			unlimited = true
			continue
		}
		used := float64(0)
		if p.Usage.CreditsAmount != nil {
			used = *p.Usage.CreditsAmount
		}
		left := float64(*q.CreditsLimit) - used
		if left < 0 {
			left = 0
		}
		total += int64(left + 0.5) // 官方 Math.round
	}
	if unlimited {
		return CreditsPoolInfo{Remain: -1, Known: true, Unlimited: true}
	}
	return CreditsPoolInfo{Remain: total, Known: true}
}

// ---- v0.12.29: ide_user_pay_status（CN 套餐 detail/quota 数据源）----

// PayStatusResult mirrors the CN /trae/api/v1/pay/ide_user_pay_status response,
// parsed tolerantly exactly like upstream cockpit-tools
// getCnEntitlementDetailFields (trae.ts): top-level detail/quota first, then
// entitlementInfo.detail/quota, then originPayStatusData.detail/quota.
type PayStatusResult struct {
	Code int `json:"code"`
	// user_pay_identity_str — upstream stores this as account.plan_type
	// (apply_entitlement_response); "free"/"Free" 等，用于 Free 判定。
	UserPayIdentityStr string `json:"user_pay_identity_str"`

	Detail          map[string]json.RawMessage `json:"detail"`
	Quota           map[string]json.RawMessage `json:"quota"`
	EntitlementInfo *PayStatusNested           `json:"entitlementInfo"`
	OriginPayStatus *PayStatusOrigin           `json:"originPayStatusData"`
}

// PayStatusNested is the entitlementInfo.{detail,quota} fallback shape.
type PayStatusNested struct {
	Detail map[string]json.RawMessage `json:"detail"`
	Quota  map[string]json.RawMessage `json:"quota"`
}

// PayStatusOrigin is the originPayStatusData.detail fallback shape.
type PayStatusOrigin struct {
	Detail map[string]json.RawMessage `json:"detail"`
}

// pickInt probes the detail/quota maps (with upstream's fallback order) for
// the first key that parses as a number. Returns nil when absent everywhere.
func (p *PayStatusResult) pickInt(quota bool, keys ...string) *int64 {
	maps := make([]map[string]json.RawMessage, 0, 3)
	if quota {
		if p.Quota != nil {
			maps = append(maps, p.Quota)
		}
		if p.EntitlementInfo != nil && p.EntitlementInfo.Quota != nil {
			maps = append(maps, p.EntitlementInfo.Quota)
		}
	} else {
		if p.Detail != nil {
			maps = append(maps, p.Detail)
		}
		if p.EntitlementInfo != nil && p.EntitlementInfo.Detail != nil {
			maps = append(maps, p.EntitlementInfo.Detail)
		}
		if p.OriginPayStatus != nil && p.OriginPayStatus.Detail != nil {
			maps = append(maps, p.OriginPayStatus.Detail)
		}
	}
	for _, m := range maps {
		for _, k := range keys {
			if raw, ok := m[k]; ok {
				var v int64
				if err := json.Unmarshal(raw, &v); err == nil {
					out := v
					return &out
				}
			}
		}
	}
	return nil
}

// FastRequestPer — detail.fast_request_per / fastRequestPer（快请求/月）。
func (p *PayStatusResult) FastRequestPer() *int64 {
	return p.pickInt(false, "fast_request_per", "fastRequestPer")
}

// CanGetExpressStatus — detail.can_get_express_status / canGetExpressStatus.
func (p *PayStatusResult) CanGetExpressStatus() *int64 {
	return p.pickInt(false, "can_get_express_status", "canGetExpressStatus")
}

// SoloParallelLimit — quota.solo_agent_parallel_limit（SOLO 并发数）。
func (p *PayStatusResult) SoloParallelLimit() *int64 {
	return p.pickInt(true, "solo_agent_parallel_limit")
}

// HasSoloPackage — quota.enable_solo_* 任一为 true（上游 hasSoloPackage）。
func (p *PayStatusResult) HasSoloPackage() bool {
	if p.Quota == nil && (p.EntitlementInfo == nil || p.EntitlementInfo.Quota == nil) {
		return false
	}
	maps := []map[string]json.RawMessage{}
	if p.Quota != nil {
		maps = append(maps, p.Quota)
	}
	if p.EntitlementInfo != nil && p.EntitlementInfo.Quota != nil {
		maps = append(maps, p.EntitlementInfo.Quota)
	}
	for _, key := range []string{"enable_solo_agent", "enable_solo_builder", "enable_solo_coder", "enable_solo_lite", "enable_solo_web"} {
		for _, m := range maps {
			if raw, ok := m[key]; ok {
				var v bool
				if err := json.Unmarshal(raw, &v); err == nil && v {
					return true
				}
			}
		}
	}
	return false
}

// PlanIdentity returns the normalized user_pay_identity_str ("" when absent).
func (p *PayStatusResult) PlanIdentity() string {
	return strings.TrimSpace(p.UserPayIdentityStr)
}

// PayStatus fetches the CN pay/entitlement detail payload (best-effort data
// source for Free/SOLO accounts whose ent_usage packs carry no quota).
func (c *Client) PayStatus(a *auth.Auth) (*PayStatusResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpPayStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp PayStatusResult
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("pay status parse: %w", err)
	}
	return &resp, nil
}

// UsageSummary 汇总一次 UserEntUsage 的展示/评分所需数值。
// 展示语义对齐 cockpit-tools TraeAccountsPage.tsx 的 CN 规则：
//   - 速通证据存在 → UsageModel="fast"，Remain=fast 可用次数（-1=无限）
//   - 选中包 quota 可解析 → UsageModel="basic"，Remain=quota-usage（含 bonus）
//   - 两者都无 → UsageModel="unknown"，RemainKnown=false（面板显示 "--"，绝不猜测 0）
type UsageSummary struct {
	UsageModel  string // "fast" | "basic" | "unknown"
	Remain      int64  // fast: 可用次数(-1 无限)；basic: 套餐剩余
	RemainKnown bool
	FastLimit   int64
	FastUsed    int64
	Used        int64 // basic 模型的已用量（展示"已用"）
	Total       int64 // basic 模型的额度池（展示"额度池"）

	// v0.12.29: ide_user_pay_status 补充维度（management.go 填充；缓存随
	// UsageSummary 一并落到 /accounts，Free/SOLO 账户不再只剩 "--"）。
	FastRequestPer *int64 // detail.fast_request_per（快请求/月）
	SoloParallel   *int64 // quota.solo_agent_parallel_limit（SOLO 并发）
	SoloPackage    bool   // quota.enable_solo_* 任一为 true
	PlanType       string // user_pay_identity_str（"free" 等，Free 判定用）

	// v0.12.34: 官方口径积分池（handleCreditsQuery 填充，缓存随行）。
	CreditsPool CreditsPoolInfo
}

// IsFreePlan 对齐上游 isFreePlan，但补上中文 display_desc（"免费"）——
// 上游 .includes('free') 对 CN 显示名"免费"会漏判，这里同时匹配两者。
func IsFreePlan(plan, planType string) bool {
	p := strings.ToLower(strings.TrimSpace(plan))
	t := strings.ToLower(strings.TrimSpace(planType))
	return strings.Contains(p, "free") || strings.Contains(p, "免费") ||
		strings.Contains(t, "free") || strings.Contains(t, "免费")
}

// SummarizeUsage computes the UsageSummary for a pack list.
func SummarizeUsage(packs []EntitlementPack, isCN bool) UsageSummary {
	fastAvail, fastLimit, fastUsed, hasFast := FastRequestUsage(packs, false)
	if isCN && hasFast {
		return UsageSummary{
			UsageModel:  "fast",
			Remain:      fastAvail,
			RemainKnown: true,
			FastLimit:   fastLimit,
			FastUsed:    fastUsed,
		}
	}
	selected := SelectActivePack(packs, isCN)
	if selected != nil {
		if remain, ok := selected.PackRemain(); ok {
			q := selected.EffectiveQuota()
			var used, total int64
			if q.BasicUsageLimit != nil {
				total = *q.BasicUsageLimit
			}
			if selected.Usage.BasicUsageAmount != nil {
				used = *selected.Usage.BasicUsageAmount
			}
			return UsageSummary{
				UsageModel:  "basic",
				Remain:      remain,
				RemainKnown: true,
				Used:        used,
				Total:       total,
			}
		}
	}
	if !isCN && hasFast {
		// Intl 无速通展示语义，但保留数值兜底（不丢弃证据）。
		return UsageSummary{UsageModel: "fast", Remain: fastAvail, RemainKnown: true, FastLimit: fastLimit, FastUsed: fastUsed}
	}
	return UsageSummary{UsageModel: "unknown"}
}

// PackListRemain returns the pool-scoring remain (0 when unknown) plus whether
// the remain was actually known. Score semantics: fast -1 (unlimited) scores
// as a large constant so unlimited accounts are picked first.
func PackListRemain(packs []EntitlementPack, isCN bool) (score int64, known bool) {
	sum := SummarizeUsage(packs, isCN)
	if !sum.RemainKnown {
		return 0, false
	}
	if sum.Remain < 0 { // unlimited fast requests
		return 1 << 30, true
	}
	return sum.Remain, true
}

// SelectActivePack applies cockpit-tools apply_usage_response logic:
//   - Filter out product_type==3 (PROMO_CODE)
//   - Filter out is_hide==true || status==3 (cancelled)
//   - Pick highest-priority pack by CN or Intl order
//
// Returns the selected pack, or nil if none.
//
// CN priority:    100 (CNExpress) > 6 (Ultra) > 5 (Pro+ Pack CN) > 4 (Pro+) > 1 (Pro) > 9 (Trial) > 8 (Lite) > 0 (Free)
// Intl priority:  6 (Ultra) > 4 (Pro+) > 1 (Pro) > 9 (Trial) > 8 (Lite) > 0 (Free)
func SelectActivePack(packs []EntitlementPack, isCN bool) *EntitlementPack {
	// Step 1+2: filter out PROMO_CODE and hidden/cancelled packs.
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.EntitlementBaseInfo.ProductType == 3 { // PROMO_CODE
			continue
		}
		if p.EntitlementBaseInfo.IsHide {
			continue
		}
		if p.EntitlementBaseInfo.Status != nil && *p.EntitlementBaseInfo.Status == 3 { // cancelled
			continue
		}
		filtered = append(filtered, p)
	}
	if len(filtered) == 0 {
		return nil
	}
	// Step 3: pick by priority order.
	priorityCN := []int{100, 6, 5, 4, 1, 9, 8, 0}
	priorityIntl := []int{6, 4, 1, 9, 8, 0}
	order := priorityIntl
	if isCN {
		order = priorityCN
	}
	for _, pt := range order {
		for i := range filtered {
			if filtered[i].EntitlementBaseInfo.ProductType == pt {
				return &filtered[i]
			}
		}
	}
	return &filtered[0] // fallback: first active pack
}

// ProductTypeIdentity maps product_type to a human-readable plan identity.
// 对齐 cockpit-tools usage_identity_from_product_type。
func ProductTypeIdentity(productType int, isCN bool) string {
	switch productType {
	case 100:
		if isCN {
			return "CNExpress"
		}
		return "Unknown"
	case 6:
		return "Ultra"
	case 5:
		if isCN {
			return "Pro+"
		}
		return "Pro+"
	case 4:
		return "Pro+"
	case 1, 9:
		return "Pro" // Trial/SoloInvite → Pro
	case 8:
		return "Lite"
	case 0:
		return "Free"
	default:
		return "Unknown"
	}
}

// IsRateLimit9074 returns true if the business code indicates Trae rate limiting
// (code 9074 = "当前参与用户太多，请稍后再试"). The 3 reference projects (cockpit-tools,
// 9router, OmniRoute) all treat this as a generic error; cpa-multi-plugins
// exposes it explicitly so callers can apply exponential backoff.
func IsRateLimit9074(code int32) bool {
	return code == 9074
}

// GetUserInfo 查询账号信息（登录用）。多源 fallback：依次尝试账号
// apiHost → OAuthHost → 备用源（对齐 cockpit-tools build_api_urls）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	var raw json.RawMessage
	uid, nickname, enterpriseID, raw, err = c.GetUserInfoFull(a)
	_ = raw
	return uid, nickname, enterpriseID, err
}

// GetUserInfoFull v0.12.44: GetUserInfo plus the raw response body. The
// Result carries the rich profile cockpit-tools keeps as trae_profile_raw
// (AvatarUrl / Region / AIRegion / TenantID / NonPlainTextMobile /
// RegisterTime / UtmInfo — live-verified against a cockpit-tools export
// 2026-09-06), which the login path now persists for credential parity
// instead of discarding all but three fields.
func (c *Client) GetUserInfoFull(a *auth.Auth) (uid, nickname, enterpriseID string, raw json.RawMessage, err error) {
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	rawBody, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range exchangeHosts(a.APIHost, c.OAuthHost) {
		req, rerr := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(rawBody))
		if rerr != nil {
			return "", "", "", nil, rerr
		}
		OAuthHeaders(req)
		req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return "", "", "", nil, lastErr
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", nil, fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, data, nil
}

// CheckLoginResult is the flattened view of the CheckLogin response
// (v0.12.44). Raw keeps the full envelope for diagnostics; the response shape
// below is live-verified against the official client's stored trae_server_raw
// (cockpit-tools import) — Result{IsLogin, BoundDeviceID, DeviceBindStatus,
// Host, ExpiredAt, Region, AIRegion, AIHost, UserID, MigrateToSG}.
type CheckLoginResult struct {
	IsLogin          bool
	BoundDeviceID    string
	DeviceBindStatus string
	Host             string
	Region           string
	AIRegion         string
	UserID           string
	ExpiredAt        int64 // ms epoch, 0 = absent
	Raw              json.RawMessage
}

// CheckLogin probes the account's login state and SERVER-BOUND device
// (POST /cloudide/api/v3/trae/CheckLogin, cockpit-tools
// trae_account_core_refresh.rs:1035-1045 — same cloudide family and auth
// scheme as GetUserInfo). The bound-device echo is the ground truth for the
// device-binding health of a credential: a checkin that presents a deviceId
// other than BoundDeviceID is a plausible risk-control trigger (9074), so
// the management layer logs a warning on mismatch.
func (c *Client) CheckLogin(a *auth.Auth) (*CheckLoginResult, error) {
	body := map[string]any{"IDEVersion": IdeVersion}
	rawBody, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range exchangeHosts(a.APIHost, c.OAuthHost) {
		req, rerr := http.NewRequest(http.MethodPost, host+EpCheckLogin, bytes.NewReader(rawBody))
		if rerr != nil {
			return nil, rerr
		}
		OAuthHeaders(req)
		req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	var resp struct {
		Result struct {
			IsLogin          bool   `json:"IsLogin"`
			BoundDeviceID    string `json:"BoundDeviceID"`
			DeviceBindStatus string `json:"DeviceBindStatus"`
			Host             string `json:"Host"`
			Region           string `json:"Region"`
			AIRegion         string `json:"AIRegion"`
			UserID           string `json:"UserID"`
			ExpiredAt        any    `json:"ExpiredAt"`
		} `json:"Result"`
		// Top-level echoes (live export: Result.AIRegion is empty while the
		// response root carries AIRegion/loginRegion/storeRegion — cockpit
		// merges the routing context the same way).
		TopAIRegion string `json:"AIRegion"`
		TopRegion   string `json:"storeRegion"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("checklogin parse: %w", err)
	}
	if resp.Result.AIRegion == "" {
		resp.Result.AIRegion = resp.TopAIRegion
	}
	if resp.Result.Region == "" {
		resp.Result.Region = resp.TopRegion
	}
	out := &CheckLoginResult{
		IsLogin:          resp.Result.IsLogin,
		BoundDeviceID:    resp.Result.BoundDeviceID,
		DeviceBindStatus: resp.Result.DeviceBindStatus,
		Host:             resp.Result.Host,
		Region:           resp.Result.Region,
		AIRegion:         resp.Result.AIRegion,
		UserID:           resp.Result.UserID,
		Raw:              data,
	}
	if n, ok := numericAnyToI64(resp.Result.ExpiredAt); ok && n > 0 {
		out.ExpiredAt = n
	}
	return out, nil
}

// numericAnyToI64 coerces a decoded JSON number (float64 via encoding/json,
// json.Number, or int64) to int64. Package-local counterpart of main's
// toInt64 (v0.12.44 CheckLogin ExpiredAt parsing).
func numericAnyToI64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
