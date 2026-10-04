package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestSchedulerPickDefersToBuiltin locks the issue #2 fix: the host probes
// scheduler.pick on every plugin; trae must ANSWER (previously unknown
// method → host 500 on every request while trae was enabled) while keeping
// its no-routing philosophy (Handled: false → host built-in scheduler).
func TestSchedulerPickDefersToBuiltin(t *testing.T) {
	raw := []byte(`{"plugin":{"id":"trae"},"provider":"trae","model":"m","candidates":[{"id":"a","provider":"trae"},{"id":"b","provider":"other"}]}`)
	out, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatalf("handleSchedulerPick: %v", err)
	}
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Result.Handled {
		t.Error("trae must not take over routing (Handled must be false)")
	}
	if env.Result.AuthID != "" {
		t.Errorf("no auth should be picked, got %q", env.Result.AuthID)
	}
}
