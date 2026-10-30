package main

// host_owned_fields_test.go — issue #68: the auth.parse response must carry
// the host-owned operator fields (proxy_url above all) so the host's
// in-memory record survives restarts and its persist path keeps the keys in
// the file.

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestApplyHostOwnedAuthFields(t *testing.T) {
	doc := []byte(`{
  "type": "workbuddy",
  "proxy_url": " http://127.0.0.1:7890 ",
  "prefix": "teamA",
  "priority": 7,
  "weight": 2.5,
  "request_retry": 3,
  "label": "ops-1",
  "headers": {"X-Ops": "on"},
  "auth": {"access_token": "at"},
  "account": {"uid": "u1"}
}`)
	ad := &pluginapi.AuthData{Provider: "workbuddy"}
	applyHostOwnedAuthFields(ad, doc)

	if ad.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy_url must ride the AuthData field (the host applies it to the in-memory record): got %q", ad.ProxyURL)
	}
	if ad.Prefix != "teamA" {
		t.Fatalf("prefix must ride the AuthData field: got %q", ad.Prefix)
	}
	for _, k := range []string{"proxy_url", "prefix", "priority", "weight", "request_retry", "headers", "label"} {
		if _, ok := ad.Metadata[k]; !ok {
			t.Errorf("host-owned key %q must be re-injected into Metadata (the persist path writes from there)", k)
		}
	}
	if got, _ := ad.Metadata["priority"].(float64); got != 7 {
		t.Errorf("priority must survive as a number, got %v", ad.Metadata["priority"])
	}
	if _, ok := ad.Metadata["auth"]; ok {
		t.Errorf("non-whitelisted keys must not be copied into Metadata")
	}

	// Absent keys stay absent; the helper never invents values.
	bare := &pluginapi.AuthData{Provider: "workbuddy"}
	applyHostOwnedAuthFields(bare, []byte(`{"type":"workbuddy","auth":{"access_token":"at"}}`))
	if bare.ProxyURL != "" || bare.Prefix != "" || len(bare.Metadata) != 0 {
		t.Fatalf("no host-owned keys in the document: nothing should be injected, got %+v", bare)
	}

	// Malformed payload: untouched, never an error path.
	applyHostOwnedAuthFields(bare, []byte("not json"))
}

func TestHandleParseAuthCarriesHostOwnedFields(t *testing.T) {
	uid := "e2e68aaaaaaaa-0000-0000-0000-000000000001"
	doc := []byte(`{
  "type": "workbuddy",
  "proxy_url": "http://10.0.0.8:7890",
  "prefix": "teamA",
  "priority": 7,
  "auth": {
    "accessToken": "at",
    "refreshToken": "rt",
    "domain": "www.codebuddy.cn",
    "expiresAt": 9999999999
  },
  "account": {
    "uid": "` + uid + `",
    "nickname": "nick"
  }
}`)
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/auths/workbuddy-" + uid + ".json",
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  doc,
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("expected handled")
	}
	if resp.Auth.ProxyURL != "http://10.0.0.8:7890" {
		t.Fatalf("the parse response must carry the operator proxy (issue #68): got %q", resp.Auth.ProxyURL)
	}
	if resp.Auth.Prefix != "teamA" {
		t.Fatalf("the parse response must carry the operator prefix: got %q", resp.Auth.Prefix)
	}
	if resp.Auth.Metadata["priority"].(float64) != 7 {
		t.Fatalf("priority must ride Metadata for the persist path: %v", resp.Auth.Metadata["priority"])
	}
}
