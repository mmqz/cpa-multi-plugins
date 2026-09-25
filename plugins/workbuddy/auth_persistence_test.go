package main

import (
	"encoding/json"
	"testing"
)

// --- normalizeWorkbuddyAuthDoc: attribution stamps, everything else survives ---

func TestNormalizeWorkbuddyAuthDocClassifiesAndPreserves(t *testing.T) {
	for _, raw := range []string{
		// Nested plugin shape with panel-managed extras.
		`{"auth":{"accessToken":"a","domain":"copilot.tencent.com"},"account":{"uid":"u","enterpriseId":"e"},"disabled":true,"note":"keep","priority":7,"prefix":"team","custom":{"id":9007199254740993}}`,
		// Legacy flat shape with pre-merge family type declarations.
		`{"type":"codebuddy-cn","provider":"workbuddy-cn","accessToken":"a","disabled":true,"note":"keep","priority":7,"prefix":"team","custom":{"id":9007199254740993}}`,
	} {
		doc, err := normalizeWorkbuddyAuthDoc([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var before map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &before)
		for _, key := range []string{"disabled", "note", "priority", "prefix", "custom", "account", "auth"} {
			if want, ok := before[key]; ok && string(doc[key]) != string(want) {
				t.Fatalf("changed %s: %s -> %s", key, want, doc[key])
			}
		}
		if string(doc["auth_kind"]) != `"oauth"` || string(doc["type"]) != `"workbuddy"` || string(doc["provider"]) != `"workbuddy"` {
			t.Fatalf("classification missing: %v/%v/%v", doc["type"], doc["provider"], doc["auth_kind"])
		}
	}
	for _, raw := range []string{`null`, `[]`, `not-json{`, `{"type":"qoder"}`, `{"provider":"codex"}`, `{"type":42}`} {
		if _, err := normalizeWorkbuddyAuthDoc([]byte(raw)); err == nil {
			t.Fatalf("unsafe object accepted: %s", raw)
		}
	}
}

// The refresh pipeline (mergeStoredAuthIntoDoc -> hostAuthSaveJSON) must
// deliver a document whose classification survives the save gate while the
// merge boundary itself stays unchanged: owned auth/account objects replaced
// wholesale, unknown top-level panel fields preserved.
func TestWorkbuddySavePipelineClassifiesOAuth(t *testing.T) {
	existing := `{
                "auth": {"accessToken": "old-at", "refreshToken": "old-rt", "expiresAt": 1},
                "account": {"uid": "old-uid", "nickname": "old-nick"},
                "proxy_url": "http://127.0.0.1:7890",
                "logo": "data:image/png;base64,AAAA",
                "disabled": true,
                "note": "Session dead (12153): re-login required"
        }`
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 999, Domain: "copilot.tencent.com", Region: "cn"},
		Account: storedAccount{UID: "new-uid", Nickname: "new-nick"},
	}
	merged, err := mergeStoredAuthIntoDoc([]byte(existing), sa)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	doc, err := normalizeWorkbuddyAuthDoc(merged)
	if err != nil {
		t.Fatalf("save gate rejected the merged document: %v", err)
	}
	var final map[string]any
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &final); err != nil {
		t.Fatal(err)
	}
	if final["auth_kind"] != "oauth" || final["type"] != "workbuddy" {
		t.Fatalf("pipeline classification = %v/%v", final["auth_kind"], final["type"])
	}
	auth, _ := final["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "new-at" {
		t.Fatalf("auth not refreshed through the pipeline: %v", final["auth"])
	}
	for key, want := range map[string]any{
		"proxy_url": "http://127.0.0.1:7890",
		"disabled":  true,
		"note":      "Session dead (12153): re-login required",
	} {
		if final[key] != want {
			t.Fatalf("pipeline wiped top-level %q: got %v want %v", key, final[key], want)
		}
	}
	// An unreadable refresh base is an error at the gate too.
	if _, err := normalizeWorkbuddyAuthDoc([]byte("not-json{")); err == nil {
		t.Fatal("unreadable document accepted")
	}
}

// --- runtime metadata chain carries the same classification ---

func TestWorkbuddyRuntimeMetadataClassifiesOAuth(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u-1", Nickname: "n"},
	}
	if md := toAuthData(sa).Metadata; md["auth_kind"] != "oauth" {
		t.Fatalf("runtime OAuth classification missing: %v", md["auth_kind"])
	}
	if md := toAuthDataForRefresh(sa).Metadata; md["auth_kind"] != "oauth" {
		t.Fatalf("refresh-path OAuth classification missing: %v", md["auth_kind"])
	}
	raw, err := buildAuthFileJSON(sa, true, "keep note", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["auth_kind"] != "oauth" || doc["type"] != providerName {
		t.Fatalf("lifecycle save classification = %v/%v", doc["auth_kind"], doc["type"])
	}
	if doc["disabled"] != true || doc["note"] != "keep note" {
		t.Fatal("credential settings changed")
	}
}
