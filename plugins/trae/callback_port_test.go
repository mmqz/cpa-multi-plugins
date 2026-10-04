// callback_port_test.go — v0.12.14: callback_port config parsing tests.
// The fixed-port mode lets Docker/published-port deployments map the OAuth
// callback listener once; parsing must accept quoted/plain values, clamp to
// 0..65535 and keep 0 as the ephemeral default.
package main

import (
	"testing"
)

func TestConfigureCallbackPort(t *testing.T) {
	// Save & restore the global so other tests are unaffected.
	callbackPortMu.Lock()
	orig := callbackPortValue
	callbackPortMu.Unlock()
	defer func() {
		callbackPortMu.Lock()
		callbackPortValue = orig
		callbackPortMu.Unlock()
	}()

	cases := []struct {
		name string
		line string
		want int
	}{
		{"plain", "callback_port: 41890", 41890},
		{"quoted", `callback_port: "41890"`, 41890},
		{"single-quoted", "callback_port: '41890'", 41890},
		{"zero means ephemeral", "callback_port: 0", 0},
		{"max", "callback_port: 65535", 65535},
		{"above max ignored", "callback_port: 65536", -1}, // -1: unchanged
		{"negative ignored", "callback_port: -1", -1},
		{"garbage ignored", "callback_port: abc", -1},
		{"empty ignored", "callback_port:", -1},
		{"unrelated line", "login_variant: cn", -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset to a sentinel before each case.
			callbackPortMu.Lock()
			callbackPortValue = 7777
			callbackPortMu.Unlock()

			configureCallbackPort([]string{tc.line})

			callbackPortMu.RLock()
			got := callbackPortValue
			callbackPortMu.RUnlock()

			want := tc.want
			if want == -1 {
				want = 7777 // invalid input must leave the value untouched
			}
			if got != want {
				t.Fatalf("line %q: got %d, want %d", tc.line, got, want)
			}
		})
	}
}

func TestLoadedCallbackPortDefault(t *testing.T) {
	callbackPortMu.Lock()
	orig := callbackPortValue
	callbackPortValue = 0
	callbackPortMu.Unlock()
	defer func() {
		callbackPortMu.Lock()
		callbackPortValue = orig
		callbackPortMu.Unlock()
	}()

	if got := loadedCallbackPort(); got != 0 {
		t.Fatalf("default callback port = %d, want 0 (ephemeral)", got)
	}
}
