// capture.go — TEMPORARY upstream wire capture for the reasoning-token ("01")
// investigation. Enabled ONLY when the WBCAPTURE_DIR environment variable is
// set to an existing directory; without it every helper returns immediately
// and the plugin behaves byte-for-byte as before.
//
// What is captured:
//   - dir "out": the final request body the plugin sends to the CodeBuddy
//     upstream (after prepareUpstreamBody / model rewrite) — one entry per call.
//   - dir "in": every raw upstream SSE line, verbatim, before any cleaning.
//
// Nothing else is written: no headers (the bearer token lives there), no
// captures when the env var is absent. Files: <dir>/wb-<key>.log, one JSON
// line per entry. Open/append/close per entry — capture traffic is tiny
// compared to chat, and simple code beats clever code in a debug tool.
// Remove this file (and its call sites) after the investigation.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var captureDir = strings.TrimSpace(os.Getenv("WBCAPTURE_DIR"))

func captureEnabled() bool { return captureDir != "" }

// captureKey builds a per-request key used as the capture file suffix:
// readable, unique per request, filesystem-safe.
func captureKey(prefix, model, uid string) string {
	uid8 := strings.TrimSpace(uid)
	if len(uid8) > 8 {
		uid8 = uid8[len(uid8)-8:]
	}
	return sanitizeCaptureToken(prefix + "-" + model + "-" + uid8)
}

func nanosKey(prefix string) string {
	return prefix + "-" + strings.ToLower(strconvFormatInt36(time.Now().UnixNano()))
}

func strconvFormatInt36(n int64) string {
	// Small local base-36 formatter to avoid importing strconv at multiple
	// call sites; capture keys only need uniqueness, not elegance.
	if n == 0 {
		return "0"
	}
	digits := "0123456789abcdefghijklmnopqrstuvwxyz"
	out := ""
	for n > 0 {
		out = string(digits[n%36]) + out
		n /= 36
	}
	return out
}

func sanitizeCaptureToken(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// captureUpstream writes one capture entry. Failures are silent by design:
// a capture problem must never affect the chat path.
func captureUpstream(key, dir, model, uid, data string) {
	if !captureEnabled() {
		return
	}
	entry, err := json.Marshal(map[string]string{
		"ts":   time.Now().Format(time.RFC3339Nano),
		"dir":  dir,
		"model": model,
		"uid":  uid,
		"data": data,
	})
	if err != nil {
		return
	}
	name := filepath.Join(captureDir, "wb-"+key+".log")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(entry, '\n'))
	_ = f.Close()
}
