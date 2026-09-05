package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan limit","extra":{"plan":2}}`, ErrPlanLimit},
		{200, `{"code":1005,"msg":"权益不足"}`, ErrPlanLimit},
		{429, ``, ErrSoftRate},
		{401, `{"code":1001,"msg":"login required"}`, ErrSessionDead},
		{401, ``, ErrSessionDead},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{200, `{"checked_in":false}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:      &http.Client{Transport: fn},
		AgentHost: "https://agent.example",
		UgHost:    "https://ug.example",
		OAuthHost: "https://oauth.example",
		ClientID:  ClientID,
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpExchange) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			return nil, errors.New("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ClientID":"`+ClientID+`"`)) || !bytes.Contains(body, []byte(`"RefreshToken":"oldrt"`)) {
			return nil, errors.New("bad body: " + string(body))
		}
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, APIHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt != 1786805537 {
		t.Errorf("expiresAt=%d", a.ExpiresAt)
	}
}

// TestRefreshTokenExchangeMilliseconds 覆盖上游 TokenExpireAt 返回毫秒的场景：
// 必须归一化为 Unix 秒后再写 auth.ExpiresAt。
func TestRefreshTokenExchangeMilliseconds(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, APIHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930 (毫秒转秒)", a.ExpiresAt)
	}
}

func TestRefreshTokenIfNeededSkipsFresh(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, APIHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("fresh token should not refresh")
	}
	if calls != 0 {
		t.Errorf("ExchangeToken should not be called, calls=%d", calls)
	}
	if a.AccessToken != "at" {
		t.Error("token should remain unchanged")
	}
}

func TestRefreshTokenIfNeededRefreshesExpired(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, APIHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || calls != 1 {
		t.Errorf("expired token should refresh once, refreshed=%v calls=%d", refreshed, calls)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

func TestRefreshTokenUsesAuthAPIHost(t *testing.T) {
	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Scheme + "://" + r.URL.Host
		return jsonResp(200, `{"Result":{"Token":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, APIHost: "https://custom.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if gotHost != "https://custom.example" {
		t.Errorf("host=%s want auth.apiHost", gotHost)
	}
}

func TestChatStreamSendsHeadersAndRewritesBody(t *testing.T) {
	var gotAuth, gotUID, gotAppID, gotIdeVer string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-Uid")
		gotAppID = r.Header.Get("X-App-Id")
		gotIdeVer = r.Header.Get("X-Ide-Version")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Cloud-IDE-JWT at" || gotUID != "u1" {
		t.Errorf("headers: auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotAppID != AppID || gotIdeVer != "0.1.43" {
		t.Errorf("app headers: appid=%q idever=%q", gotAppID, gotIdeVer)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) || !bytes.Contains(gotBody, []byte(`"function":"inline_chat"`)) {
		t.Errorf("body not rewritten: %s", gotBody)
	}
}

func TestChatStreamUsesDedicatedStreamClient(t *testing.T) {
	// StreamHTTP 优先于 HTTP 被 ChatStream 使用（无总超时的长 SSE 流客户端）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	c.StreamHTTP = &http.Client{Transport: c.HTTP.Transport} // 无 Timeout
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("stream client should have no total timeout, got %v", c.StreamHTTP.Timeout)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `rate limited`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 429 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("429 should come via status, err=%v", err)
	}
	if Classify(status, string(respBody)) != ErrSoftRate {
		t.Errorf("not classified soft rate: %q", respBody)
	}
}

func TestUserEntUsageAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" {
			return nil, errors.New("missing auth header")
		}
		// v0.12.28: quota 走真实字段 basic_usage_limit（credits_limit 在
		// v2 API 中不存在，上游 cockpit-tools 从不读取它）。
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[
                        {"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":2000}},"usage":{"basic_usage_amount":300}},
                        {"entitlement_base_info":{"product_type":0,"quota":{"basic_usage_limit":500}}}
                ]}`), nil
	})
	res, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	if !res.IsCreditsBilling {
		t.Error("is_credits_billing should be true")
	}
	pack := SelectActivePack(res.UserEntitlementPackList, true)
	if pack == nil {
		t.Fatal("no active pack selected")
	}
	remain, ok := pack.PackRemain()
	if !ok || remain != 1700 {
		t.Errorf("active pack remain=%d ok=%v want 1700", remain, ok)
	}
	sum := SummarizeUsage(res.UserEntitlementPackList, true)
	if sum.UsageModel != "basic" || !sum.RemainKnown || sum.Remain != 1700 {
		t.Errorf("summary: model=%s known=%v remain=%d want basic/true/1700", sum.UsageModel, sum.RemainKnown, sum.Remain)
	}
}

