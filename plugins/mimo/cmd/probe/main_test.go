// main_test.go — probe 换票孪生实现的测试。与插件 plugin_test.go 同源：
// clientSign 用独立于实现原语的已知答案向量，wire 形状用 httptest 双桩断言
// （P1 带引导行、P2 刻意无 Cookie），parse/去重逻辑覆盖边界形态。
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProbeClientSignVector(t *testing.T) {
	// 与插件 TestClientSignVector 同向量：sha1("nonce=1234567890&abcdefgh")
	// → base64 → url-escape。独立于实现自身原语。
	if got := probeClientSign("1234567890", "abcdefgh"); got != "02i8YjagkChj1jgJHHyz0OSzPJs%3D" {
		t.Fatalf("probeClientSign = %q", got)
	}
}

func TestParseServiceLoginBody(t *testing.T) {
	ok := []byte("&&&START&&&{\"code\":0,\"ssecurity\":\"sec\",\"nonce\":\"123\"," +
		"\"location\":\"https://sts.example/sts?x=1\"}&&&END&&&")
	res, err := parseServiceLoginBody(ok)
	if err != nil {
		t.Fatalf("framed payload rejected: %v", err)
	}
	if res.Security != "sec" || res.Nonce != "123" || res.Location != "https://sts.example/sts?x=1" {
		t.Fatalf("parsed fields wrong: %+v", res)
	}

	// 字符串 code 的 "0" 形态同样放行。
	if _, err := parseServiceLoginBody([]byte(`{"code":"0","ssecurity":"s","nonce":"n","location":"l"}`)); err != nil {
		t.Fatalf("string code 0 rejected: %v", err)
	}

	// 裸数字 nonce = 真机实测线型（2026-09-24）。known-answer：解组出的数字
	// 必须正是 probeClientSign 哈希的那串（与 TestProbeClientSignVector 同向量）。
	bare := []byte(`{"code":0,"ssecurity":"abcdefgh","nonce":1234567890,"location":"https://sts.example/sts?x=1"}`)
	res2, err := parseServiceLoginBody(bare)
	if err != nil || res2.Nonce != "1234567890" {
		t.Fatalf("bare-number nonce: %+v err=%v", res2, err)
	}
	if got := probeClientSign(string(res2.Nonce), res2.Security); got != "02i8YjagkChj1jgJHHyz0OSzPJs%3D" {
		t.Fatalf("probeClientSign from parsed bare nonce = %q", got)
	}

	// 实测量级（19 位）：float64 解法会因 53 位尾数丢精度，字面量必须逐字保留。
	huge := []byte(`{"code":0,"ssecurity":"s","nonce":4341996316119746560,"location":"https://sts.example/l"}`)
	if res3, err := parseServiceLoginBody(huge); err != nil || res3.Nonce != "4341996316119746560" {
		t.Fatalf("19-digit nonce must survive verbatim: %+v err=%v", res3, err)
	}

	// 空 / null nonce 同样是形态异常。
	if _, err := parseServiceLoginBody([]byte(`{"code":0,"ssecurity":"s","nonce":"","location":"l"}`)); err == nil {
		t.Fatal("empty nonce accepted")
	}
	if _, err := parseServiceLoginBody([]byte(`{"code":0,"ssecurity":"s","nonce":null,"location":"l"}`)); err == nil {
		t.Fatal("null nonce accepted")
	}

	// 非 0 code = passToken 被拒（重试无益，唯一修复是桌面重登），且不算形态异常。
	_, err = parseServiceLoginBody([]byte(`{"code":1020,"ssecurity":"","nonce":"","location":""}`))
	if !errors.Is(err, errProbePassTokenExpired) || errors.Is(err, errProbeBodyShape) {
		t.Fatalf("code 1020 → %v, want errProbePassTokenExpired only", err)
	}
	_, err = parseServiceLoginBody([]byte(`{"code":"1010","ssecurity":"s","nonce":"n","location":"l"}`))
	if !errors.Is(err, errProbePassTokenExpired) {
		t.Fatalf("string code 1010 → %v, want errProbePassTokenExpired", err)
	}

	// 缺 P2 所需字段 / 非 JSON 都算形态异常 —— 必须与网络原因区分开
	// （总结论三分类的判据）。
	_, err = parseServiceLoginBody([]byte(`{"code":0,"ssecurity":"s","nonce":"n"}`))
	if !errors.Is(err, errProbeBodyShape) {
		t.Fatalf("missing location → %v, want errProbeBodyShape", err)
	}
	_, err = parseServiceLoginBody([]byte("not json"))
	if !errors.Is(err, errProbeBodyShape) {
		t.Fatalf("garbage → %v, want errProbeBodyShape", err)
	}
}

