package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func editGuardProtocolRequest(endpoint string, stream, allowRead bool) (string, string) {
	messages := editRecoveryMessages("Error: <tool_use_error>String to replace not found in file.</tool_use_error>")
	tools := editRecoveryTools()
	payload := map[string]any{"model": "gpt-5.6-reasoning", "reasoning_effort": "max", "stream": stream}
	choice := any("auto")
	if !allowRead {
		choice = map[string]any{"type": "function", "function": map[string]any{"name": "Edit"}}
	}
	if endpoint == "chat" {
		payload["messages"], payload["tools"], payload["tool_choice"] = messages, tools, choice
		return "/v1/chat/completions", mustJSON(payload)
	}
	var converted []any
	for _, message := range messages {
		if message.Role == "assistant" {
			var blocks []any
			for _, call := range message.ToolCalls {
				fn := call["function"].(map[string]any)
				if endpoint == "responses" {
					converted = append(converted, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
				} else {
					var input any
					_ = json.Unmarshal([]byte(fn["arguments"].(string)), &input)
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": input})
				}
			}
			if endpoint == "messages" {
				converted = append(converted, map[string]any{"role": "assistant", "content": blocks})
			}
		} else if message.Role == "tool" {
			if endpoint == "responses" {
				converted = append(converted, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
			} else {
				converted = append(converted, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content}}})
			}
		} else {
			converted = append(converted, map[string]any{"role": message.Role, "content": message.Content})
		}
	}
	var wireTools []any
	for _, tool := range tools {
		fn := tool["function"].(map[string]any)
		if endpoint == "responses" {
			wireTools = append(wireTools, map[string]any{"type": "function", "name": fn["name"], "parameters": fn["parameters"]})
		} else {
			wireTools = append(wireTools, map[string]any{"name": fn["name"], "input_schema": fn["parameters"]})
		}
	}
	payload["tools"] = wireTools
	if endpoint == "responses" {
		payload["input"] = converted
		payload["tool_choice"] = "auto"
		if !allowRead {
			payload["tool_choice"] = map[string]any{"type": "function", "name": "Edit"}
		}
		return "/v1/responses", mustJSON(payload)
	}
	payload["messages"], payload["max_tokens"] = converted, 2048
	payload["tool_choice"] = map[string]any{"type": "auto"}
	if !allowRead {
		payload["tool_choice"] = map[string]any{"type": "tool", "name": "Edit"}
	}
	return "/v1/messages", mustJSON(payload)
}

func TestEditGuardAllProtocolOutputs(t *testing.T) {
	for _, endpoint := range []string{"chat", "messages", "responses"} {
		for _, mode := range []string{"router", "native"} {
			for _, stream := range []bool{false, true} {
				for _, allowRead := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/read=%t", endpoint, mode, stream, allowRead), func(t *testing.T) {
						t.Setenv("M365_USAGE_LOG", filepath.Join(t.TempDir(), "usage.json"))
						s := newAnswerRetryServer(t)
						s.usage = openUsageLog()
						s.settings.v.ToolPlanningMode = mode
						previousRouter, previousAnswer, previousStream := routerFailoverChat, answerChat, streamRecoveryChatWithEvents
						t.Cleanup(func() {
							routerFailoverChat, answerChat, streamRecoveryChatWithEvents = previousRouter, previousAnswer, previousStream
						})
						args := string(failedEditCandidate().Arguments)
						invocations := 0
						routerFailoverChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
							invocations++
							return chathub.Result{Text: "CALL_TOOL: Edit(" + args + ")"}, nil
						}
						output := "```Edit\n" + args + "\n```"
						answerChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
							invocations++
							return chathub.Result{Text: output}, nil
						}
						streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
							invocations++
							res := chathub.Result{Text: output}
							return res, onEvent(chathub.StreamEvent{Kind: "text", Text: output})
						}
						path, payload := editGuardProtocolRequest(endpoint, stream, allowRead)
						r := httptest.NewRequest("POST", path, strings.NewReader(payload))
						r.Header.Set(internalCallHeader, "1")
						w := httptest.NewRecorder()
						switch endpoint {
						case "chat":
							s.openaiChat(w, r)
						case "messages":
							s.anthropicMessages(w, r)
						default:
							s.responses(w, r)
						}
						body := w.Body.String()
						if strings.Contains(body, `"name":"Edit"`) || invocations != 1 {
							t.Fatalf("failed Edit escaped or retried upstream: calls=%d body=%s", invocations, body)
						}
						if allowRead {
							if w.Code != 200 || !strings.Contains(body, `"name":"Read"`) {
								t.Fatalf("missing recovery Read: status=%d body=%s", w.Code, body)
							}
						} else {
							if !strings.Contains(body, "failed Edit requires") || strings.Contains(body, "response.completed") {
								t.Fatalf("recovery failure hidden: status=%d body=%s", w.Code, body)
							}
							if !stream && w.Code != 409 {
								t.Fatalf("status=%d want 409", w.Code)
							}
						}
					})
				}
			}
		}
	}
}
