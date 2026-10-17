package main

import (
	"encoding/json"
	"testing"
)

func TestForceStreamBody_SetsStreamTrue(t *testing.T) {
	in := []byte(`{"model":"glm","messages":[]}`)
	out := forceStreamBody(in, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Fatal("stream must be true")
	}
}

func TestForceStreamBody_FallbackToOriginal(t *testing.T) {
	in := []byte(``)
	orig := []byte(`{"model":"glm","messages":[]}`)
	out := forceStreamBody(in, orig)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Fatal("stream must be true from original")
	}
}

func TestForceStreamBody_InvalidJSONPassthrough(t *testing.T) {
	in := []byte(`not json`)
	out := forceStreamBody(in, nil)
	if string(out) != "not json" {
		t.Fatal("invalid JSON should pass through unchanged")
	}
}

func TestRewriteSystemForUpstream_PassthroughUnchanged(t *testing.T) {
	in := []byte(`{"model":"glm","messages":[{"role":"user","content":"hello"}]}`)
	out := rewriteSystemForUpstream(in)
	// No blocked templates → should return original
	if string(out) != string(in) {
		t.Fatal("unchanged payload should pass through")
	}
}

func TestRewriteSystemForUpstream_EmptyPassthrough(t *testing.T) {
	out := rewriteSystemForUpstream([]byte(``))
	if len(out) != 0 {
		t.Fatal("empty payload should pass through")
	}
}

func TestRewriteSystemForUpstream_InvalidJSONPassthrough(t *testing.T) {
	in := []byte(`not json`)
	out := rewriteSystemForUpstream(in)
	if string(out) != "not json" {
		t.Fatal("invalid JSON should pass through")
	}
}

func TestForceStreamBody_SetsIncludeUsage(t *testing.T) {
	in := []byte(`{"model":"glm","messages":[]}`)
	out := forceStreamBody(in, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	opts, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %s", out)
	}
	if opts["include_usage"] != true {
		t.Fatalf("include_usage must be true: %s", out)
	}
}

func TestPrepareUpstreamBody_PreservesStreamOptions(t *testing.T) {
	in := []byte(`{"model":"glm","messages":[],"stream_options":{"trace":"keep"}}`)
	out := prepareUpstreamBody(in, nil, &storedAuth{}, "m")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	opts, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %s", out)
	}
	if opts["include_usage"] != true || opts["trace"] != "keep" {
		t.Fatalf("stream_options not preserved: %s", out)
	}
}
