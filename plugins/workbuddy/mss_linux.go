//go:build linux

// mss_linux.go — TCP_MAXSEG clamp for the billing dialer (v0.9.53).
//
// MTU blackhole defence: setting TCP_MAXSEG before connect lowers the MSS we
// advertise in the SYN, so the server segments ITS sends (including the TLS
// certificate flight, the classic large-packet casualty of broken PMTUD
// paths) to fit inside the working packet size. The clamp is deliberately
// conservative (1200 fits PPPoE 1492 and most tunnel overheads) and
// best-effort: a failed setsockopt never aborts the dial.
package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const billingClampMSS = 1200

func dialControl(network, address string, c syscall.RawConn) error {
	if network != "tcp4" && network != "tcp" && network != "tcp6" {
		return nil
	}
	_ = c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG, billingClampMSS)
	})
	return nil
}
