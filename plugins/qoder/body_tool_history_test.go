package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Qoder drops assistant turns with empty content even when they carry
// tool_calls, so the tool result that follows loses its anchor upstream.
// buildQoderBody must give exactly those turns a minimal body — and leave
// every other shape (and the caller's request) untouched.

func TestBuildQoderBodyKeepsEmptyToolCallAssistant(t *testing.T) {
	for _, content := range []string{"", `,"content":null`, `,"content":""`, `,"content":"  "`, `,"content":[]`} {
		for _, slim := range []bool{false, true} {
			name := content + map[bool]string{false: "/template", true: "/slim"}[slim]
			t.Run(name, func(t *testing.T) {
				call := mustMsg(`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{}"}}]` + content + `}`)
				req := &openAIRequest{Messages: []openAIMessage{
					mustMsg(`{"role":"user","content":"Compute 1+1."}`),
					call,
					mustMsg(`{"role":"tool","tool_call_id":"call_1","content":"2"}`),
				}}
				if slim {
					req.Messages = append([]openAIMessage{mustMsg(`{"role":"system","content":"Answer briefly."}`)}, req.Messages...)
				}
				before, _ := json.Marshal(req.Messages)
				body, err := buildQoderBody(req, "dfmodel", "personal_professional_trial")
				if err != nil {
					t.Fatal(err)
				}
				var out struct {
					Messages []map[string]json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatal(err)
				}
				if len(out.Messages) < 3 {
					t.Fatalf("messages = %d, want assistant+tool tail", len(out.Messages))
				}
				assistant := out.Messages[len(out.Messages)-2]
				var text string
				if json.Unmarshal(assistant["content"], &text) != nil || strings.TrimSpace(text) == "" {
					t.Fatal("upstream would discard the empty assistant and orphan its tool result")
				}
				var calls, originalCalls any
				_ = json.Unmarshal(assistant["tool_calls"], &calls)
				_ = json.Unmarshal(call.raw["tool_calls"], &originalCalls)
				if !reflect.DeepEqual(calls, originalCalls) {
					t.Fatal("tool call changed")
				}
				if string(out.Messages[len(out.Messages)-1]["tool_call_id"]) != `"call_1"` {
					t.Fatal("tool result association changed")
				}
				after, _ := json.Marshal(req.Messages)
				if string(before) != string(after) {
					t.Fatal("caller messages mutated")
				}
			})
		}
	}
}

func TestBuildQoderBodyPreservesOtherAssistantContent(t *testing.T) {
	for _, raw := range []string{
		`{"role":"assistant","content":"I will calculate.","tool_calls":[{"id":"c1"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"I will calculate."}],"tool_calls":[{"id":"c1"}]}`,
		`{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}],"tool_calls":[{"id":"c1"}]}`,
		`{"role":"assistant","content":null}`,
		`{"role":"assistant","content":null,"tool_calls":[]}`,
	} {
		req := &openAIRequest{Messages: []openAIMessage{
			mustMsg(`{"role":"system","content":"Answer briefly."}`),
			mustMsg(raw),
			mustMsg(`{"role":"user","content":"Continue."}`),
		}}
		body, err := buildQoderBody(req, "dfmodel", "personal_professional_trial")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		var original map[string]any
		_ = json.Unmarshal([]byte(raw), &original)
		if !reflect.DeepEqual(out.Messages[1], original) {
			t.Fatalf("message changed: %s", raw)
		}
	}
}

// 占位词也是模型模仿源：历史里反复写 "Calling tools."，Q 模型就会学它继续说。
// 空工具调用只需要一个中性非空正文，不能再注入这句 UI 前缀。
func TestBuildQoderBodyUsesNeutralToolCallPlaceholder(t *testing.T) {
	call := mustMsg(`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{}"}}]}`)
	req := &openAIRequest{Messages: []openAIMessage{
		mustMsg(`{"role":"system","content":"Answer briefly."}`),
		mustMsg(`{"role":"user","content":"Compute 1+1."}`),
		call,
		mustMsg(`{"role":"tool","tool_call_id":"call_1","content":"2"}`),
	}}
	body, err := buildQoderBody(req, "dfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) < 3 {
		t.Fatalf("messages = %d, want assistant+tool tail", len(out.Messages))
	}
	assistant := out.Messages[len(out.Messages)-2]
	var text string
	if err := json.Unmarshal(assistant["content"], &text); err != nil || strings.TrimSpace(text) == "" {
		t.Fatalf("占位正文必须非空：text=%q err=%v", text, err)
	}
	if strings.EqualFold(strings.TrimSpace(text), "Calling tools") ||
		strings.EqualFold(strings.TrimSpace(text), "Calling tools.") {
		t.Fatalf("不能再注入 Calling tools. 占位词：%q", text)
	}
}

// 历史占位不能是模型会模仿的可见字符：`.` 已经被模型回显成草稿小点。
// 这里固定用零宽占位符，保证上游拿到非空 assistant 正文，但用户看不到。
func TestBuildQoderBodyUsesInvisibleToolCallPlaceholder(t *testing.T) {
	call := mustMsg(`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{}"}}]}`)
	req := &openAIRequest{Messages: []openAIMessage{
		mustMsg(`{"role":"system","content":"Answer briefly."}`),
		mustMsg(`{"role":"user","content":"Compute 1+1."}`),
		call,
		mustMsg(`{"role":"tool","tool_call_id":"call_1","content":"2"}`),
	}}
	body, err := buildQoderBody(req, "dfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	assistant := out.Messages[len(out.Messages)-2]
	var text string
	if err := json.Unmarshal(assistant["content"], &text); err != nil {
		t.Fatalf("占位正文必须是字符串：%v", err)
	}
	if text != "\u200b" {
		t.Fatalf("占位正文必须是零宽占位符，实际=%q", text)
	}
}
