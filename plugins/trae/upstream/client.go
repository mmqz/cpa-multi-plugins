// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
        "bytes"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
        ErrNone        ErrKind = iota // 成功
        ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
        ErrSoftRate                   // 429 → 短冷却 60s
        ErrSessionDead                // 401 + Cloud-IDE-JWT 失效 → 禁用
        ErrNotFound                   // 404 → 短冷却 60s 不累计 errCount
        ErrServer                     // 5xx
        ErrClient                     // 其他 4xx
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

// bizError 构造业务码错误（对齐 cockpit-tools 签到接口 code!=0 的报错语义：
// "获取签到状态失败 (code=N): Token 已过期，请重新登录"）。9074 归类软限流，
// 其余非零码按会话失效处理（pool 据此禁用，与上游提示一致）。
func bizError(code int32, prefix string) *Error {
        msg := fmt.Sprintf("%s (code=%d): %s", prefix, code, func() string {
                if code == 9074 {
                        return "当前参与用户太多，请稍后再试"
                }
                return "Token 已过期，请重新登录"
        }())
        kind := ErrSessionDead
        if code == 9074 {
                kind = ErrSoftRate
        }
        return &Error{Kind: kind, Msg: msg, BizCode: code}
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
        lower := strings.ToLower(body)
        // 1005 plan 权益不足
        if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
                return ErrPlanLimit
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
        if !a.NeedsRefreshLocked(skew) {
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
        req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body, a.Variant)))
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
        ContextWindow int64 // = maxInputTokens
        MaxTokens     int64 // = maxOutputTokens
}

// FetchModels 拉 SOLO 模型表（get_detail_param，32 配置）。
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
                        ConfigName    string `json:"config_name"`
                        DisplayConfig struct {
                                DisplayName string `json:"display_name"`
                        } `json:"display_config"`
                        ModelDetailList []struct {
                                ModelName string `json:"model_name"`
                        } `json:"model_detail_list"`
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
                out = append(out, ModelInfo{
                        ID:   cfg.ConfigName,
                        Name: cfg.DisplayConfig.DisplayName,
                })
        }
        if len(out) == 0 {
                return nil, fmt.Errorf("models api returned empty list")
        }
        return out, nil
}

// CheckinStatus 查询签到状态。
// 对齐 cockpit-tools trae_account_token_injection.rs:2761 用 GET + did query param
// （cockpit-tools 用 GET；traework2api 原版用 POST body={}，两种都被接受）。
// 鉴权用 Bearer 方案（UgCheckinHeaders，对齐上游签到实现；v0.12.35 起）。
// 返回完整字段：CheckedIn / Credits / Enable + 业务码 Code（用于 9074 限流识别）。
// 对齐上游 code!=0 语义（trae_account_token_injection.rs:2786-2791）：
// 非零业务码 → 返回错误（Token 过期/限流等），绝不能当成 "未签到" 静默通过。
// 9074（参与用户太多）作为 *Error{BizCode:9074} 返回，调用方可识别重试。
type CheckinStatusResult struct {
        CheckedIn bool   `json:"checked_in"`
        Credits   int64  `json:"credits"`
        Enable    bool   `json:"enable"`
        Code      int32  `json:"code"` // 业务码：0=成功，9074=限流，其他=token 失效
        Message   string `json:"message"`
}

func (c *Client) CheckinStatus(a *auth.Auth) (*CheckinStatusResult, error) {
        url := c.ugBase() + EpCheckinStatus
        if a.DeviceID != "" {
                url += "?did=" + a.DeviceID
        }
        req, err := http.NewRequest(http.MethodGet, url, nil)
        if err != nil {
                return nil, err
        }
        UgCheckinHeaders(req, a)
        data, err := c.doJSON(req)
        if err != nil {
                return nil, err
        }
        var resp CheckinStatusResult
        if err := json.Unmarshal(data, &resp); err != nil {
                return nil, fmt.Errorf("checkin status parse: %w", err)
        }
        if resp.Code != 0 {
                return nil, bizError(resp.Code, "获取签到状态失败")
        }
        return &resp, nil
}

