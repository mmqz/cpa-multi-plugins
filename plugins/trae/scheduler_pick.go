// scheduler_pick.go implements the pluginabi.MethodSchedulerPick capability
// for the trae plugin.
//
// issue #2: the host unconditionally probes scheduler.pick on every plugin.
// A missing method made the host log
//
//	scheduler rejected auth pick plugin_id=trae error=unknown method:
//	scheduler.pick
//
// and fail the whole /v1/chat/completions request with HTTP 500 — enabling
// the trae plugin alone was enough to break every request, even ones routed
// to other plugins.
//
// Trae deliberately does NOT take over auth routing: each request goes to
// whichever account the host routed it to (see intl_management.go). So the
// handler validates the request envelope and defers every pick back to the
// built-in scheduler (Handled: false). The method now exists — the host
// stops treating the plugin as broken — and routing behavior is unchanged.
package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Routing stays host-side by design; see the package comment. Handled:
	// false tells the host to run its built-in scheduler for this pick.
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}
