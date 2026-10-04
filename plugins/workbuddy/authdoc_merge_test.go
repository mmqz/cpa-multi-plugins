package main

import (
	"encoding/json"
	"testing"
)

// TestMergeStoredAuthIntoDocPreservesUnknownFields pins the v0.9.27 fix
// (adapted from PR #6 / 4d5533e): persistAuthTokens must not rebuild the
// on-disk document from the storedAuth struct alone. Panel-managed keys
// (proxy_url, logo) and lifecycle markers written by markSessionDead
// (disabled, note) must survive a token refresh; only auth/account are
// overwritten with the refreshed values.
func TestMergeStoredAuthIntoDocPreservesUnknownFields(t *testing.T) {
	existing := `{
                "auth": {"accessToken": "old-at", "refreshToken": "old-rt", "expiresAt": 1},
                "account": {"uid": "old-uid", "enterpriseId": "old-eid", "nickname": "old-nick"},
                "proxy_url": "http://127.0.0.1:7890",
                "logo": "data:image/png;base64,AAAA",
                "disabled": true,
                "note": "Session dead (12153): re-login required"
        }`
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  "new-at",
			RefreshToken: "new-rt",
			ExpiresAt:    999,
			Domain:       "copilot.tencent.com",
			Region:       "cn",
		},
		Account: storedAccount{UID: "new-uid", EnterpriseID: "new-eid", Nickname: "new-nick"},
	}
	raw, err := mergeStoredAuthIntoDoc([]byte(existing), sa)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("merged doc not JSON: %v", err)
	}

	// Refreshed values win inside the owned objects.
	auth, _ := doc["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "new-at" || auth["refreshToken"] != "new-rt" {
		t.Fatalf("auth not refreshed: %v", doc["auth"])
	}
	acc, _ := doc["account"].(map[string]any)
	if acc == nil || acc["uid"] != "new-uid" || acc["nickname"] != "new-nick" {
		t.Fatalf("account not refreshed: %v", doc["account"])
	}

	// Unknown top-level fields survive verbatim.
	for key, want := range map[string]any{
		"proxy_url": "http://127.0.0.1:7890",
		"logo":      "data:image/png;base64,AAAA",
		"disabled":  true,
		"note":      "Session dead (12153): re-login required",
	} {
		if got := doc[key]; got != want {
			t.Fatalf("top-level %q wiped: got %v want %v", key, got, want)
		}
	}
}

// TestMergeStoredAuthIntoDocEmptyAndBadDocuments covers the two edge
// documents: an empty phys.JSON builds a fresh doc, and an unreadable one is
// an error (silently discarding it would reintroduce the wipeout).
func TestMergeStoredAuthIntoDocEmptyAndBadDocuments(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 7},
		Account: storedAccount{UID: "u1"},
	}

	raw, err := mergeStoredAuthIntoDoc(nil, sa)
	if err != nil {
		t.Fatalf("empty doc merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("doc not JSON: %v", err)
	}
	auth, _ := doc["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "at" {
		t.Fatalf("fresh doc missing refreshed auth: %v", doc["auth"])
	}

	if _, err := mergeStoredAuthIntoDoc([]byte("not-json{"), sa); err == nil {
		t.Fatalf("bad phys.JSON accepted: want error, got nil")
	}
}

// TestMergeStoredAuthIntoDocOwnedObjectReplace pins the deliberate boundary
// of the merge: the overwrite is TOP-LEVEL key-level — unknown top-level
// fields survive, but the owned auth/account objects are replaced wholesale
// with the refreshed values. Object-internal unmodeled keys (e.g. a stale
// deviceId from another tool) are intentionally not preserved: the plugin
// owns these two objects and must keep them exactly in sync with its own
// struct, otherwise stale sub-fields could contradict the refreshed tokens.
func TestMergeStoredAuthIntoDocOwnedObjectReplace(t *testing.T) {
	existing := `{"auth": {"accessToken": "old", "deviceId": "dev-42"}, "account": {"uid": "old"}}`
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "new", RefreshToken: "rt"},
		Account: storedAccount{UID: "new"},
	}
	raw, err := mergeStoredAuthIntoDoc([]byte(existing), sa)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("doc not JSON: %v", err)
	}
	auth, _ := doc["auth"].(map[string]any)
	if auth == nil {
		t.Fatalf("auth object lost: %v", doc["auth"])
	}
	if auth["accessToken"] != "new" {
		t.Fatalf("accessToken not refreshed: %v", auth["accessToken"])
	}
	if _, still := auth["deviceId"]; still {
		t.Fatalf("owned auth object must be replaced wholesale (stale sub-fields must not survive): %v", auth)
	}
}
