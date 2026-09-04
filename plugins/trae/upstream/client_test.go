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
	var path string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
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
