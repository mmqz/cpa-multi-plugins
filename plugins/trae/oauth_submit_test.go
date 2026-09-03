package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// v0.12.16: paste-to-complete fallback. The pasted URL is the FULL failed
// redirect (real trae shape: isRedirect + authCodeInfo JSON + loginTraceID +
// host + userRegion + userInfo — no state/authCode params), and oauth_submit
// must replay it through the exact same resolution path as a direct callback
// hit.

func realFormRedirectURL(state string) string {
	authCodeInfo, _ := json.Marshal(map[string]any{
		"AuthCode":       "REAL-FORM-CODE-1",
		"ExpireAt":       time.Now().Add(10 * time.Minute).UnixMilli(),
		"ExpireDuration": 600000,
	})
	userInfo, _ := json.Marshal(map[string]any{
		"AIRegion": "CN", "Region": "CN", "UserID": "1237380756941412",
	})
	q := url.Values{}
	q.Set("isRedirect", "true")
	q.Set("scope", "trae")
	q.Set("authCodeInfo", string(authCodeInfo))
	q.Set("loginTraceID", state)
	q.Set("host", "https://api.trae.com.cn")
	q.Set("userRegion", "cn")
	q.Set("userInfo", string(userInfo))
	return "http://127.0.0.1:41961/authorize?" + q.Encode()
}

func TestHandleOAuthSubmitResourcePOST(t *testing.T) {
	state := "test-submit-cn-001"
	lc := &loginCtx{variant: variantCN, state: state, loginTraceID: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)

	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(realFormRedirectURL(state)) + `}`),
	})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("submit body = %s", body)
	}
	if lc.authCode != "REAL-FORM-CODE-1" {
		t.Fatalf("authCode not extracted from authCodeInfo: %+v", lc)
	}
	if lc.loginHost != "https://api.trae.com.cn" {
		t.Fatalf("loginHost not picked up from host param: %+v", lc)
	}
	select {
	case <-lc.done:
	default:
		t.Fatalf("lc.done not closed after submit")
	}
}

func TestHandleOAuthSubmitResourceGETCBURL(t *testing.T) {
	state := "test-submit-cn-002"
	lc := &loginCtx{variant: variantSolo, state: state, loginTraceID: state, expires: time.Now().Add(time.Minute)}
	loginStates.Store(state, lc)
	defer loginStates.Delete(state)

	q := url.Values{}
	q.Set("cb_url", realFormRedirectURL(state))
	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET", Query: q})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("submit body = %s", body)
	}
	if lc.authCode != "REAL-FORM-CODE-1" {
		t.Fatalf("authCode not extracted: %+v", lc)
	}
}

func TestHandleOAuthSubmitResourceIntl(t *testing.T) {
	state := "test-submit-intl-003"
	lc := &intlloginCtx{state: state, expires: time.Now().Add(time.Minute)}
	intlloginStates.Store(state, lc)
	defer intlloginStates.Delete(state)

	// intl redirect echoes loginTraceID too; host is the intl API host.
	raw := strings.Replace(realFormRedirectURL(state), "api.trae.com.cn", "api.trae.ai", 1)
	body := handleOAuthSubmitResource(pluginapi.ManagementRequest{
		Method: "POST",
		Body:   []byte(`{"url":` + mustJSON(raw) + `}`),
	})
	if !strings.Contains(string(body), "Login successful") {
		t.Fatalf("submit body = %s", body)
	}
	if lc.authCode != "REAL-FORM-CODE-1" {
		t.Fatalf("intl authCode: %+v", lc)
	}
}

func TestHandleOAuthSubmitResourceErrors(t *testing.T) {
	if body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "POST", Body: []byte(`{}`)}); !strings.Contains(string(body), "Missing callback URL") {
		t.Fatalf("empty body: %s", body)
	}
	if body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET"}); !strings.Contains(string(body), "Missing callback URL") {
		t.Fatalf("empty query: %s", body)
	}
	q := url.Values{}
	q.Set("cb_url", "not a url at all")
	if body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET", Query: q}); !strings.Contains(string(body), "Invalid callback URL") {
		t.Fatalf("unparseable url: %s", body)
	}
	q2 := url.Values{}
	q2.Set("cb_url", "http://127.0.0.1:1/plain")
	if body := handleOAuthSubmitResource(pluginapi.ManagementRequest{Method: "GET", Query: q2}); !strings.Contains(string(body), "Invalid callback URL") {
		t.Fatalf("url without query: %s", body)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
