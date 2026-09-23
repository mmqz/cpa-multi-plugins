//go:build windows

// cookies_windows.go — Windows os_crypt: v10 cookies are AES-256-GCM under
// the 32-byte key stored DPAPI-wrapped in Local State
// (os_crypt.encrypted_key). The plugin runs as the same OS user as the
// desktop, so CryptUnprotectData resolves without prompting.
package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func decryptPlatformV10(payload []byte, userDataDir, version string) ([]byte, error) {
	key, err := localStateCryptKey(userDataDir, dpapiUnprotect)
	if err != nil {
		return nil, err
	}
	return aesGCMDecrypt(key, payload)
}

// dpapiUnprotect unwraps one DPAPI blob (crypt32 CryptUnprotectData, no
// extra entropy — Chromium wraps os_crypt keys without it).
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("empty dpapi blob")
	}
	in := windows.CryptoProtectBlob{
		cbData: uint32(len(blob)),
		pbData: &blob[0],
	}
	var out windows.CryptoProtectBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.pbData)))
	raw := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
}
