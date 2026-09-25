//go:build windows

// dpapi_windows.go — 探针的 DPAPI 环节，与插件 cookies_windows.go 同源
// （crypt32 CryptUnprotectData，无附加熵 —— Chromium 包装 os_crypt 密钥不用熵）。
package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("empty dpapi blob")
	}
	in := windows.DataBlob{
		Size: uint32(len(blob)),
		Data: &blob[0],
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	raw := unsafe.Slice(out.Data, out.Size)
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
}
