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

        "github.com/mmqz/cpa-multi-plugins/plugins/trae-cn/auth"
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
}

func (e *Error) Error() string {
        return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
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
func (c *Client) refreshLocked(a *auth.Auth) error {
        if strings.TrimSpace(a.RefreshToken) == "" {
                return fmt.Errorf("no refreshToken")
        }
        host := a.ApiHost
        if host == "" {
                host = c.oauthBase()
        }
        body := map[string]any{
                "ClientID":     c.ClientID,
                "RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
                "ClientSecret": "-",
                "UserID":       "",
        }
        raw, _ := json.Marshal(body)
        req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
        if err != nil {
                return err
        }
        OAuthHeaders(req)
        data, err := c.doJSON(req)
        if err != nil {
                return err
        }
        var resp struct {
                Result struct {
                        Token                string `json:"Token"`
                        TokenExpireAt        int64  `json:"TokenExpireAt"`
                        TokenExpireDuration  int64  `json:"TokenExpireDuration"`
                        RefreshToken         string `json:"RefreshToken"`
                        RefreshExpireAt      int64  `json:"RefreshExpireAt"`
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
        req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
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
                "function":            Function,
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
                        ConfigName string `json:"config_name"`
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
// 返回完整字段：CheckedIn / Credits / Enable + 业务码 Code（用于 9074 限流识别）。
type CheckinStatusResult struct {
        CheckedIn bool  `json:"checked_in"`
        Credits   int64 `json:"credits"`
        Enable    bool  `json:"enable"`
        Code      int32 `json:"code"`    // 业务码：0=成功，9074=限流，其他=token 失效
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
        UgHeaders(req, a)
        data, err := c.doJSON(req)
        if err != nil {
                return nil, err
        }
        var resp CheckinStatusResult
        if err := json.Unmarshal(data, &resp); err != nil {
                return nil, fmt.Errorf("checkin status parse: %w", err)
        }
        return &resp, nil
}

// CheckinClaim 执行签到。返回业务码 Code 用于 9074 限流识别。
type CheckinClaimResult struct {
        Code    int32  `json:"code"`
        Message string `json:"message"`
}

func (c *Client) CheckinClaim(a *auth.Auth) (*CheckinClaimResult, error) {
        req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
        if err != nil {
                return nil, err
        }
        UgHeaders(req, a)
        data, err := c.doJSON(req)
        if err != nil {
                return nil, err
        }
        var resp CheckinClaimResult
        if err := json.Unmarshal(data, &resp); err != nil {
                return nil, fmt.Errorf("checkin claim parse: %w", err)
        }
        return &resp, nil
}

// EntitlementPack represents one entry in user_entitlement_pack_list.
// 对齐 cockpit-tools trae_account_token_injection.rs apply_usage_response 的解析。
type EntitlementPack struct {
        EntitlementBaseInfo struct {
                ProductType int    `json:"product_type"` // 0=Free, 1=Pro, 4=Pro+, 5=Pro+CN, 6=Ultra, 8=Lite, 9=Trial, 100=CNExpress
                EndTime     int64  `json:"end_time"`
                IsHide      bool   `json:"is_hide"`
                Status      *int   `json:"status"` // nil 或 1=active，3=cancelled
                Quota       struct {
                        CreditsLimit                  int64 `json:"credits_limit"`
                        PremiumModelFastRequestLimit  int64 `json:"premium_model_fast_request_limit"` // -1=unlimited
                } `json:"quota"`
        } `json:"entitlement_base_info"`
        Usage struct {
                PremiumModelFastAmount int64 `json:"premium_model_fast_amount"`
        } `json:"usage"`
}

// EntUsageResult is the parsed v2 credit API response.
type EntUsageResult struct {
        IsCreditsBilling       bool             `json:"is_credits_billing"`
        UserEntitlementPackList []EntitlementPack `json:"user_entitlement_pack_list"`
}

// UserEntUsage 聚合积分 + 识别当前生效 pack。
// 对齐 cockpit-tools trae_account_token_injection.rs apply_usage_response：
//   1. 过滤废弃 pack (product_type == 3 PROMO_CODE)
//   2. 过滤隐藏/已取消 pack (is_hide || status==3)
//   3. 按 CN/Intl 优先级选最高 pack（100→6→5/4→1/9→8→0）
//   4. 选中的 pack 的 credits_limit 作为 remain
//   5. fastRequestLimit/fastRequestUsed 来自选中 pack
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

// SelectActivePack applies cockpit-tools apply_usage_response logic:
//   - Filter out product_type==3 (PROMO_CODE)
//   - Filter out is_hide==true || status==3 (cancelled)
//   - Pick highest-priority pack by CN or Intl order
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

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
        host := a.ApiHost
        if host == "" {
                host = c.oauthBase()
        }
        body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
        raw, _ := json.Marshal(body)
        req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
        if err != nil {
                return "", "", "", err
        }
        OAuthHeaders(req)
        req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
        data, err := c.doJSON(req)
        if err != nil {
                return "", "", "", err
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
