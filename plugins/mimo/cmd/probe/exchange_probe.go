// exchange_probe.go — M2 换票链诊断（与插件 exchange.go 孪生同源，wire 规格
// docs/MIMO_AUTH.md §6.2，真机端到端验证 2026-09-23）。probe 的健康判据与
// 插件 0.2.0 对齐：serviceLogin → STS 能否换出 serviceToken —— 而非已退役的
// /user/xiaomi/me（me 对有效 cookie lane 请求也 302，不能判健康，§6.2 坑 1）。
//
//	P1  GET https://account.xiaomi.com/pass/serviceLogin
//	      ?_locale=zh_CN&_snsNone=true&sid=<sid>&_json=true
//	    Cookie: passToken; userId; cUserId [; uLocale]（分区库明文账号域行）
//	    → 200 "&&&START&&&{code,ssecurity,nonce,location}&&&END&&&"
//
//	P2  GET <location>&clientSign=urlencode(base64(sha1("nonce="+nonce+"&"+ssecurity)))
//	    （刻意不发 Cookie —— 与桌面 /sts 回调同形）
//	    → 200 Set-Cookie: serviceToken / userId / <sid>_ph / <sid>_slh
//
// 隐私边界：账号域行只发给 passport（它们的原生域），P2 刻意无 Cookie；明文
// 与票据值绝不打印（只报 Set-Cookie 名单与 serviceToken 长度）；换票 URL 含
// nonce/clientSign，输出只报状态码。UA 不伪装（与插件默认一致；实测值
// MiClaw/1.0 仅在 passport 边缘开始拦 UA 时才需要，插件侧为 exchange_user_agent）。
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// probePassportBase 与插件 serviceLoginBase 同形：var 以便测试注入 httptest。
var probePassportBase = "https://account.xiaomi.com"

// errProbePassTokenExpired 标记 P1 拒绝中"重试无益"的一类：passToken 本身
// 已死，唯一的修复是桌面重新登录（与插件 errPassTokenExpired 同语义）。
var errProbePassTokenExpired = errors.New("passToken 被 passport 拒绝（请在桌面重新登录后重试）")

// exchangeTargets 是换票诊断的区域序：sgp→cn（与插件 auto 同序，sgp 为实测
// 可用集群）。ru/in 的 sid 从未观测到，插件 regionSID 显式拒绝臆测，probe
// 同立场不列（docs/MIMO_AUTH.md §6.2 区域映射）。
var exchangeTargets = []struct {
	region string
	sid    string
}{
	{"sgp", "mimosgp"},
	{"cn", "mimopc"},
}

// probeClientSign 计算 P2 凭据：urlencode(base64(sha1("nonce="+nonce+"&"+ssecurity)))。
// 已知答案向量见 main_test.go（与插件 TestClientSignVector 同源）。
func probeClientSign(nonce, ssecurity string) string {
	sum := sha1.Sum([]byte("nonce=" + nonce + "&" + ssecurity))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))
}

// probeServiceLogin 是 P1 剥壳后的载荷。
type probeServiceLogin struct {
	Code     any    `json:"code"`
	Security string `json:"ssecurity"`
	Nonce    string `json:"nonce"`
	Location string `json:"location"`
}

// parseServiceLoginBody 剥 &&&START&&&/&&&END&&& 壳并校验 P2 所需字段；code
// 非 0 归一为 errProbePassTokenExpired（数字与字符串两种 code 形态都见过）。
func parseServiceLoginBody(body []byte) (*probeServiceLogin, error) {
	s := strings.TrimSpace(string(body))
	s = strings.TrimPrefix(s, "&&&START&&&")
	s = strings.TrimSuffix(s, "&&&END&&&")
	s = strings.TrimSpace(s)
	var res probeServiceLogin
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return nil, fmt.Errorf("serviceLogin json: %w", err)
	}
	switch code := res.Code.(type) {
	case float64:
		if code != 0 {
			return nil, fmt.Errorf("%w（passport code %v）", errProbePassTokenExpired, code)
		}
	case string:
		if code != "" && code != "0" {
			return nil, fmt.Errorf("%w（passport code %s）", errProbePassTokenExpired, code)
		}
	}
	if res.Security == "" || res.Nonce == "" || res.Location == "" {
		return nil, errors.New("serviceLogin 响应缺 ssecurity/nonce/location")
	}
	return &res, nil
}

// bootstrapNames 是换票消费的账号域行（实测工作集 §6.2：四条全在场）。
var bootstrapNames = map[string]bool{
	"passToken": true,
	"userId":    true,
	"cUserId":   true,
	"uLocale":   true,
}