func TestSummarizeUsageFastRequest(t *testing.T) {
	// 速通证据存在 → fast 模型，available = sum(limit)-sum(used)。
	body := `[` +
		`{"entitlement_base_info":{"product_type":6,"quota":{"premium_model_fast_request_limit":50}},"usage":{"premium_model_fast_amount":20}},` +
		`{"entitlement_base_info":{"product_type":0,"quota":{"basic_usage_limit":500}}}` +
		`]`
	var packs []EntitlementPack
	if err := json.Unmarshal([]byte(body), &packs); err != nil {
		t.Fatal(err)
	}
	sum := SummarizeUsage(packs, true)
	if sum.UsageModel != "fast" || !sum.RemainKnown || sum.Remain != 30 {
		t.Errorf("fast summary: model=%s known=%v remain=%d want fast/true/30", sum.UsageModel, sum.RemainKnown, sum.Remain)
	}
	// 无任何 fast/basic 证据（Free 包无 quota）→ unknown，不猜测。
	body2 := `[` +
		`{"entitlement_base_info":{"product_type":0}}` +
		`]`
	var packs2 []EntitlementPack
	if err := json.Unmarshal([]byte(body2), &packs2); err != nil {
		t.Fatal(err)
	}
	sum2 := SummarizeUsage(packs2, true)
	if sum2.UsageModel != "unknown" || sum2.RemainKnown {
		t.Errorf("unknown summary: model=%s known=%v want unknown/false", sum2.UsageModel, sum2.RemainKnown)
	}
	// 无限速通（limit=-1）→ available=-1。
	body3 := `[` +
		`{"entitlement_base_info":{"product_type":6,"quota":{"premium_model_fast_request_limit":-1}},"usage":{"premium_model_fast_amount":7}}` +
		`]`
	var packs3 []EntitlementPack
	if err := json.Unmarshal([]byte(body3), &packs3); err != nil {
		t.Fatal(err)
	}
	sum3 := SummarizeUsage(packs3, true)
	if sum3.UsageModel != "fast" || sum3.Remain != -1 {
		t.Errorf("unlimited summary: model=%s remain=%d want fast/-1", sum3.UsageModel, sum3.Remain)
	}
}

func TestSelectActivePackPriority(t *testing.T) {
	body := `[` +
		`{"entitlement_base_info":{"product_type":1,"quota":{"basic_usage_limit":300}}},` +
		`{"entitlement_base_info":{"product_type":6,"quota":{"basic_usage_limit":1500}}},` +
		`{"entitlement_base_info":{"product_type":0,"quota":{"basic_usage_limit":100}}}` +
		`]`
	var packs []EntitlementPack
	if err := json.Unmarshal([]byte(body), &packs); err != nil {
		t.Fatal(err)
	}
	if p := SelectActivePack(packs, true); p == nil || p.EntitlementBaseInfo.ProductType != 6 {
		t.Errorf("CN should pick Ultra(6), got %+v", p)
	}
	if p := SelectActivePack(packs, false); p == nil || p.EntitlementBaseInfo.ProductType != 6 {
		t.Errorf("Intl should pick Ultra(6), got %+v", p)
	}

	// 隐藏 (is_hide) 与已取消 (status=3) 的 pack 必须被过滤
	body2 := `[` +
		`{"entitlement_base_info":{"product_type":100,"quota":{"basic_usage_limit":9000},"is_hide":true}},` +
		`{"entitlement_base_info":{"product_type":5,"quota":{"basic_usage_limit":800},"status":3}},` +
		`{"entitlement_base_info":{"product_type":1,"quota":{"basic_usage_limit":300}}}` +
		`]`
	var packs2 []EntitlementPack
	if err := json.Unmarshal([]byte(body2), &packs2); err != nil {
		t.Fatal(err)
	}
	if p := SelectActivePack(packs2, true); p == nil || p.EntitlementBaseInfo.ProductType != 1 {
		t.Errorf("hidden/cancelled must be skipped, want Pro(1), got %+v", p)
	}
}

