package main

// intl_pay_test.go — v0.12.72 (issue #29 field report, Xyloz3n 2026-10-03):
// intl accounts must ride the intl pay face (grow-normal.trae.ai +
// /trae/api/v1/pay/*) instead of the CN client (api.trae.cn + v2 →
// 401 code 4014 → "ent_usage: upstream session_dead"). The ent_usage
// fixture mirrors the field-captured 200 response (float usage amounts).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
	pluginapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestIsIntlStoredAuth(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want bool
	}{
		{"nil", nil, false},
		{"variant intl", &storedAuth{Variant: "intl"}, true},
		{"variant INTL case", &storedAuth{Variant: "INTL"}, true},
		{"variant cn", &storedAuth{Variant: "cn"}, false},
		{"variant solo", &storedAuth{Variant: "solo"}, false},
		{"empty variant trae.ai domain", &storedAuth{Auth: storedTokens{Domain: "trae.ai"}}, true},
		{"empty variant marscode", &storedAuth{Auth: storedTokens{Domain: "api.marscode.com"}}, false},
		{"empty variant api.trae.cn", &storedAuth{Auth: storedTokens{Domain: "api.trae.cn"}}, false},
	}
	for _, tc := range cases {
		if got := isIntlStoredAuth(tc.sa); got != tc.want {
			t.Errorf("%s: isIntlStoredAuth=%v want %v", tc.name, got, tc.want)
		}
	}
}

// intlEntUsageFixture is the field-verified 200 payload (Free plan, float
// usage). advanced_model_request_limit is the metered dimension on intl
// Free packs (basic_usage_limit absent) — the conversion maps it in.
const intlEntUsageFixture = `{
  "user_entitlement_pack_list": [{
    "display_desc": "Free plan",
    "entitlement_base_info": {
      "quota": {
        "advanced_model_request_limit": 1000,
        "auto_completion_limit": 5000,
        "premium_model_fast_request_limit": 10,
        "premium_model_slow_request_limit": 50,
        "credits_limit": 0,
        "enable_solo_agent": true,
        "solo_agent_parallel_limit": 2
      },
      "start_time": 1790812800,
      "end_time": 1793491199
    },
    "usage": {
      "basic_usage_amount": 0.2147,
      "bonus_usage_amount": 0,
      "credits_amount": 0,
      "is_flash_consuming": true
    }
  }]
}`

func TestRefreshAccountCredits_IntlRoutesToIntlPayFace(t *testing.T) {
	var gotPath, gotAuth, gotRegion, gotDevice, gotUA string
	pay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("pay body = %q, want {}", body)
		}
		switch r.URL.Path {
		case "/trae/api/v1/pay/ide_user_ent_usage":
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotRegion = r.Header.Get("X-User-Region")
			gotDevice = r.Header.Get("X-Device-Id")
			gotUA = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(intlEntUsageFixture))
		case "/trae/api/v1/pay/ide_user_pay_status":
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			t.Errorf("unexpected intl pay path %s", r.URL.Path)
		}
	}))
	defer pay.Close()

	oldBase := intlPayBase
	intlPayBase = pay.URL
	defer func() { intlPayBase = oldBase }()

	// Legacy-shaped intl auth file: device id only under boundDeviceId.
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:   "test-jwt",
			Domain:        "trae.ai",
			Variant:       "intl",
			Region:        "US-East",
			BoundDeviceID: "dev-bound-123",
		},
		Account: storedAccount{UID: "uid-intl-1", Nickname: "Intl"},
	}
	f := pluginapi.HostAuthFileEntry{AuthIndex: "test-intl-1"}

	entry := refreshAccountCredits(f, sa, hostAuthAsUpstream(sa))

	if gotPath != "/trae/api/v1/pay/ide_user_ent_usage" {
		t.Fatalf("intl pay face not used, last path = %q", gotPath)
	}
	if gotAuth != "Cloud-IDE-JWT test-jwt" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotRegion != "US-East" {
		t.Errorf("X-User-Region = %q, want US-East", gotRegion)
	}
	if gotDevice != "dev-bound-123" {
		t.Errorf("X-Device-Id = %q, want boundDeviceId fallback", gotDevice)
	}
	if gotUA != intlPayUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, intlPayUserAgent)
	}
	if errStr, ok := entry["error"]; ok {
		t.Fatalf("unexpected error: %v", errStr)
	}
	if entry["variant"] != "intl" {
		t.Errorf("variant = %v, want intl", entry["variant"])
	}
	if entry["remain_known"] != true {
		t.Fatalf("remain_known = %v, want true (advanced_model_request_limit mapped in)", entry["remain_known"])
	}
	if remain, ok := entry["total_remain"].(int64); !ok || remain != 1000 {
		t.Errorf("total_remain = %v (%T), want int64(1000)", entry["total_remain"], entry["total_remain"])
	}
	if entry["plan"] != "Free plan" {
		t.Errorf("plan = %v, want Free plan", entry["plan"])
	}
	if _, ok := entry["checked_in"]; ok {
		t.Errorf("intl entry must not carry check-in fields")
	}
	if _, ok := entry["checkin_status_error"]; ok {
		t.Errorf("intl entry must not attempt the CN check-in face")
	}
}

