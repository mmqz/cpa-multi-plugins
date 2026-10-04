package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// TestPlatformIDForMatrix locks the v0.12.44 2×2 lineage matrix against
// cockpit-tools (trae_account_core_platform_storage.rs:196-199 + a live
// cockpit export where platformId=trae_solo_cn for a TRAE SOLO CN account).
func TestPlatformIDForMatrix(t *testing.T) {
	cases := []struct {
		variant  string
		platID   string
		platName string
		clientID string
		solo     bool
		intl     bool
	}{
		{"cn", "trae_cn", "TRAE CN", "ono9krqynydwx5", false, false},
		{"solo", "trae_solo_cn", "TRAE SOLO CN", "en1oxy7wnw8j9n", true, false},
		{"intl", "trae", "TRAE", "ono9krqynydwx5", false, true},
		{"solo-intl", "trae_solo_intl", "TRAE SOLO", "en1oxy7wnw8j9n", true, true},
		{"", "trae_cn", "TRAE CN", "ono9krqynydwx5", false, false}, // default lineage
	}
	for _, c := range cases {
		if got := PlatformIDFor(c.variant); got != c.platID {
			t.Errorf("PlatformIDFor(%q)=%q want %q", c.variant, got, c.platID)
		}
		if got := PlatformNameFor(c.variant); got != c.platName {
			t.Errorf("PlatformNameFor(%q)=%q want %q", c.variant, got, c.platName)
		}
		if got := ClientIDFor(c.variant); got != c.clientID {
			t.Errorf("ClientIDFor(%q)=%q want %q", c.variant, got, c.clientID)
		}
		if got := IsSoloVariant(c.variant); got != c.solo {
			t.Errorf("IsSoloVariant(%q)=%v want %v", c.variant, got, c.solo)
		}
		if got := IsIntlVariant(c.variant); got != c.intl {
			t.Errorf("IsIntlVariant(%q)=%v want %v", c.variant, got, c.intl)
		}
	}
}

// TestCheckLoginRequestAndParse verifies the CheckLogin wire contract
// (POST /cloudide/api/v3/trae/CheckLogin, X-Cloudide-Token scheme, body
// {"IDEVersion":...}) and the flattened parse of the response shape the
// official client stores as trae_server_raw (live-verified via a cockpit
// import export 2026-09-06).
func TestCheckLoginRequestAndParse(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	resp := `{"AIRegion":"CN","ResponseMetadata":{"Service":"","TraceID":"t"},"Result":{"AIHost":"","AIPayHost":"","AIRegion":"","BoundDeviceID":"e4w2k6llxjrky2","DeviceBindStatus":"BOUND","ExpiredAt":1804206273052,"Host":"https://api.trae.com.cn","IsLogin":true,"MigrateToSG":false,"NickNameEditStatus":"","PasswordChanged":false,"Region":"CN","UserID":"1237380756941412"},"authClientId":"en1oxy7wnw8j9n","host":"https://api.trae.cn","loginHost":"https://api.trae.cn","loginRegion":"cn","storeRegion":"CN"}`
	fn := rtFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("X-Cloudide-Token")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, resp), nil
	})
	c := testClient(fn)
	a := &auth.Auth{AccessToken: "tok-1", APIHost: "https://api.example"}

	out, err := c.CheckLogin(a)
	if err != nil {
		t.Fatalf("CheckLogin: %v", err)
	}
	if gotPath != EpCheckLogin {
		t.Errorf("path=%q want %q", gotPath, EpCheckLogin)
	}
	if gotAuth != "tok-1" {
		t.Errorf("X-Cloudide-Token=%q want tok-1", gotAuth)
	}
	if !strings.Contains(gotBody, "IDEVersion") {
		t.Errorf("body missing IDEVersion: %q", gotBody)
	}
	if !out.IsLogin || out.BoundDeviceID != "e4w2k6llxjrky2" || out.DeviceBindStatus != "BOUND" {
		t.Errorf("bind fields wrong: %+v", out)
	}
	if out.Host != "https://api.trae.com.cn" || out.Region != "CN" || out.AIRegion != "CN" || out.UserID != "1237380756941412" {
		t.Errorf("region/host fields wrong: %+v", out)
	}
	if out.ExpiredAt != 1804206273052 {
		t.Errorf("ExpiredAt=%d want 1804206273052 (ms epoch preserved)", out.ExpiredAt)
	}
	if len(out.Raw) == 0 || !strings.Contains(string(out.Raw), "DeviceBindStatus") {
		t.Errorf("Raw envelope not preserved")
	}
}

// TestCheckLoginMultiHostFallback: first candidate host fails, second answers.
func TestCheckLoginMultiHostFallback(t *testing.T) {
	calls := 0
	fn := rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResp(500, `boom`), nil
		}
		return jsonResp(200, `{"Result":{"IsLogin":true,"BoundDeviceID":"d-1","DeviceBindStatus":"BOUND"}}`), nil
	})
	c := testClient(fn)
	out, err := c.CheckLogin(&auth.Auth{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("CheckLogin fallback: %v", err)
	}
	if calls != 2 || out.BoundDeviceID != "d-1" {
		t.Errorf("calls=%d out=%+v — expected success on 2nd host", calls, out)
	}
}
