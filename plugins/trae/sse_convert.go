// sse_convert.go: SOLO SSE → OpenAI SSE real-time converter.
// Reads upstream SOLO SSE events and emits OpenAI-compatible SSE chunks.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// convertSOLOStreamToOpenAI reads SOLO SSE from r and emits OpenAI SSE chunks
// to the returned channel. Each emitted []byte is "data: {...}\n\n".
func convertSOLOStreamToOpenAI(r io.Reader, model string, onErr func(*upstream.SOLOStreamError)) <-chan []byte {
	ch := make(chan []byte, 32)
	go func() {
		defer close(ch)
		id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		created := time.Now().Unix()

		emit := func(obj map[string]any) {
			raw, _ := json.Marshal(obj)
			ch <- []byte("data: " + string(raw) + "\n\n")
		}

		// Role chunk
		emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		})

		br := bufio.NewReaderSize(r, 64*1024)
		var event, dataLine string
		var pendingUsage map[string]any
		sawDone := false

		for {
			line, err := br.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if line == "" && event != "" {
				ev, parseErr := upstream.ParseSOLOLine(event, dataLine)
				if parseErr == nil && ev != nil {
					switch ev.Event {
					case "output":
						delta := map[string]any{}
						if ev.Response != "" {
							delta["content"] = ev.Response
						}
						if ev.Reasoning != "" {
							delta["reasoning_content"] = ev.Reasoning
						}
						if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
							var tc []map[string]any
							if json.Unmarshal(ev.ToolCalls, &tc) == nil {
								for _, call := range tc {
									if fc, ok := call["function_call"].(map[string]any); ok {
										call["function"] = fc
										delete(call, "function_call")
									}
									if fn, ok := call["function"].(map[string]any); ok {
										delete(fn, "namespace")
										delete(fn, "partial_arguments")
									}
								}
								delta["tool_calls"] = tc
							}
						}
						if len(delta) > 0 {
							emit(map[string]any{
								"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
								"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
							})
						}
					case "token_usage":
						pendingUsage = ev.Usage
					case "done":
						emit(map[string]any{
							"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
							"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": ev.FinishReason}},
						})
						if pendingUsage != nil {
							pt := getFloatVal(pendingUsage, "prompt_tokens")
							ct := getFloatVal(pendingUsage, "completion_tokens")
							tt := getFloatVal(pendingUsage, "total_tokens")
							if tt == 0 {
								tt = pt + ct
							}
							emit(map[string]any{
								"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
								"choices": []any{},
								"usage":   map[string]any{"prompt_tokens": int64(pt), "completion_tokens": int64(ct), "total_tokens": int64(tt)},
							})
						}
						sawDone = true
					case "error":
						se := &upstream.SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
						if onErr != nil {
							onErr(se)
						}
						msg := fmt.Sprintf("trae error code=%d msg=%s", ev.ErrorCode, ev.ErrorMessage)
						emit(map[string]any{
							"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
							"choices": []any{},
							"error":   map[string]any{"message": msg, "type": "api_error"},
						})
						sawDone = true
					}
				}
				event = ""
				dataLine = ""
			} else if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				dataLine = strings.TrimPrefix(line, "data:")
			}
			if err != nil {
				break
			}
		}
		ch <- []byte("data: [DONE]\n\n")
		_ = sawDone // sawDone tracked for future use (e.g. conditional [DONE])
	}()
	return ch
}

func getFloatVal(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func toChunkChannel(chunks []pluginapi.ExecutorStreamChunk) <-chan pluginapi.ExecutorStreamChunk {
	ch := make(chan pluginapi.ExecutorStreamChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch
}
