// headers_test.go — v0.12.45 ug 抓包指纹头回归锁。
// 证据：dsh-router-traework ugHeaders（2026-09-03 抓包成功签到请求逐头复刻）
// + 用户账号 37396015402 实测（旧头多日 9074，新头当日 claim code=0）。
// 锁死要点：VSCode 插件进程 UA、无 Origin/Referer/x-app-type、Accept=*/*、
// per-request X-Request-Id/X-TT-Trace-Id、DeviceID 透传。
package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

func TestUgUserAgentByVariant(t *testing.T) {
	// solo（TRAE SOLO CN）是抓包实证值；其余按 PlatformNameFor 同构映射。
	cases := map[string]string{
		"solo":      "VSCode 1.107.1 (TRAE SOLO CN)",
		"cn":        "VSCode 1.107.1 (TRAE CN)",
		"intl":      "VSCode 1.107.1 (TRAE)",
		"solo-intl": "VSCode 1.107.1 (TRAE SOLO)",
		"":          "VSCode 1.107.1 (TRAE CN)", // 未知谱系回退 cn
	}
	for variant, want := range cases {
		if got := ugUserAgentFor(variant); got != want {
			t.Errorf("ugUserAgentFor(%q) = %q, want %q", variant, got, want)
		}
	}
}

func TestUgPackageTypeByVariant(t *testing.T) {
	if got := ugPackageTypeFor("solo"); got != "stable_cn" {
		t.Errorf("solo package-type = %q, want stable_cn", got)
	}
	if got := ugPackageTypeFor("solo-intl"); got != "stable" {
		t.Errorf("solo-intl package-type = %q, want stable", got)
	}
}

func TestUgBaseHeadersCapturedFingerprint(t *testing.T) {
	a := &auth.Auth{AccessToken: "tok", DeviceID: "1711320556112436", Variant: "solo"}
	req, _ := http.NewRequest(http.MethodPost, "https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim", nil)
	ugBaseHeaders(req, a)

	// 抓包指纹：VSCode 插件进程身份
	if got := req.Header.Get("User-Agent"); got != "VSCode 1.107.1 (TRAE SOLO CN)" {
		t.Errorf("UA = %q, want VSCode plugin-process identity", got)
	}
	if got := req.Header.Get("Accept"); got != "*/*" {
		t.Errorf("Accept = %q, want */* (captured value)", got)
	}
	if got := req.Header.Get("X-Market-Client-Id"); got != "VSCode 1.107.1" {
		t.Errorf("X-Market-Client-Id = %q", got)
	}
	if got := req.Header.Get("Package-Type"); got != "stable_cn" {
		t.Errorf("Package-Type = %q", got)
	}
	if got := req.Header.Get("App-Version"); got != "0.1.61" {
		t.Errorf("App-Version = %q, want 0.1.61 (captured; not the 2.3.81345 desktop-shell value)", got)
	}
	if got := req.Header.Get("X-Device-Id"); got != "1711320556112436" {
		t.Errorf("X-Device-Id = %q, want device passthrough", got)
	}

	// 旧 WebView 画像必须消失（真实客户端 ug 请求不携带）
	for _, gone := range []string{"Origin", "Referer", "X-App-Type"} {
		if got := req.Header.Get(gone); got != "" {
			t.Errorf("%s = %q, must be absent (captured request has none)", gone, got)
		}
	}

	// per-request trace：uuid-v4 + tt-trace 格式
	rid := req.Header.Get("X-Request-Id")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(rid) {
		t.Errorf("X-Request-Id = %q, want uuid-v4", rid)
	}
	trace := req.Header.Get("X-TT-Trace-Id")
	if !strings.HasPrefix(trace, "00-") || !strings.HasSuffix(trace, "-01") {
		t.Errorf("X-TT-Trace-Id = %q, want 00-<32hex>-<16hex>-01", trace)
	}
	if want := "00-" + trace[3:35] + "-" + strings.ReplaceAll(rid, "-", "")[:16] + "-01"; trace != want {
		t.Errorf("X-TT-Trace-Id = %q, span must derive from request id (%q)", trace, want)
	}

	// 每次调用生成新 trace（不能是包级常量）
	req2, _ := http.NewRequest(http.MethodPost, "https://api.trae.cn/x", nil)
	ugBaseHeaders(req2, a)
	if req2.Header.Get("X-Request-Id") == rid {
		t.Errorf("X-Request-Id repeated across requests: %q", rid)
	}
}

func TestUgCheckinRequestCarriesCapturedIdentity(t *testing.T) {
	// 签到路径端到端：ugCheckinRequest 必须以 VSCode 插件进程身份发 claim，
	// 且 Bearer 回退方案也共享同一身份（仅 Authorization 不同）。
	a := &auth.Auth{AccessToken: "tok", DeviceID: "1711320556112436", Variant: "solo"}
	for _, scheme := range ugCheckinSchemes() {
		req, err := ugCheckinRequest(a, http.MethodPost, "https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim", `{}`, scheme, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("User-Agent"); got != "VSCode 1.107.1 (TRAE SOLO CN)" {
			t.Errorf("scheme %s: UA = %q", scheme, got)
		}
		wantAuth := "Cloud-IDE-JWT tok"
		if scheme == UgSchemeBearer {
			wantAuth = "Bearer tok"
		}
		if got := req.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("scheme %s: Authorization = %q, want %q", scheme, got, wantAuth)
		}
		// v0.12.40 的桌面壳设备头已废弃（下沉为抓包指纹）
		if got := req.Header.Get("X-App-Version"); got != "" {
			t.Errorf("scheme %s: X-App-Version = %q, must be absent (App-Version is the captured header)", scheme, got)
		}
	}
}
