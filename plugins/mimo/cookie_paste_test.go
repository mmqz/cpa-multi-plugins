package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestParseBootstrapCookieBlob pins the paste shapes a human actually copies
// (raw Cookie header, DevTools k=v pairs, newline-separated pairs, JSON) and
// the garbage filters (non-bootstrap names, empty values, prose lines).
func TestParseBootstrapCookieBlob(t *testing.T) {
	mk := func(name, value string) mimoCookie {
		return mimoCookie{Name: name, Value: value, Domain: ".account.xiaomi.com", Path: "/", Secure: true, HTTPOnly: true}
	}
	want := []mimoCookie{mk("passToken", "pt-value"), mk("userId", "3839"), mk("cUserId", "cu")}
	tests := []struct {
		name     string
		raw      string
		wantRows int
	}{
		{"cookie header", "Cookie: passToken=pt-value; userId=3839; cUserId=cu", 3},
		{"cookie header lowercase", "cookie: passToken=pt-value; userId=3839; cUserId=cu", 3},
		{"plain pairs", "passToken=pt-value; userId=3839; cUserId=cu", 3},
		{"newline separated", "passToken=pt-value\nuserId=3839\ncUserId=cu", 3},
		{"mixed prose", "好的我复制了：\npassToken=pt-value;\nuserId=3839;\n然后呢？", 2},
		{"json", `{"passToken":"pt-value","userId":"3839","cUserId":"cu"}`, 3},
		{"quoted values", `passToken="pt-value"; userId="3839"`, 2},
	}
	for _, tc := range tests {
		jar := parseBootstrapCookieBlob(tc.raw)
		if len(jar) != tc.wantRows {
			t.Errorf("%s: got %d rows %+v, want %d", tc.name, len(jar), jar, tc.wantRows)
			continue
		}
		for i, c := range jar {
			if c.Name != want[i].Name || c.Value != want[i].Value || c.Domain != want[i].Domain || c.Path != want[i].Path {
				t.Errorf("%s: row %d = %+v, want %+v", tc.name, i, c, want[i])
			}
		}
	}
	// passToken's own colon must survive the first-= cut.
	jar := parseBootstrapCookieBlob("passToken=V1:abc+de==; userId=3839")
	if len(jar) != 2 || jar[0].Value != "V1:abc+de==" {
		t.Fatalf("V1: colon value mangled: %+v", jar)
	}
	// Garbage in → empty jar (and the handler rejects with the hint).
	if jar := parseBootstrapCookieBlob("这是一段没有任何键值对的文字"); len(jar) != 0 {
		t.Fatalf("prose parsed as rows: %+v", jar)
	}
}

// TestPasteJarMissing: passToken+userId are the exchange's minimum; the
// optional rows never block a submit.
func TestPasteJarMissing(t *testing.T) {
	mk := func(name string) mimoCookie { return mimoCookie{Name: name, Value: "v", Domain: ".account.xiaomi.com"} }
	if msg := pasteJarMissing(nil); !strings.Contains(msg, "passToken") || !strings.Contains(msg, "userId") {
		t.Fatalf("empty jar message = %q", msg)
	}
	if msg := pasteJarMissing([]mimoCookie{mk("passToken"), mk("userId")}); msg != "" {
		t.Fatalf("minimum jar rejected: %q", msg)
	}
	if msg := pasteJarMissing([]mimoCookie{mk("passToken")}); !strings.Contains(msg, "userId") {
		t.Fatalf("missing-userId message = %q", msg)
	}
	if msg := pasteJarMissing([]mimoCookie{mk("passToken"), mk("userId"), mk("cUserId"), mk("uLocale")}); msg != "" {
		t.Fatalf("full jar rejected: %q", msg)
	}
}

