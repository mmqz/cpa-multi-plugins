// version_guard_test.go — repo lesson 2026-09-23 (trae v0.12.86 shipped
// self-reporting 0.12.56) and issue #30 follow-up (workbuddy 0.9.56 shipped
// builds self-reporting 0.9.46): releases build WITHOUT -X injection, so
// main.go's var must track the VERSION file.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestVersionMatchesVERSIONFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Skipf("VERSION file unavailable: %v", err)
	}
	if v := strings.TrimSpace(string(raw)); v != version {
		t.Fatalf("main.go var version %q drifts from the VERSION file %q — keep them in lockstep", version, v)
	}
}
