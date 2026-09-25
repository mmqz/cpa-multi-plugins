//go:build !windows

// dpapi_other.go — 非 Windows 平台的占位：DPAPI 绑定 Windows 用户主密钥，
// 无法在别的平台解。诊断时请用 --inspect-only 做结构盘点。
package main

import "fmt"

func dpapiUnprotect([]byte) ([]byte, error) {
	return nil, fmt.Errorf("DPAPI 仅存在于 Windows 且绑定原用户；本平台无法解密（用 --inspect-only 盘点结构）")
}
