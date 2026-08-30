// helpers.go: net aliases + random device identifiers.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"net"
)

// Aliases so main.go can reference them as netListen / netTCPAddr etc.
type (
	netListener   = net.Listener
	netTCPAddr    = net.TCPAddr
	netTCPListener = net.TCPListener
)

func netListen(network, addr string) (netListener, error) {
	return net.Listen(network, addr)
}

// randomHex returns n random bytes as a hex string (length 2*n).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
