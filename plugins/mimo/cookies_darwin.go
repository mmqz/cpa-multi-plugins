//go:build darwin

// cookies_darwin.go — macOS os_crypt keys live in the login Keychain
// ("<app> Safe Storage"). Non-interactive keychain access from a plugin
// process is not something M1 will attempt (it prompts or fails depending on
// Keychain ACL state), so cookie adoption is explicitly unsupported on
// darwin: use the sk lane, which needs no desktop install at all.
package main

import "fmt"

func decryptPlatformV10(payload []byte, userDataDir, version string) ([]byte, error) {
	return nil, fmt.Errorf("macOS Chromium cookie decryption requires Keychain access — unsupported; use the sk lane (platform OAuth)")
}
