//go:build !linux

// mss_other.go — no-op dial control for non-Linux platforms. The MSS clamp is
// a Linux-specific mitigation (TCP_MAXSEG pre-connect semantics); darwin /
// windows builds dial with the platform default.
package main

import "syscall"

func dialControl(network, address string, c syscall.RawConn) error {
	return nil
}