// rankAccountHost 为引导行去重排序：.account.xiaomi.com（0）优先于
// .xiaomi.com（1）——与插件 pickBootstrap/rankAccountHost 同语义。
func rankAccountHost(hostKey string) int {
	d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(hostKey), "."))
	switch {
	case d == "account.xiaomi.com" || strings.HasSuffix(d, ".account.xiaomi.com"):
		return 0
	case d == "xiaomi.com" || strings.HasSuffix(d, ".xiaomi.com"):
		return 1
	default:
		return 2
	}
}

// pickBootstrapRows 从登录侧行里选换票材料：按名去重、账号域优先，稳定序
// passToken→userId→cUserId→uLocale（插件 pickBootstrap 同序，可复现）。
func pickBootstrapRows(rows []cookieRow) []cookieRow {
	byName := map[string]cookieRow{}
	for _, r := range rows {
		if !bootstrapNames[r.Name] || strings.TrimSpace(r.Value) == "" {
			continue
		}
		prev, ok := byName[r.Name]
		if !ok || rankAccountHost(r.HostKey) < rankAccountHost(prev.HostKey) {
			byName[r.Name] = r
		}
	}
	order := []string{"passToken", "userId", "cUserId", "uLocale"}
	out := make([]cookieRow, 0, len(order))
	for _, name := range order {
		if r, ok := byName[name]; ok {
			out = append(out, r)
		}
	}
	return out
}

// probeExchangeResult 汇报一次成功换票——只有结构性信息，无任何凭据值。
type probeExchangeResult struct {
	Region      string
	SID         string
	SetCookie   []string // Set-Cookie 名单（仅名称）
	TokenLength int      // serviceToken 字节数（实测 364）
}

// probeExchange 对一个 sid 跑 P1+P2。成功 = 双 200 且 Set-Cookie 里有
// serviceToken。重定向一律不跟随：P1 带 _json=true 本就该原地 200，被重定向
// 即拒绝，要看原始形态而不是登录页；P2 的 302 同样是拒绝信号。
func probeExchange(bootstrap []cookieRow, sid string, timeout time.Duration) (*probeExchangeResult, error) {
	if sid == "" {
		return nil, errors.New("该区域没有已知 sid（sgp/cn 之外未实测，拒绝臆测）")
	}
	if len(bootstrap) == 0 {
		return nil, errors.New("没有引导材料（passToken/userId/cUserId）")
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// ---- P1：serviceLogin(_json)，带明文账号域行 ----
	pairs := make([]string, 0, len(bootstrap))
	for _, r := range bootstrap {
		pairs = append(pairs, r.Name+"="+r.Value)
	}
	p1 := probePassportBase + "/pass/serviceLogin?_locale=zh_CN&_snsNone=true&sid=" +
		url.QueryEscape(sid) + "&_json=true"
	req, err := http.NewRequest(http.MethodGet, p1, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", strings.Join(pairs, "; "))
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serviceLogin: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 不打印 URL（含 sid）；302 意味着这批引导材料已不构成登录态。
		return nil, fmt.Errorf("serviceLogin: HTTP %d（意外重定向——引导材料可能已失效）", resp.StatusCode)
	}
	parsed, err := parseServiceLoginBody(raw)
	if err != nil {
		return nil, err
	}

	// ---- P2：STS 换票铸造（刻意无 Cookie）----
	p2 := parsed.Location
	if !strings.Contains(p2, "?") {
		p2 += "?"
	}
	p2 += "&clientSign=" + probeClientSign(parsed.Nonce, parsed.Security)
	req2, err := http.NewRequest(http.MethodGet, p2, nil)
	if err != nil {
		return nil, err
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("sts: %w", err)
	}
	// P2 的答案在 Set-Cookie 里；body 排干即弃。
	_, _ = io.ReadAll(io.LimitReader(resp2.Body, 16<<10))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sts: HTTP %d（clientSign 被拒）", resp2.StatusCode)
	}
	names := make([]string, 0, 4)
	tokenLen := 0
	for _, c := range resp2.Cookies() {
		if c == nil || c.Name == "" {
			continue
		}
		names = append(names, c.Name)
		if c.Name == "serviceToken" {
			tokenLen = len(c.Value)
		}
	}
	if tokenLen == 0 {
		return nil, fmt.Errorf("sts: Set-Cookie 无 serviceToken（sid=%s）", sid)
	}
	return &probeExchangeResult{
		Region:      regionForSIDProbe(sid),
		SID:         sid,
		SetCookie:   names,
		TokenLength: tokenLen,
	}, nil
}

// regionForSIDProbe 反查区域名（mimosgp→sgp / mimopc→cn），仅供展示。
func regionForSIDProbe(sid string) string {
	switch sid {
	case "mimosgp":
		return "sgp"
	case "mimopc":
		return "cn"
	default:
		return sid
	}
}
