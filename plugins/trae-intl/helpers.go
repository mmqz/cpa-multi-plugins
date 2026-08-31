// helpers.go: crypto helpers (separated to avoid pulling crypto/rand import
// conflicts with the cgo main file).
package main

import (
	"crypto/rand"
	"encoding/hex"
)

func readRand(b []byte) (int, error) {
	return rand.Read(b)
}

func hexEncode(b []byte) string {
	return hex.EncodeToString(b)
}