func TestCheckinStatusAndClaim(t *testing.T) {
	// v0.12.41: 官方契约 = POST + {"req_source":N}（TraeCode 2.3.79946 deb 与
	// TraeWork 2.3.81345 exe 双版反编译交叉实证）；req_source=1 是 TRAE 谱系
	// token 的正确值（我方 ClientID=ono9krqynydwx5），居探测序列首位。
	var path, method, body string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path, method = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		if r.Header.Get("X-User-Region") != "CN" {
			return nil, errors.New("missing X-User-Region")
		}
		return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
	})
	res, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckedIn || !res.Enable || res.Credits != 200 {
		t.Errorf("status: checked=%v enable=%v credits=%d", res.CheckedIn, res.Enable, res.Credits)
	}
	if path != EpCheckinStatus {
		t.Errorf("path=%s", path)
	}
	if method != http.MethodPost {
		t.Errorf("method=%s, want POST (official contract)", method)
	}
	if body != `{"req_source":1}` {
		t.Errorf("body=%s, want first-probe req_source:1 literal", body)
	}
}

func TestCheckinAuthSchemeAlignment(t *testing.T) {
	// v0.12.38: 签到鉴权双方案——Cloud-IDE-JWT 优先（官方客户端逆向实证
	// FINDINGS §五；v0.12.34 我方 status 实测 code=0），Bearer 回退
	// （cockpit-tools rs:2761,2859 的 token 类别）。pay/usage 恒为 Cloud-IDE-JWT。
	a := &auth.Auth{AccessToken: "tok", DeviceID: "dev-1"}
	req, err := ugCheckinRequest(a, http.MethodPost, "https://api.trae.cn/x", "{}", UgSchemeCloudIDEJWT)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Errorf("checkin primary Authorization=%q, want Cloud-IDE-JWT tok", got)
	}
	if got := req.Header.Get("x-device-id"); got != "dev-1" {
		t.Errorf("checkin x-device-id=%q, want dev-1", got)
	}
	reqB, err := ugCheckinRequest(a, http.MethodPost, "https://api.trae.cn/x", "", UgSchemeBearer)
	if err != nil {
		t.Fatal(err)
	}
	if got := reqB.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("checkin fallback Authorization=%q, want Bearer tok", got)
	}
	reqPay, _ := http.NewRequest(http.MethodPost, "https://api.trae.cn/x", nil)
	UgHeaders(reqPay, a)
	if got := reqPay.Header.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Errorf("pay/usage Authorization=%q, want Cloud-IDE-JWT tok", got)
	}
	// 端到端：CheckinStatus 首选方案必须是 Cloud-IDE-JWT。
	var saw string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		saw = r.Header.Get("Authorization")
		return jsonResp(200, `{"checked_in":false,"credits":150,"enable":true}`), nil
	})
	resp, err := c.CheckinStatus(a)
	if err != nil {
		t.Fatal(err)
	}
	if saw != "Cloud-IDE-JWT tok" {
		t.Errorf("first attempt auth=%q, want Cloud-IDE-JWT tok", saw)
	}
	if resp.SchemeUsed != UgSchemeCloudIDEJWT {
		t.Errorf("SchemeUsed=%q, want Cloud-IDE-JWT", resp.SchemeUsed)
	}
	if !resp.Enable || resp.Credits != 150 {
		t.Errorf("status payload = %+v, want enable/credits=150", resp)
	}
}
func TestCheckinSchemeFallbackStatus(t *testing.T) {
	// 首选 Cloud-IDE-JWT 被拒（1001）→ 回退 Bearer 成功：SchemeUsed=Bearer，
	// 两个方案的 Authorization 都按预期发出。
	a := &auth.Auth{AccessToken: "tok", DeviceID: "dev-1"}
	var auths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		auths = append(auths, r.Header.Get("Authorization"))
		if len(auths) == 1 {
			return jsonResp(200, `{"code":1001,"message":"token class rejected"}`), nil
		}
		return jsonResp(200, `{"code":0,"checked_in":false,"credits":150,"enable":true}`), nil
	})
	resp, err := c.CheckinStatus(a)
	if err != nil {
		t.Fatal(err)
	}
	if resp.SchemeUsed != UgSchemeBearer {
		t.Errorf("SchemeUsed=%q, want Bearer", resp.SchemeUsed)
	}
	if resp.Credits != 150 {
		t.Errorf("credits=%d, want 150", resp.Credits)
	}
	if len(auths) != 2 || auths[0] != "Cloud-IDE-JWT tok" || auths[1] != "Bearer tok" {
		t.Errorf("attempts=%v, want [Cloud-IDE-JWT tok, Bearer tok]", auths)
	}
}
func TestCheckinSchemeFallbackClaim(t *testing.T) {
	// claim 同构：Cloud-IDE-JWT 失败后回退 Bearer 成功 + ClaimCredits 提取。
	a := &auth.Auth{AccessToken: "tok"}
	n := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return jsonResp(200, `{"code":1001,"message":"denied"}`), nil
		}
		return jsonResp(200, `{"code":0,"message":"ok","add_credits":150}`), nil
	})
	resp, err := c.CheckinClaim(a)
	if err != nil {
		t.Fatal(err)
	}
	if resp.SchemeUsed != UgSchemeBearer {
		t.Errorf("SchemeUsed=%q, want Bearer", resp.SchemeUsed)
	}
	if resp.ClaimCredits == nil || *resp.ClaimCredits != 150 {
		t.Errorf("ClaimCredits=%v, want 150", resp.ClaimCredits)
	}
}
func TestCheckinReqSourceProbeOn9074(t *testing.T) {
	// v0.12.41: 9074 = 活动校验拒绝（req_source 与 token 产品谱系错配）→ 换
	// req_source 重试一次；两个源都被拒才把 9074 作为 soft rate-limit 上抛。
	a := &auth.Auth{AccessToken: "tok"}
	var bodies []string
	n := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		n++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if n == 1 {
			return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
		}
		return jsonResp(200, `{"code":0,"message":"ok","add_credits":150}`), nil
	})
	resp, err := c.CheckinClaim(a)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || bodies[0] != `{"req_source":1}` || bodies[1] != `{"req_source":2}` {
		t.Errorf("probe order = %v (%d attempts), want [req_source:1 req_source:2]", bodies, n)
	}
	if resp.ReqSourceUsed != `{"req_source":2}` || resp.ClaimCredits == nil || *resp.ClaimCredits != 150 {
		t.Errorf("resp = %+v, want ReqSourceUsed=req_source:2 + 150 credits", resp)
	}

	// 两个 req_source 均被拒（真·活动侧拒绝）→ 每源一次 Cloud-IDE-JWT 尝试
	// （9074 不在同源内换鉴权方案），错误仍以 biz 9074 上抛供调度器退避重试。
	n2 := 0
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		n2++
		return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
	})
	_, err = c2.CheckinClaim(a)
	if n2 != 2 {
		t.Errorf("attempts=%d, want 2 (one per req_source)", n2)
	}
	var ue *Error
	if !errors.As(err, &ue) || !IsRateLimit9074(ue.BizCode) {
		t.Errorf("9074 should surface as biz rate limit, got %v", err)
	}
}
func TestBizErrorSurfacesUpstreamMessage(t *testing.T) {
	// v0.12.38: 上游 message 必须透传（此前硬编码"Token 已过期"掩盖 1001 真因）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":1001,"message":"upstream says XYZ"}`), nil
	})
	_, err := c.CheckinStatus(&auth.Auth{AccessToken: "bad"})
	if err == nil {
		t.Fatal("code=1001 should error")
	}
	if !strings.Contains(err.Error(), "upstream says XYZ") {
		t.Errorf("error should carry upstream message, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "biz_code=1001") {
		t.Errorf("error should carry biz code, got %q", err.Error())
	}
}

