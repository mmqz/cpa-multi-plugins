package main

// Parse ownership guard regression (repo v0.12.9): the host rewrites an
// empty req.Provider to the POLLED plugin's own identifier before calling
// auth.parse, so req.Provider can never prove ownership for type-less
// files. Guards must decide on (declared type, filename family) only.

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func parseHandled(t *testing.T, provider, fileName string, body []byte) bool {
	t.Helper()
	req := pluginapi.AuthParseRequest{Provider: provider, FileName: fileName, RawJSON: body}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := handleParseAuth(raw)
	if err != nil {
		t.Fatalf("handleParseAuth: %v", err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(resp, &env)
	var out pluginapi.AuthParseResponse
	_ = json.Unmarshal(env.Result, &out)
	return out.Handled
}

func TestParseOwnershipGuard(t *testing.T) {
	typeless := []byte(`{"auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u","nickname":"N"}}`)
	qoderTyped := []byte(`{"type":"qoder","auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u"}}`)
	wbTyped := []byte(`{"type":"workbuddy","auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u"}}`)
	legacyTyped := []byte(`{"type":"qoderwork","auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u"}}`)
	traeTyped := []byte(`{"type":"trae","auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u"}}`)

	cases := []struct {
		name     string
		provider string // what the host stuffed into req.Provider (its own id when empty)
		fileName string
		body     []byte
		want     bool
	}{
		{"typeless + our prefix → claim (host stuffed our id, filename decides)", "qoder", "qoder-uid1.json", typeless, true},
		{"typeless + legacy exact name → claim", "qoder", "qoderwork.json", typeless, true},
		{"typeless + legacy family prefix → claim", "qoder", "qoder-cn-uid1.json", typeless, true},
		{"typeless + foreign prefix → reject (was the bug: stolen from workbuddy)", "qoder", "workbuddy-uid1.json", typeless, false},
		{"typeless + generic name → reject (orphan, not stolen)", "qoder", "generic-cred.json", typeless, false},
		{"declared own type → claim", "qoder", "any-name.json", qoderTyped, true},
		{"declared legacy family type → claim", "qoder", "any-name.json", legacyTyped, true},
		{"declared foreign type → reject", "qoder", "qoder-uid1.json", wbTyped, false},
		{"declared foreign type trae → reject", "qoder", "qoder-uid1.json", traeTyped, false},
	}
	for _, tc := range cases {
		if got := parseHandled(t, tc.provider, tc.fileName, tc.body); got != tc.want {
			t.Errorf("%s: handled=%v, want %v", tc.name, got, tc.want)
		}
	}
}