func TestPickBootstrapRows(t *testing.T) {
	rows := []cookieRow{
		{Name: "uLocale", Value: "zh", HostKey: ".xiaomi.com"},
		{Name: "userId", Value: "mirror", HostKey: ".xiaomi.com"},
		{Name: "userId", Value: "primary", HostKey: ".account.xiaomi.com"},
		{Name: "cUserId", Value: "", HostKey: ".xiaomi.com"}, // 空值剔除
		{Name: "_ga", Value: "x", HostKey: ".xiaomi.com"},    // 非引导行
		{Name: "passToken", Value: "pt", HostKey: ".account.xiaomi.com"},
	}
	got := pickBootstrapRows(rows)
	if len(got) != 3 {
		t.Fatalf("picked %d rows, want 3 (cUserId dropped, _ga ignored)", len(got))
	}
	for i, name := range []string{"passToken", "userId", "uLocale"} {
		if got[i].Name != name {
			t.Fatalf("position %d = %s, want %s", i, got[i].Name, name)
		}
	}
	// 同名去重时账号域（.account.xiaomi.com）优先于 .xiaomi.com 镜像。
	if got[1].Value != "primary" {
		t.Fatalf("userId = %q, want account-host row %q", got[1].Value, "primary")
	}
}

func TestProbeExchangeWire(t *testing.T) {
	var stsServer *httptest.Server
	passport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "_json=true") {
			t.Errorf("P1 missing _json=true: %s", r.URL.RawQuery)
		}
		if got := r.URL.Query().Get("sid"); got != "mimosgp" {
			t.Errorf("P1 sid = %q, want mimosgp", got)
		}
		cookie := r.Header.Get("Cookie")
		for _, pair := range []string{"passToken=pt", "userId=42", "cUserId=7", "uLocale=zh"} {
			if !strings.Contains(cookie, pair) {
				t.Errorf("P1 cookie missing %q: %q", pair, cookie)
			}
		}
		// 账号域行只发给 passport（原生域）——这正是 P1；P1 不该带任何上游头。
		if r.Header.Get("X-Mimo-Source") != "" || r.Header.Get("Authorization") != "" {
			t.Error("P1 must not carry upstream lane headers")
		}
		w.Header().Set("Content-Type", "text/plain")
		// nonce 为裸数字 —— 真机实测线型（2026-09-24）。
		fmt.Fprintf(w, "&&&START&&&{\"code\":0,\"ssecurity\":\"abcdefgh\",\"nonce\":1234567890,"+
			"\"location\":\"%s/sts?callback=https%%3A%%2F%%2Fmimo-server-sgp.xiaomimimo.com%%2Fapi%%2Fsts\"}&&&END&&&",
			stsServer.URL)
	}))
	defer passport.Close()
	stsServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// P2 刻意无 Cookie —— clientSign 就是凭据。
		if r.Header.Get("Cookie") != "" {
			t.Errorf("P2 must be cookieless, got %q", r.Header.Get("Cookie"))
		}
		// clientSign 独立复算（不调 probeClientSign，与实现原语解耦）。
		sum := sha1.Sum([]byte("nonce=1234567890&abcdefgh"))
		want, _ := url.QueryUnescape(url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:])))
		if got := r.URL.Query().Get("clientSign"); got != want {
			t.Errorf("clientSign = %q, want %q", got, want)
		}
		http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: strings.Repeat("t", 364)})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "42"})
		http.SetCookie(w, &http.Cookie{Name: "mimosgp_ph", Value: "ph"})
		http.SetCookie(w, &http.Cookie{Name: "mimosgp_slh", Value: "slh"})
		w.WriteHeader(http.StatusOK)
	}))
	defer stsServer.Close()

	oldBase := probePassportBase
	probePassportBase = passport.URL
	defer func() { probePassportBase = oldBase }()

	bootstrap := pickBootstrapRows([]cookieRow{
		{Name: "passToken", Value: "pt", HostKey: ".account.xiaomi.com"},
		{Name: "userId", Value: "42", HostKey: ".account.xiaomi.com"},
		{Name: "cUserId", Value: "7", HostKey: ".account.xiaomi.com"},
		{Name: "uLocale", Value: "zh", HostKey: ".xiaomi.com"},
	})
	res, err := probeExchange(bootstrap, "mimosgp", 5*time.Second)
	if err != nil {
		t.Fatalf("probeExchange: %v", err)
	}
	if res.Region != "sgp" || res.SID != "mimosgp" || res.TokenLength != 364 {
		t.Fatalf("result wrong: %+v", res)
	}
	for _, name := range []string{"serviceToken", "userId", "mimosgp_ph", "mimosgp_slh"} {
		found := false
		for _, n := range res.SetCookie {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("Set-Cookie names %v missing %q", res.SetCookie, name)
		}
	}
}

func TestProbeExchangePassTokenRejected(t *testing.T) {
	passport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "&&&START&&&{\"code\":1020}&&&END&&&")
	}))
	defer passport.Close()
	oldBase := probePassportBase
	probePassportBase = passport.URL
	defer func() { probePassportBase = oldBase }()

	bootstrap := []cookieRow{
		{Name: "passToken", Value: "dead", HostKey: ".account.xiaomi.com"},
		{Name: "userId", Value: "42", HostKey: ".account.xiaomi.com"},
	}
	_, err := probeExchange(bootstrap, "mimosgp", 5*time.Second)
	if !errors.Is(err, errProbePassTokenExpired) {
		t.Fatalf("dead passToken → %v, want errProbePassTokenExpired", err)
	}

	// 无 sid / 无引导材料的守卫。
	if _, err := probeExchange(bootstrap, "", time.Second); err == nil {
		t.Fatal("empty sid accepted")
	}
	if _, err := probeExchange(nil, "mimosgp", time.Second); err == nil {
		t.Fatal("empty bootstrap accepted")
	}
}
