package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 0.8.74 (PR #73 hardening): the thinking dial the catalog declares —
// object-keyed efforts, array shapes, reasoning_efforts fallback, canonical
// order, ZeroAllowed from the disabled key.

func mustRaw(s string) json.RawMessage { return json.RawMessage(s) }

func assertLevels(t *testing.T, ts *pluginapi.ThinkingSupport, want []string, wantZero bool) {
	t.Helper()
	if ts == nil {
		t.Fatalf("thinkingSupport = nil, want levels %v", want)
	}
	if !reflect.DeepEqual(ts.Levels, want) {
		t.Fatalf("Levels = %v, want %v", ts.Levels, want)
	}
	if ts.ZeroAllowed != wantZero {
		t.Fatalf("ZeroAllowed = %v, want %v", ts.ZeroAllowed, wantZero)
	}
}

func TestThinkingSupportObjectEfforts(t *testing.T) {
	// The documented shape: an object keyed by level (values carry
	// is_default, which the level NAME alone carries here).
	m := chatSceneModel{ThinkingConfig: mustRaw(
		`{"enabled":{"efforts":{"max":{},"low":{"is_default":true},"high":{}}}}`)}
	assertLevels(t, m.thinkingSupport(), []string{"low", "high", "max"}, false)
}

func TestThinkingSupportDisabledKeySetsZeroAllowed(t *testing.T) {
	m := chatSceneModel{ThinkingConfig: mustRaw(
		`{"enabled":{"efforts":{"low":{},"high":{}}},"disabled":{"efforts":{}}}`)}
	assertLevels(t, m.thinkingSupport(), []string{"low", "high"}, true)
}

func TestThinkingSupportArrayShapedEfforts(t *testing.T) {
	// Shape drift tolerance: the same names as an array must parse too.
	m := chatSceneModel{ThinkingConfig: mustRaw(
		`{"enabled":{"efforts":["max","low","high"]}}`)}
	assertLevels(t, m.thinkingSupport(), []string{"low", "high", "max"}, false)
}

func TestThinkingSupportCanonicalOrder(t *testing.T) {
	m := chatSceneModel{ThinkingConfig: mustRaw(
		`{"enabled":{"efforts":{"max":{},"minimal":{},"xhigh":{},"low":{},"turbo":{},"high":{},"medium":{}}}}`)}
	// Canonical ranks first in order, unknown names after (alphabetically).
	assertLevels(t, m.thinkingSupport(),
		[]string{"minimal", "low", "medium", "high", "xhigh", "max", "turbo"}, false)
}

func TestThinkingSupportReasoningEffortsFallback(t *testing.T) {
	// Entries without a thinking_config may carry the levels as a
	// reasoning_efforts array (magpie modelInfo parity).
	m := chatSceneModel{ReasoningEfforts: mustRaw(`["high","low","max"]`)}
	assertLevels(t, m.thinkingSupport(), []string{"low", "high", "max"}, false)
}

func TestThinkingConfigWinsOverReasoningEfforts(t *testing.T) {
	// A thinking_config OBJECT discards reasoning_efforts — even when it
	// names no levels (that model thinks at upstream defaults, pre-#73
	// behavior).
	m := chatSceneModel{
		ReasoningEfforts: mustRaw(`["low","high"]`),
		ThinkingConfig:   mustRaw(`{"enabled":{"efforts":{}}}`),
	}
	if ts := m.thinkingSupport(); ts != nil {
		t.Fatalf("empty efforts object must declare nothing, got %+v", ts)
	}
	// Same for enabled with no efforts key at all.
	m2 := chatSceneModel{
		ReasoningEfforts: mustRaw(`["low","high"]`),
		ThinkingConfig:   mustRaw(`{"enabled":{}}`),
	}
	if ts := m2.thinkingSupport(); ts != nil {
		t.Fatalf("enabled without efforts must declare nothing, got %+v", ts)
	}
}

func TestThinkingSupportNullAndAbsent(t *testing.T) {
	// thinking_config: null is not an object — the reasoning_efforts
	// fallback still applies (magpie: a null tc is falsy).
	m := chatSceneModel{
		ReasoningEfforts: mustRaw(`["low","high"]`),
		ThinkingConfig:   mustRaw(`null`),
	}
	assertLevels(t, m.thinkingSupport(), []string{"low", "high"}, false)

	// Nothing declared at all.
	m2 := chatSceneModel{}
	if ts := m2.thinkingSupport(); ts != nil {
		t.Fatalf("no thinking material must declare nothing, got %+v", ts)
	}

	// Non-string array members are skipped, not fatal.
	m3 := chatSceneModel{ReasoningEfforts: mustRaw(`["low",42,null,""]`)}
	assertLevels(t, m3.thinkingSupport(), []string{"low"}, false)
}

func TestRawKindHelpers(t *testing.T) {
	if !rawIsObject(mustRaw(` {"enabled":{}} `)) {
		t.Error("object with leading space not detected")
	}
	if rawIsObject(mustRaw(`null`)) || rawIsObject(mustRaw(`[1]`)) || rawIsObject(nil) {
		t.Error("null/array/absent must not read as object")
	}
	if !rawPresent(mustRaw(`{}`)) || !rawPresent(mustRaw(`{ "efforts": {} }`)) {
		t.Error("present values must read as present")
	}
	if rawPresent(mustRaw(`null`)) || rawPresent(nil) {
		t.Error("null/absent must not read as present")
	}
}
