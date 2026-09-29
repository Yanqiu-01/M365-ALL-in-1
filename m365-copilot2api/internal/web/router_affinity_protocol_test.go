package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestRouterToolContinuationKeepsHealthyAccount(t *testing.T) {
	for _, endpoint := range []string{"chat", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				t.Setenv("M365_USAGE_LOG", filepath.Join(t.TempDir(), "usage.json"))
				t.Setenv("M365_MAX_TOOL_CALLS_PER_TURN", "32")
				s := newAnswerRetryServer(t)
				s.usage = openUsageLog()
				s.settings.v.ToolPlanningMode = "router"
				s.resourceScheduler = newResourceScheduler()
				addBindingAccount(t, s, "zz-backup", time.Now().Add(time.Hour))
				firstAccount := ""
				previousRouter, previousAnswer, previousStream := routerFailoverChat, answerChat, streamRecoveryChatWithEvents
				t.Cleanup(func() {
					routerFailoverChat, answerChat, streamRecoveryChatWithEvents = previousRouter, previousAnswer, previousStream
				})
				routerFailoverChat = func(_ context.Context, _ *Server, id string, _ chathub.Account, _ chathub.Request) (chathub.Result, error) {
					firstAccount = id
					var directives []string
					for _, label := range []string{"A", "B", "C"} {
						directives = append(directives, "CALL_TOOL: "+spawnRegressionName+`({"fork_context":true,"message":"Review `+label+`"})`)
					}
					return chathub.Result{Text: strings.Join(directives, "\n")}, nil
				}
				const initial = "Dispatch three independent read-only review tasks for cobalt materials."
				fn := spawnRegressionTools()[0]["function"].(map[string]any)
				payload := map[string]any{"model": "gpt-5.6-sol", "stream": stream}
				path := "/v1/chat/completions"
				switch endpoint {
				case "chat":
					payload["messages"] = []any{map[string]any{"role": "user", "content": initial}}
					payload["tools"] = spawnRegressionTools()
				case "messages":
					path = "/v1/messages"
					payload["max_tokens"] = 1024
					payload["messages"] = []any{map[string]any{"role": "user", "content": initial}}
					payload["tools"] = []any{map[string]any{"name": spawnRegressionName, "input_schema": fn["parameters"]}}
				default:
					path = "/v1/responses"
					payload["input"] = initial
					payload["tools"] = []any{map[string]any{"type": "function", "name": spawnRegressionName, "parameters": fn["parameters"]}}
				}
				invoke := func() string {
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					r := httptest.NewRequest("POST", path, strings.NewReader(string(encoded)))
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
					if w.Code != 200 {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					return w.Body.String()
				}
				calls := readSpawnWireCalls(t, endpoint, stream, invoke())
				if firstAccount == "" || len(calls) != 3 {
					t.Fatalf("account=%s calls=%d", firstAccount, len(calls))
				}
				answerInvocations := 0
				answer := func(id string, req chathub.Request) chathub.Result {
					answerInvocations++
					if id != firstAccount {
						t.Fatalf("healthy tool continuation moved account: %s -> %s", firstAccount, id)
					}
					if req.ConversationID != "" || !strings.Contains(req.Text, "cobalt materials") || !strings.Contains(req.Text, "review-result-") {
						t.Fatalf("local tool continuation lost full history: %+v", req)
					}
					return chathub.Result{Text: "Reviews are complete."}
				}
				answerChat = func(_ context.Context, _ *Server, id string, _ chathub.Account, req chathub.Request) (chathub.Result, error) {
					return answer(id, req), nil
				}
				streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, id string, _ chathub.Account, req chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
					res := answer(id, req)
					return res, onEvent(chathub.StreamEvent{Kind: "text", Text: res.Text})
				}
				delete(payload, "tools")
				var toolCalls, toolResults []any
				for i, call := range calls {
					if call.arguments == "" {
						b, _ := json.Marshal(call.input)
						call.arguments = string(b)
					}
					if call.input == nil {
						if err := json.Unmarshal([]byte(call.arguments), &call.input); err != nil {
							t.Fatal(err)
						}
					}
					result := fmt.Sprintf("review-result-%d", i)
					switch endpoint {
					case "chat":
						toolCalls = append(toolCalls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": call.arguments}})
						toolResults = append(toolResults, map[string]any{"role": "tool", "tool_call_id": call.id, "content": result})
					case "messages":
						toolCalls = append(toolCalls, map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": call.input})
						toolResults = append(toolResults, map[string]any{"type": "tool_result", "tool_use_id": call.id, "content": result})
					default:
						toolCalls = append(toolCalls, map[string]any{"type": "function_call", "call_id": call.id, "name": call.name, "arguments": call.arguments})
						toolResults = append(toolResults, map[string]any{"type": "function_call_output", "call_id": call.id, "output": result})
					}
				}
				history := []any{map[string]any{"role": "user", "content": initial}}
				switch endpoint {
				case "chat":
					history = append(history, map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls})
					history = append(history, toolResults...)
				case "messages":
					history = append(history, map[string]any{"role": "assistant", "content": toolCalls}, map[string]any{"role": "user", "content": toolResults})
				default:
					history = append(history, toolCalls...)
					history = append(history, toolResults...)
				}
				history = append(history, map[string]any{"role": "user", "content": "Summarize the review results."})
				if endpoint == "responses" {
					payload["input"] = history
				} else {
					payload["messages"] = history
				}
				if body := invoke(); !strings.Contains(body, "Reviews are complete.") || answerInvocations != 1 {
					t.Fatalf("continuation response=%s calls=%d", body, answerInvocations)
				}
			})
		}
	}
}
