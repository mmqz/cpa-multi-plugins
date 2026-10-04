// intl_pay.go — Trae Intl 计费面（issue #29 评论实测证据，Xyloz3n 2026-10-03）。
//
// intl 账号的额度/支付端点不在 CN 的 api.trae.cn + v2 上（打过去
// 401 code 4014，面板表现为 "ent_usage: upstream session_dead"），而是：
//
//	https://grow-normal.trae.ai + /trae/api/v1/pay/*
//
// 此前 refreshAccountCredits 对 intl 账号也走 CN 客户端（v0.12.2 起 intl
// 由统一插件接管，但 credits 查询从未分流）。实测可用的请求形状：
//
//	POST，body {}
//	Authorization: Cloud-IDE-JWT <accessToken>
//	X-User-Region: <auth.region>        # 实测任意值都过（CN 值也过）
//	X-Device-Id: <auth.boundDeviceId>
//	User-Agent: Trae/3.5.66
//	Content-Type: application/json
//
// 注意 intl v1 回包的 usage 数值是浮点（basic_usage_amount: 0.2147 —
// is_flash_consuming 按次数分数扣减），与 CN v2 的 int64 不同；先解码为
// float，换算进 CN 口径（upstream.EntUsageResult）时四舍五入，后续
// SummarizeUsage / 面板投影全部复用 CN 管线。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// intlPayBase is the field-verified intl billing face host. Var (not const)
// so tests can point it at an httptest server.
var intlPayBase = "https://grow-normal.trae.ai"

// intlPayUserAgent is the UA the intl pay face accepts (field-verified;
// matches the official client family).
const intlPayUserAgent = "Trae/3.5.66"

// intlPayHTTP is the pooled client for the intl pay face (small, credits
// queries are rare — panel refresh / scheduler cadence).
var intlPayHTTP = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// Endpoint paths on the intl pay face (v1 — NOT the CN v2 family).
const (
	intlEpEntUsage  = "/trae/api/v1/pay/ide_user_ent_usage"
	intlEpPayStatus = "/trae/api/v1/pay/ide_user_pay_status"
)

// intlEntUsageResult mirrors the intl v1 ent_usage payload. Quota/usage
// numeric fields are floats here (see file comment).
type intlEntUsageResult struct {
	IsCreditsBilling        bool                  `json:"is_credits_billing"`
	UserEntitlementPackList []intlEntitlementPack `json:"user_entitlement_pack_list"`
}

type intlEntitlementPack struct {
	EntitlementBaseInfo struct {
		ProductType  int           `json:"product_type"`
		EndTime      int64         `json:"end_time"`
		IsHide       bool          `json:"is_hide"`
		Status       *int          `json:"status"`
		Quota        intlPackQuota `json:"quota"`
		ProductExtra struct {
			SubscriptionExtra struct {
				Quota intlPackQuota `json:"quota"`
			} `json:"subscription_extra"`
			PackageExtra struct {
				Quota intlPackQuota `json:"quota"`
			} `json:"package_extra"`
		} `json:"product_extra"`
	} `json:"entitlement_base_info"`
	Usage       intlPackUsage `json:"usage"`
	DisplayDesc string        `json:"display_desc"`
}

type intlPackQuota struct {
	// BasicUsageLimit 在 intl Free 包上缺省；实际计量维度是
	// AdvancedModelRequestLimit（进阶模型请求次数）。两者都收，换算时
	// basic 缺失回退 advanced。
	BasicUsageLimit              *float64 `json:"basic_usage_limit"`
	BonusUsageLimit              *float64 `json:"bonus_usage_limit"`
	AdvancedModelRequestLimit    *float64 `json:"advanced_model_request_limit"`
	PremiumModelFastRequestLimit *float64 `json:"premium_model_fast_request_limit"`
	CreditsLimit                 *float64 `json:"credits_limit"`
}

type intlPackUsage struct {
	BasicUsageAmount       *float64 `json:"basic_usage_amount"`
	BonusUsageAmount       *float64 `json:"bonus_usage_amount"`
	PremiumModelFastAmount *float64 `json:"premium_model_fast_amount"`
	IsFlashConsuming       bool     `json:"is_flash_consuming"`
	CreditsAmount          *float64 `json:"credits_amount"`
}

// intlPayPost issues one authenticated POST against the intl pay face and
// returns the payload JSON. Both flat payloads and the {"code":..,"data":..}
// envelope are tolerated (field reports show the pack list reachable either
// way across gateway versions).
func intlPayPost(path, accessToken, region, deviceID string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, intlPayBase+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", intlPayUserAgent)
	if region != "" {
		req.Header.Set("X-User-Region", region)
	}
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+accessToken)

	resp, err := intlPayHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("intl pay %s: HTTP %d: %s", path, resp.StatusCode, truncateRedactedHead(string(raw), 160))
	}
	return raw, nil
}