func TestRefreshAccountCredits_CNKeepsCNFace(t *testing.T) {
	intlHits := 0
	pay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		intlHits++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer pay.Close()

	oldBase := intlPayBase
	intlPayBase = pay.URL
	defer func() { intlPayBase = oldBase }()

	if upstreamClient == nil {
		// upstreamClient is initialized in cliproxy_plugin_init (cgo entry),
		// which never runs in the test binary — create one for the routing probe.
		upstreamClient = upstream.New()
	}
	oldUg := upstreamClient.UgHost
	upstreamClient.UgHost = "http://127.0.0.1:1" // CN face unreachable on purpose
	defer func() { upstreamClient.UgHost = oldUg }()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "cn-jwt", Domain: "api.trae.cn", Variant: "cn"},
		Account: storedAccount{UID: "uid-cn-1"},
	}
	f := pluginapi.HostAuthFileEntry{AuthIndex: "test-cn-1"}

	entry := refreshAccountCredits(f, sa, hostAuthAsUpstream(sa))

	if intlHits != 0 {
		t.Fatalf("CN account must not touch the intl pay face (%d hits)", intlHits)
	}
	errStr, _ := entry["error"].(string)
	if errStr == "" {
		t.Fatalf("expected CN ent_usage error (unreachable host), got entry %v", entry)
	}
	if len(errStr) > 0 && (errStr[:4] == "intl") {
		t.Errorf("CN failure mislabeled as intl: %q", errStr)
	}
}

func TestIntlEntUsage_EnvelopeTolerant(t *testing.T) {
	// Flat payload (field shape).
	flat, err := intlEntUsageDecode([]byte(intlEntUsageFixture))
	if err != nil {
		t.Fatalf("flat decode: %v", err)
	}
	if len(flat.UserEntitlementPackList) != 1 {
		t.Fatalf("flat packs = %d, want 1", len(flat.UserEntitlementPackList))
	}
	// Enveloped payload {"code":0,"data":{...}}.
	enveloped, err := intlEntUsageDecode([]byte(`{"code":0,"msg":"ok","data":` + intlEntUsageFixture + `}`))
	if err != nil {
		t.Fatalf("enveloped decode: %v", err)
	}
	if len(enveloped.UserEntitlementPackList) != 1 {
		t.Fatalf("enveloped packs = %d, want 1", len(enveloped.UserEntitlementPackList))
	}
	// Non-zero business code → error.
	if _, err := intlEntUsageDecode([]byte(`{"code":4014,"msg":"denied","data":{}}`)); err == nil {
		t.Fatal("expected business-code error")
	}
}

func TestIntlPackConversion_FloatRounding(t *testing.T) {
	var parsed intlEntUsageResult
	if err := json.Unmarshal([]byte(intlEntUsageFixture), &parsed); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	cn := parsed.toCNResult()
	if len(cn.UserEntitlementPackList) != 1 {
		t.Fatalf("packs = %d", len(cn.UserEntitlementPackList))
	}
	p := cn.UserEntitlementPackList[0]
	q := p.EffectiveQuota()
	if q.BasicUsageLimit == nil || *q.BasicUsageLimit != 1000 {
		t.Fatalf("BasicUsageLimit = %v, want 1000 (advanced_model_request_limit fallback)", q.BasicUsageLimit)
	}
	if p.Usage.BasicUsageAmount == nil || *p.Usage.BasicUsageAmount != 0 {
		t.Fatalf("BasicUsageAmount = %v, want 0 (0.2147 rounded)", p.Usage.BasicUsageAmount)
	}
	remain, ok := p.PackRemain()
	if !ok || remain != 1000 {
		t.Errorf("PackRemain = %d ok=%v, want 1000", remain, ok)
	}
}

func TestHostAuthAsUpstream_DeviceIDFallback(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{DeviceID: "primary", BoundDeviceID: "bound"}}
	if got := hostAuthAsUpstream(sa).DeviceID; got != "primary" {
		t.Errorf("DeviceID = %q, want primary", got)
	}
	sa2 := &storedAuth{Auth: storedTokens{BoundDeviceID: "bound-only"}}
	if got := hostAuthAsUpstream(sa2).DeviceID; got != "bound-only" {
		t.Errorf("DeviceID = %q, want bound-only (fallback)", got)
	}
}
