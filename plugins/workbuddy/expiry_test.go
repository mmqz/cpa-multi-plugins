package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPreserveExpiry(t *testing.T) {
	old := time.Now().Add(time.Hour).Unix()
	if got := preserveExpiry(0, old); got != old {
		t.Fatalf("zero new expiry should keep old: got %d want %d", got, old)
	}
	newer := time.Now().Add(2 * time.Hour).Unix()
	if got := preserveExpiry(newer, old); got != newer {
		t.Fatalf("positive new expiry should win: got %d want %d", got, newer)
	}
}

// handleRefreshAuth must not zero out expiresAt when the upstream refresh
// response carries tokens but no expiresIn (preserveExpiry keeps the old
// value, and it must survive the storage round-trip into AuthData).
func TestRefreshKeepsExpiryWhenExpiresInMissing(t *testing.T) {
	oldExpiry := time.Now().Add(30 * time.Minute).Unix()
	storage := []byte(`{"auth":{"accessToken":"old-at","refreshToken":"rt","expiresAt":` +
		strconv.FormatInt(oldExpiry, 10) + `},"account":{"uid":"u1","nickname":"n"}}`)
	sa, err := parseStored(storage)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	sa.Auth.AccessToken = "new-at"
	sa.Auth.ExpiresAt = preserveExpiry(0, sa.Auth.ExpiresAt)
	out := toAuthData(sa)
	if !strings.Contains(string(out.StorageJSON), strconv.FormatInt(oldExpiry, 10)) {
		t.Fatalf("expiresAt lost in storage: %s", out.StorageJSON)
	}
}