// intlEntUsage fetches the intl entitlement/usage snapshot.
func intlEntUsage(accessToken, region, deviceID string) (*intlEntUsageResult, error) {
	data, err := intlPayPost(intlEpEntUsage, accessToken, region, deviceID)
	if err != nil {
		return nil, err
	}
	return intlEntUsageDecode(data)
}

// intlEntUsageDecode parses an ent_usage payload, tolerating both the flat
// pack-list shape and the {"code":..,"data":..} envelope.
func intlEntUsageDecode(data []byte) (*intlEntUsageResult, error) {
	raw := data
	var probe struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.Data != nil && len(probe.Data) > 0 && probe.Data[0] == '{' {
		if probe.Code != nil && *probe.Code != 0 {
			return nil, fmt.Errorf("intl ent_usage: business code %d: %s", *probe.Code, truncateRedactedHead(string(data), 160))
		}
		raw = probe.Data
	}
	var out intlEntUsageResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("intl ent_usage parse: %w", err)
	}
	return &out, nil
}

// intlPayStatusRaw fetches the intl pay_status payload (best-effort caller).
func intlPayStatusRaw(accessToken, region, deviceID string) (json.RawMessage, error) {
	return intlPayPost(intlEpPayStatus, accessToken, region, deviceID)
}

// toCN converts the intl pack into the CN-shaped EntitlementPack so the
// existing SummarizeUsage / CreditsPoolUsage / SelectActivePack pipeline and
// panel projections work unchanged. Floats round half-away-from-zero; the
// basic计量维度回退 advanced_model_request_limit（intl Free 包口径）。
func (p *intlEntitlementPack) toCN() upstream.EntitlementPack {
	out := upstream.EntitlementPack{
		DisplayDesc: p.DisplayDesc,
	}
	out.EntitlementBaseInfo.ProductType = p.EntitlementBaseInfo.ProductType
	out.EntitlementBaseInfo.EndTime = p.EntitlementBaseInfo.EndTime
	out.EntitlementBaseInfo.IsHide = p.EntitlementBaseInfo.IsHide
	out.EntitlementBaseInfo.Status = p.EntitlementBaseInfo.Status
	out.EntitlementBaseInfo.Quota = p.EntitlementBaseInfo.Quota.toCN()
	out.EntitlementBaseInfo.ProductExtra.SubscriptionExtra.Quota = p.EntitlementBaseInfo.ProductExtra.SubscriptionExtra.Quota.toCN()
	out.EntitlementBaseInfo.ProductExtra.PackageExtra.Quota = p.EntitlementBaseInfo.ProductExtra.PackageExtra.Quota.toCN()
	out.Usage = p.Usage.toCN()
	return out
}

func (q intlPackQuota) toCN() upstream.PackQuota {
	basic := q.BasicUsageLimit
	if basic == nil {
		basic = q.AdvancedModelRequestLimit // intl Free 包：进阶请求次数即计量维度
	}
	return upstream.PackQuota{
		BasicUsageLimit:              floatToInt64Ptr(basic),
		BonusUsageLimit:              floatToInt64Ptr(q.BonusUsageLimit),
		PremiumModelFastRequestLimit: floatToInt64Ptr(q.PremiumModelFastRequestLimit),
		CreditsLimit:                 floatToInt64Ptr(q.CreditsLimit),
	}
}

func (u intlPackUsage) toCN() upstream.PackUsage {
	return upstream.PackUsage{
		BasicUsageAmount:       floatToInt64Ptr(u.BasicUsageAmount),
		BonusUsageAmount:       floatToInt64Ptr(u.BonusUsageAmount),
		PremiumModelFastAmount: floatToInt64Ptr(u.PremiumModelFastAmount),
		IsFlashConsuming:       u.IsFlashConsuming,
		CreditsAmount:          u.CreditsAmount,
	}
}

func floatToInt64Ptr(v *float64) *int64 {
	if v == nil {
		return nil
	}
	r := int64(math.Round(*v))
	return &r
}

// toCNResult converts the whole intl snapshot.
func (r *intlEntUsageResult) toCNResult() *upstream.EntUsageResult {
	out := &upstream.EntUsageResult{IsCreditsBilling: r.IsCreditsBilling}
	for i := range r.UserEntitlementPackList {
		out.UserEntitlementPackList = append(out.UserEntitlementPackList, r.UserEntitlementPackList[i].toCN())
	}
	return out
}

// truncateRedactedHead keeps an error body head for logs (defense-in-depth:
// never echo a full token-bearing body).
func truncateRedactedHead(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