// CheckinClaim 执行签到。返回业务码 Code 用于 9074 限流识别。
// 对齐上游 claim_trae_checkin（trae_account_token_injection.rs:2884-2889）：
// code!=0 → 错误（领取成功后上游还会重新查一次状态，这里由调用方负责）。
// v0.12.32: 官方客户端 claim = POST {} + Cloud-IDE-JWT + x-device-id(真实绑定 did)
// （BlueChonk/trae-credential-reverse-engineering FINDINGS §五 实测到账，
// trae-mate/traework2api/trae-work-checkin 同构）。响应中的 credits/add_credits/
// reward 等数值字段作为"入账证据"带回（ClaimCredits），便于诊断"code=0 但未到账"。
// v0.12.35: 鉴权改用 Bearer 方案（UgCheckinHeaders）。cockpit-tools 实测可用实现
// （claim_trae_checkin, rs:2859）对签到端点恒用 Bearer；Cloud-IDE-JWT 方案下
// status 读接口放行、claim 写接口被拒为 biz_code=9074（"当前参与用户太多"）——
// 这是此前"每次签到都 9074"的根因，并非真实限流。
type CheckinClaimResult struct {
        Code    int32  `json:"code"`
        Message string `json:"message"`
        // ClaimCredits 服务端响应里携带的积分入账数额（best-effort 提取，nil=响应未携带）。
        ClaimCredits *int64 `json:"-"`
}

func (c *Client) CheckinClaim(a *auth.Auth) (*CheckinClaimResult, error) {
        req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
        if err != nil {
                return nil, err
        }
        UgCheckinHeaders(req, a)
        data, err := c.doJSON(req)
        if err != nil {
                return nil, err
        }
        var resp CheckinClaimResult
        if err := json.Unmarshal(data, &resp); err != nil {
                return nil, fmt.Errorf("checkin claim parse: %w", err)
        }
        if resp.Code != 0 {
                return nil, bizError(resp.Code, "签到领取失败")
        }
        resp.ClaimCredits = pickCreditField(data)
        return &resp, nil
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
        CreditsLimit                 *int64 `json:"credits_limit"`
}

// PackUsage 是 pack.usage 的已知数值字段。
type PackUsage struct {
        BasicUsageAmount       *int64 `json:"basic_usage_amount"`
        BonusUsageAmount       *int64 `json:"bonus_usage_amount"`
        PremiumModelFastAmount *int64 `json:"premium_model_fast_amount"`
        IsFlashConsuming       bool   `json:"is_flash_consuming"`
        // v0.12.34: 积分池已用量（浮点，官方 Math.round 后再聚合）。
        CreditsAmount          *float64 `json:"credits_amount"`
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

        Detail             map[string]json.RawMessage `json:"detail"`
        Quota              map[string]json.RawMessage `json:"quota"`
        EntitlementInfo    *PayStatusNested           `json:"entitlementInfo"`
        OriginPayStatus    *PayStatusOrigin           `json:"originPayStatusData"`
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
        body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
        raw, _ := json.Marshal(body)
        var data json.RawMessage
        var lastErr error
        for _, host := range exchangeHosts(a.APIHost, c.OAuthHost) {
                req, rerr := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
                if rerr != nil {
                        return "", "", "", rerr
                }
                OAuthHeaders(req)
                req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
                data, lastErr = c.doJSON(req)
                if lastErr == nil {
                        break
                }
        }
        if lastErr != nil {
                return "", "", "", lastErr
        }
        var resp struct {
                Result struct {
                        UserID       string `json:"UserID"`
                        ScreenName   string `json:"ScreenName"`
                        EnterpriseID string `json:"EnterpriseID"`
                } `json:"Result"`
        }
        if err := json.Unmarshal(data, &resp); err != nil {
                return "", "", "", fmt.Errorf("userinfo parse: %w", err)
        }
        return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
        s = strings.TrimSpace(s)
        if len(s) > n {
                return s[:n]
        }
        return s
}
