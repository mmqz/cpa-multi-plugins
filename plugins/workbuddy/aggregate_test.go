package main

import (
	"strings"
	"testing"
)

func TestAggregateCompletion_BasicSSE(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"1\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":10,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "test-model")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "Hello world") {
		t.Fatalf("content not merged: %s", s)
	}
	if !strings.Contains(s, "stop") {
		t.Fatalf("finish_reason missing: %s", s)
	}
}

func TestAggregateCompletion_Empty(t *testing.T) {
	// v0.12.76: an empty 200 stream is an upstream failure, not a synthetic
	// success — aggregateCompletion now mirrors the streaming paths' guard.
	_, err := aggregateCompletion(strings.NewReader(""), "test")
	if err == nil || !strings.Contains(err.Error(), "empty_stream") {
		t.Fatalf("expected empty_stream error, got %v", err)
	}
}

func TestAggregateCompletion_NoDone(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"
	out, _ := aggregateCompletion(strings.NewReader(sse), "m")
	if !strings.Contains(string(out), "hi") {
		t.Fatal("content missing")
	}
}

func TestPrepareUpstreamBodyMaxCompletionTokensRename(t *testing.T) {
	// OpenAI-newer clients send max_completion_tokens; the upstream speaks
	// max_tokens. Value copies across when max_tokens is absent; the foreign
	// key is always dropped; an existing max_tokens wins and is untouched.
	body := []byte(`{"model":"deepseek-v4.1","max_completion_tokens":8192,"messages":[]}`)
	out := string(prepareUpstreamBody(body, nil, &storedAuth{}, "m"))
	if !strings.Contains(out, `"max_tokens":8192`) {
		t.Fatalf("max_tokens not set from max_completion_tokens: %s", out)
	}
	if strings.Contains(out, "max_completion_tokens") {
		t.Fatalf("foreign key must be dropped: %s", out)
	}
	body2 := []byte(`{"max_tokens":100,"max_completion_tokens":8192,"messages":[]}`)
	out2 := string(prepareUpstreamBody(body2, nil, &storedAuth{}, "m"))
	if !strings.Contains(out2, `"max_tokens":100`) {
		t.Fatalf("existing max_tokens must win: %s", out2)
	}
	if strings.Contains(out2, "max_completion_tokens") {
		t.Fatalf("foreign key must be dropped even when max_tokens exists: %s", out2)
	}
}

func TestAggregateCompletionDropsTruncatedToolArgs(t *testing.T) {
	// A stream cut mid-arguments leaves an unparseable JSON string; the
	// aggregate must drop the damaged call while clean calls pass through.
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_good\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"/a.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_bad\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"content\\\":\\\"half-writ\"}}]},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "call_bad") || strings.Contains(s, "half-writ") {
		t.Fatalf("truncated tool call must be dropped: %s", s)
	}
	if !strings.Contains(s, "call_good") || !strings.Contains(s, "/a.txt") {
		t.Fatalf("clean tool call must survive: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("finish_reason=length must surface: %s", s)
	}
}

func TestAggregateCompletionKeepsNoArgToolCalls(t *testing.T) {
	// Empty arguments string is a legal no-argument call, not truncation.
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	out, err := aggregateCompletion(strings.NewReader(sse), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !strings.Contains(string(out), "call_x") {
		t.Fatalf("no-arg tool call must survive: %s", string(out))
	}
}

func TestIsTruncatedArgumentsMatrix(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},                // legal no-arg
		{"  ", false},              // whitespace = no-arg
		{`{"a":1}`, false},         // parseable
		{"null", false},            // parseable scalar
		{"[1,2]", false},           // parseable array
		{`{"a":`, true},            // cut mid-object
		{`{"content":"half`, true}, // cut mid-string
	}
	for _, tc := range cases {
		if got := isTruncatedArguments(tc.in); got != tc.want {
			t.Errorf("isTruncatedArguments(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