// TestCookieSubmitEndToEnd drives the real resource route: POST pasted rows,
// stubbed passport mints sgp, credential lands in the (stubbed) host store as
// mimo-cookie-<uid>.json with the manual-paste source and the minted ticket.
// hostAuthList has no live bridge under tests and fails harmlessly — the
// same-uid name-reuse branch is skipped, so the canonical default name lands.
func TestCookieSubmitEndToEnd(t *testing.T) {
	srv := withPassportStub(t)
	swapServiceLoginBase(t, srv.URL)

	var savedName string
	var savedRaw []byte
	hostAuthPersistFn = func(name string, raw []byte) error {
		savedName, savedRaw = name, raw
		return nil
	}
	defer func() { hostAuthPersistFn = hostAuthPersist }()

	form := url.Values{"cookies": {"passToken=pt; userId=3839; cUserId=cu"}, "region": {"sgp"}}
	resp := handleMimoCookieSubmit(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/mimo/cookie_submit",
		Body:   []byte(form.Encode()),
	})
	page := string(resp)
	if !strings.Contains(page, "粘贴登录完成") || !strings.Contains(page, "现场换票成功") {
		t.Fatalf("page missing success markers: %s", page)
	}

	if savedName != "mimo-cookie-3839.json" {
		t.Fatalf("persist name = %q, want mimo-cookie-3839.json", savedName)
	}
	var file struct {
		Type string `json:"type"`
		Auth struct {
			Lane    string `json:"lane"`
			UID     string `json:"uid"`
			Region  string `json:"region"`
			SID     string `json:"sid"`
			Source  string `json:"source"`
			Cookies []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"auth"`
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(savedRaw, &file); err != nil {
		t.Fatalf("persisted auth JSON: %v", err)
	}
	if file.Type != providerName {
		t.Fatalf("type = %q, want %q", file.Type, providerName)
	}
	if file.Auth.Lane != laneCookie || file.Auth.Source != "manual-paste" {
		t.Fatalf("lane/source wrong: lane=%q source=%q", file.Auth.Lane, file.Auth.Source)
	}
	if file.Auth.UID != "3839" || file.Account.UID != "3839" {
		t.Fatalf("uid = %q/%q, want 3839 (from the pasted userId row)", file.Auth.UID, file.Account.UID)
	}
	if file.Auth.Region != "sgp" || file.Auth.SID != "mimosgp" {
		t.Fatalf("region/sid stamp wrong: %q/%q", file.Auth.Region, file.Auth.SID)
	}
	hasRow := func(name, value string) bool {
		for _, c := range file.Auth.Cookies {
			if c.Name == name && c.Value == value {
				return true
			}
		}
		return false
	}
	if !hasRow("serviceToken", "st-new") {
		t.Fatal("minted serviceToken missing from persisted jar")
	}
	if !hasRow("passToken", "pt") {
		t.Fatal("bootstrap passToken must survive the merge")
	}
	if !hasRow("userId", "3839") {
		t.Fatal("bootstrap userId must survive the merge")
	}
	if !strings.Contains(page, "mimo-cookie-3839.json") {
		t.Fatal("page must name the saved credential file")
	}
}

// TestCookieSubmitIncompleteForm: a prose paste is rejected with the format
// hint and persists nothing.
func TestCookieSubmitIncompleteForm(t *testing.T) {
	persisted := false
	hostAuthPersistFn = func(name string, raw []byte) error {
		persisted = true
		return nil
	}
	defer func() { hostAuthPersistFn = hostAuthPersist }()

	form := url.Values{"cookies": {"我不知道要填什么"}, "region": {"cn"}}
	resp := handleMimoCookieSubmit(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/mimo/cookie_submit",
		Body:   []byte(form.Encode()),
	})
	page := string(resp)
	if !strings.Contains(page, "提交不完整") || !strings.Contains(page, "passToken") {
		t.Fatalf("incomplete paste must be rejected with the hint: %s", page)
	}
	if persisted {
		t.Fatal("nothing must persist on rejection")
	}
}

// TestCookieSubmitFormPage: GET renders the guided form (scenario, steps,
// region choices, security warning).
func TestCookieSubmitFormPage(t *testing.T) {
	resp := handleMimoCookieSubmit(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/mimo/cookie_submit",
	})
	page := string(resp)
	for _, want := range []string{"account.xiaomi.com", "passToken", "userId", "Application", "cookie_submit", "等同账号登录态"} {
		if !strings.Contains(page, want) {
			t.Errorf("form page missing %q", want)
		}
	}
}

// TestNormalizePasteRegion: the pin whitelist.
func TestNormalizePasteRegion(t *testing.T) {
	for raw, want := range map[string]string{"cn": "cn", "SGP": "sgp", "": "", "auto": "", "ru": ""} {
		if got := normalizePasteRegion(raw); got != want {
			t.Errorf("normalizePasteRegion(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCookieSubmitGETQueryProcessesPaste pins the v0.2.17 fetch-based lane:
// GET ?cookies=…&region=… processes the paste exactly like the POST form
// (no navigation required), and plain GET keeps rendering the combined page.
func TestCookieSubmitGETQueryProcessesPaste(t *testing.T) {
	srv := withPassportStub(t)
	swapServiceLoginBase(t, srv.URL)

	var savedName string
	hostAuthPersistFn = func(name string, raw []byte) error { savedName = name; return nil }
	defer func() { hostAuthPersistFn = hostAuthPersist }()

	page := string(handleMimoCookieSubmit(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/mimo/cookie_submit",
		Query:  url.Values{"cookies": {"passToken=pt; userId=3839; cUserId=cu"}, "region": {"sgp"}},
	}))
	if !strings.Contains(page, "粘贴登录完成") || savedName == "" {
		t.Fatalf("GET query paste not processed: page=%q saved=%q", page, savedName)
	}

	plain := string(handleMimoCookieSubmit(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/mimo/cookie_submit",
	}))
	if !strings.Contains(plain, "桌面会员") || !strings.Contains(plain, "cb_url") {
		t.Fatalf("plain GET no longer renders the combined page")
	}
}

// TestMimoPagesCarryPasteInterceptor pins the no-navigation guarantee: every
// rendered mimo page ships the fetch-based submit interceptor, and the
// interceptor derives its target paths from location.pathname (mount-prefix
// and trailing-slash immune) rather than any hardcoded /v0/resource shape.
func TestMimoPagesCarryPasteInterceptor(t *testing.T) {
	page := string(mimoSubmitPage("t", "<b>body</b>"))
	for _, marker := range []string{"addEventListener('submit'", "fetch(", "location.pathname", "cb_url", "cookies"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("interceptor missing marker %q", marker)
		}
	}
}