func TestCheckinBizCodeNonZero(t *testing.T) {
	// 对齐上游 code!=0 → 错误（Token 已过期）语义；绝不能当成“未签到”通过。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":1001,"checked_in":false,"credits":0,"enable":false}`), nil
	})
	if _, err := c.CheckinStatus(&auth.Auth{AccessToken: "bad"}); err == nil {
		t.Fatal("code=1001 should error")
	}
	if _, err := c.CheckinClaim(&auth.Auth{AccessToken: "bad"}); err == nil {
		t.Fatal("claim code=1001 should error")
	}
	// 9074 限流 → BizCode 可识别，重试语义保留。
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
	})
	_, err := c2.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("code=9074 should error")
	}
	var ue *Error
	if !errors.As(err, &ue) || !IsRateLimit9074(ue.BizCode) {
		t.Errorf("9074 should surface as biz rate limit, got %v", err)
	}
}

func TestPayStatusParsing(t *testing.T) {
	// v0.12.29: ide_user_pay_status 容错解析 —— 对齐上游 getCnEntitlementDetailFields
	// 的三段回退（detail → entitlementInfo → originPayStatusData）。Free CN/SOLO
	// 账户的 ent_usage pack 无 quota，快请求/月与 SOLO 并发只能从这里拿。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpPayStatus) {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"user_pay_identity_str":"free",`+
			`"detail":{"fast_request_per":50,"can_get_express_status":1},`+
			`"quota":{"solo_agent_parallel_limit":2,"enable_solo_agent":true}}`), nil
	})
	ps, err := c.PayStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if v := ps.FastRequestPer(); v == nil || *v != 50 {
		t.Errorf("fast_request_per = %v, want 50", v)
	}
	if v := ps.CanGetExpressStatus(); v == nil || *v != 1 {
		t.Errorf("can_get_express_status = %v, want 1", v)
	}
	if v := ps.SoloParallelLimit(); v == nil || *v != 2 {
		t.Errorf("solo_agent_parallel_limit = %v, want 2", v)
	}
	if !ps.HasSoloPackage() {
		t.Error("enable_solo_agent=true should set HasSoloPackage")
	}
	if got := ps.PlanIdentity(); got != "free" {
		t.Errorf("plan identity = %q, want free", got)
	}
	// camelCase 键名 + entitlementInfo/originPayStatusData 回退。
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"entitlementInfo":{"detail":{"fastRequestPer":30}},`+
			`"originPayStatusData":{"detail":{"canGetExpressStatus":0}}}`), nil
	})
	ps2, err := c2.PayStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if v := ps2.FastRequestPer(); v == nil || *v != 30 {
		t.Errorf("fallback fastRequestPer = %v, want 30", v)
	}
	if v := ps2.CanGetExpressStatus(); v == nil || *v != 0 {
		t.Errorf("fallback canGetExpressStatus = %v, want 0", v)
	}
	if ps2.SoloParallelLimit() != nil {
		t.Error("absent solo limit should be nil, not 0")
	}
	if ps2.HasSoloPackage() {
		t.Error("absent enable_solo_* should not set HasSoloPackage")
	}
	// IsFreePlan：中英文两种 display_desc 都要命中（上游 .includes('free') 漏"免费"）。
	if !IsFreePlan("免费", "") || !IsFreePlan("Free Plan", "") || !IsFreePlan("", "free") {
		t.Error("IsFreePlan should match 免费/Free/plan_type=free")
	}
	if IsFreePlan("Pro+", "") || IsFreePlan("专业版", "") {
		t.Error("IsFreePlan should not match paid plans")
	}
}

// v0.12.32: claim 响应中的入账证据字段提取 —— "code=0 但未到账"诊断链路的一部分。
func TestPickCreditField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *int64
	}{
		{"top-level credits", `{"code":0,"credits":150}`, ptr64(150)},
		{"nested data", `{"code":0,"data":{"add_credits":200}}`, ptr64(200)},
		{"absent", `{"code":0,"message":"ok"}`, nil},
		{"non-numeric", `{"code":0,"credits":"150"}`, nil},
		{"bad json", `not-json`, nil},
	}
	for _, tc := range cases {
		got := pickCreditField([]byte(tc.body))
		if tc.want == nil && got != nil {
			t.Errorf("%s: got %v, want nil", tc.name, *got)
		}
		if tc.want != nil && (got == nil || *got != *tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, *tc.want)
		}
	}
}

func ptr64(v int64) *int64 { return &v }
