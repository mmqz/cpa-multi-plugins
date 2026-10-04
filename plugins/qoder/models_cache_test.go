package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestModelCatalogAccountKeyScopesByCredential pins the v0.8.17 fix (fork
// libo0118/qoder-custom review): the model-catalog cache key must differ
// across credentials (a plan-specific offer must not cross accounts) and
// across regions sharing a token prefix. The token itself never appears in
// the key.
func TestModelCatalogAccountKeyScopesByCredential(t *testing.T) {
	a := &storedAuth{Auth: storedTokens{AccessToken: "token-A", Region: "cn"}}
	a2 := &storedAuth{Auth: storedTokens{AccessToken: "token-A", Region: "cn"}}
	b := &storedAuth{Auth: storedTokens{AccessToken: "token-B", Region: "cn"}}
	intl := &storedAuth{Auth: storedTokens{AccessToken: "token-A", Region: "intl"}}

	if modelCatalogAccountKey(a) != modelCatalogAccountKey(a2) {
		t.Fatalf("same credential must map to the same key")
	}
	if modelCatalogAccountKey(a) == modelCatalogAccountKey(b) {
		t.Fatalf("different tokens must map to different keys")
	}
	if modelCatalogAccountKey(a) == modelCatalogAccountKey(intl) {
		t.Fatalf("same token across regions must map to different keys")
	}
	if key := modelCatalogAccountKey(a); containsSubstring(key, "token-A") {
		t.Fatalf("key must not embed the raw token: %q", key)
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestDynamicModelsCacheScopedToAccount exercises the cache contract: a
// stored entry is returned only for the credential that produced it, and a
// foreign key misses even inside the TTL.
func TestDynamicModelsCacheScopedToAccount(t *testing.T) {
	keyA := modelCatalogAccountKey(&storedAuth{Auth: storedTokens{AccessToken: "tok-A"}})
	keyB := modelCatalogAccountKey(&storedAuth{Auth: storedTokens{AccessToken: "tok-B"}})

	storeDynamicModels(keyA, []pluginapi.ModelInfo{{ID: "from-A"}})
	if got, ok := cachedDynamicModels(keyA); !ok || len(got) != 1 || got[0].ID != "from-A" {
		t.Fatalf("own key must hit: ok=%v len=%d", ok, len(got))
	}
	if _, ok := cachedDynamicModels(keyB); ok {
		t.Fatalf("foreign key must MISS even inside TTL (cross-account catalog leak)")
	}

	// Re-store under B; A must now miss (single-entry cache follows the key).
	storeDynamicModels(keyB, []pluginapi.ModelInfo{{ID: "from-B"}})
	if _, ok := cachedDynamicModels(keyA); ok {
		t.Fatalf("A must miss after B stored over the slot")
	}
	if got, ok := cachedDynamicModels(keyB); !ok || got[0].ID != "from-B" {
		t.Fatalf("B must hit after re-store")
	}
}
