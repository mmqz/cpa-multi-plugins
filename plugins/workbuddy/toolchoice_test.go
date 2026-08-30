package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, string(b))
	}
	return obj
}

func TestNormalizeToolsOpenAIObjectForce(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash","tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`)
	got := normalizeToolsForUpstream(in)
	obj := decodeBody(t, got)
	tc, _ := obj["tool_choice"].(string)
	if tc != "get_weather" {
		t.Fatalf("expected tool_choice=get_weather, got %#v body=%s", obj["tool_choice"], string(got))
	}
	if _, ok := obj["tools"]; !ok {
		t.Fatal("tools should be preserved for force-name")
	}
}

func TestNormalizeToolsOpenAIObjectAutoRequired(t *testing.T) {
	for _, typ := range []string{"auto", "required"} {
		in, _ := json.Marshal(map[string]any{
			"tools":       []any{map[string]any{"type": "function"}},
			"tool_choice": map[string]any{"type": typ},
		})
		obj := decodeBody(t, normalizeToolsForUpstream(in))
		if obj["tool_choice"] != typ {
			t.Fatalf("type %s: got %#v", typ, obj["tool_choice"])
		}
	}
}

func TestNormalizeToolsNoneStripsTools(t *testing.T) {
	in := []byte(`{"model":"x","tools":[{"type":"function","function":{"name":"get_weather"}}],"functions":[{"name":"legacy"}],"tool_choice":"none","messages":[{"role":"user","content":"hi"}]}`)
	obj := decodeBody(t, normalizeToolsForUpstream(in))
	if _, ok := obj["tool_choice"]; ok {
		t.Fatalf("tool_choice should be removed for none, got %#v", obj["tool_choice"])
	}
	if _, ok := obj["tools"]; ok {
		t.Fatal("tools must be stripped when tool_choice=none")
	}
	if _, ok := obj["functions"]; ok {
		t.Fatal("functions must be stripped when tool_choice=none")
	}
	if obj["model"] != "x" {
		t.Fatalf("unrelated fields altered: %v", obj["model"])
	}
}

func TestNormalizeToolsNoneObjectStripsTools(t *testing.T) {
	in := []byte(`{"tools":[{"type":"function"}],"tool_choice":{"type":"none"}}`)
	obj := decodeBody(t, normalizeToolsForUpstream(in))
	if _, ok := obj["tool_choice"]; ok {
		t.Fatal("tool_choice object none should be removed")
	}
	if _, ok := obj["tools"]; ok {
		t.Fatal("tools should be stripped for object none")
	}
}

func TestNormalizeToolsStringPassthrough(t *testing.T) {
	for _, tc := range []string{"auto", "required", "get_weather"} {
		in, _ := json.Marshal(map[string]any{
			"tools":       []any{map[string]any{"type": "function"}},
			"tool_choice": tc,
		})
		got := normalizeToolsForUpstream(in)
		// auto/required/name leave body unchanged (no re-marshal needed)
		if tc != "none" && string(got) != string(in) {
			// re-marshal may reorder — compare decoded
			obj := decodeBody(t, got)
			if obj["tool_choice"] != tc {
				t.Fatalf("%s: tool_choice became %#v", tc, obj["tool_choice"])
			}
			if _, ok := obj["tools"]; !ok {
				t.Fatalf("%s: tools stripped unexpectedly", tc)
			}
		}
	}
}

func TestNormalizeToolsDropsUnknownObject(t *testing.T) {
	in := []byte(`{"tool_choice":{"type":"weird","foo":1},"tools":[]}`)
	obj := decodeBody(t, normalizeToolsForUpstream(in))
	if _, ok := obj["tool_choice"]; ok {
		t.Fatalf("unknown object tool_choice should be dropped, got %#v", obj["tool_choice"])
	}
}

func TestNormalizeToolsEmptyPayload(t *testing.T) {
	if got := normalizeToolsForUpstream(nil); got != nil {
		t.Fatalf("nil in → %#v", got)
	}
	if got := normalizeToolsForUpstream([]byte{}); len(got) != 0 {
		t.Fatalf("empty in → %q", got)
	}
}

func TestNormalizeToolsInvalidJSONPassthrough(t *testing.T) {
	in := []byte(`not-json`)
	if got := normalizeToolsForUpstream(in); string(got) != string(in) {
		t.Fatalf("invalid json altered: %q", got)
	}
}

func TestNormalizeToolsFunctionObjectMissingNameFallsBackAuto(t *testing.T) {
	in := []byte(`{"tool_choice":{"type":"function","function":{}}}`)
	obj := decodeBody(t, normalizeToolsForUpstream(in))
	if obj["tool_choice"] != "auto" {
		t.Fatalf("expected auto fallback, got %#v", obj["tool_choice"])
	}
}

// Ensure the execute pipeline order still produces a string tool_choice and
// stream=true after all rewrites (regression for the 11101 object error).
func TestPipelineObjectToolChoiceEndsAsString(t *testing.T) {
	in := []byte(`{
		"model":"point/deepseek-v4-flash",
		"stream":false,
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"messages":[{"role":"user","content":"Weather in Tokyo?"}]
	}`)
	body := rewriteModelInBody(normalizeToolsForUpstream(rewriteSystemForUpstream(forceStreamBody(in, nil))), "deepseek-v4-flash")
	obj := decodeBody(t, body)
	if obj["stream"] != true {
		t.Fatalf("stream not forced: %#v", obj["stream"])
	}
	if obj["model"] != "deepseek-v4-flash" {
		t.Fatalf("model not rewritten: %#v", obj["model"])
	}
	tc, ok := obj["tool_choice"].(string)
	if !ok || tc != "get_weather" {
		t.Fatalf("tool_choice not normalized to string name: %#v body=%s", obj["tool_choice"], string(body))
	}
	// must not still be an object (would 400 upstream)
	if strings.Contains(string(body), `"tool_choice":{`) {
		t.Fatalf("object tool_choice leaked into body: %s", body)
	}
}
