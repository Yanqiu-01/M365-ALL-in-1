package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

type spawnWireCall struct {
	id, name, arguments string
	input               map[string]any
}

func TestSpawnCallsReachAllProtocolHandlers(t *testing.T) {
	for _, mode := range []string{"native", "router"} {
		for _, endpoint := range []string{"chat", "messages", "responses", "responses_namespace"} {
			for _, stream := range []bool{false, true} {
				t.Run(mode+"/"+endpoint+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
					t.Setenv("M365_MAX_TOOL_CALLS_PER_TURN", "32")
					t.Setenv("M365_USAGE_LOG", filepath.Join(t.TempDir(), "usage.json"))
					s := newAnswerRetryServer(t)
					s.usage = openUsageLog()
					s.settings.v.ToolPlanningMode = mode
					s.settings.v.MaxToolCallsPerTurn = 32
					messages := []string{"A: preserve literal ) and inspect only", "B: preserve literal ( and inspect only", "C: inspect only"}
					var blocks, directives []string
					for _, message := range messages {
						args, _ := json.Marshal(map[string]any{"fork_context": true, "message": message})
						blocks = append(blocks, "```"+spawnRegressionName+"("+string(args)+" )```")
						directives = append(directives, "CALL_TOOL: "+spawnRegressionName+"("+string(args)+")")
					}
					output := strings.Join(blocks, "\n")
					previousAnswer, previousStream, previousRouter := answerChat, streamRecoveryChatWithEvents, routerFailoverChat
					t.Cleanup(func() {
						answerChat = previousAnswer
						streamRecoveryChatWithEvents = previousStream
						routerFailoverChat = previousRouter
					})
					invocations := 0
					routerFailoverChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
						invocations++
						return chathub.Result{Text: strings.Join(directives, "\n")}, nil
					}
					answerChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
						invocations++
						return chathub.Result{Text: output}, nil
					}
					streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
						invocations++
						// Deliberately split tool names, JSON strings and closing fences.
						for i := 0; i < len(output); i += 11 {
							end := i + 11
							if end > len(output) {
								end = len(output)
							}
							if err := onEvent(chathub.StreamEvent{Kind: "text", Text: output[i:end]}); err != nil {
								return chathub.Result{}, err
							}
						}
						return chathub.Result{Text: output}, nil
					}
					fn := spawnRegressionTools()[0]["function"].(map[string]any)
					payload := map[string]any{"model": "gpt-5.6-sol", "stream": stream}
					path := "/v1/chat/completions"
					switch endpoint {
					case "chat":
						payload["messages"] = []any{map[string]any{"role": "user", "content": "Dispatch three independent read-only review tasks."}}
						payload["tools"] = spawnRegressionTools()
						payload["parallel_tool_calls"] = true
					case "messages":
						path = "/v1/messages"
						payload["max_tokens"] = 1024
						payload["messages"] = []any{map[string]any{"role": "user", "content": "Dispatch three independent read-only review tasks."}}
						payload["tools"] = []any{map[string]any{"name": spawnRegressionName, "description": "Run an independent read-only task", "input_schema": fn["parameters"]}}
					default:
						path = "/v1/responses"
						payload["input"] = "Dispatch three independent read-only review tasks."
						payload["parallel_tool_calls"] = true
						tool := map[string]any{"type": "function", "name": spawnRegressionName, "parameters": fn["parameters"]}
						if endpoint == "responses_namespace" {
							tool["name"] = "spawn_agent"
							payload["tools"] = []any{map[string]any{"type": "namespace", "name": "multi_agent_v1", "tools": []any{tool}}}
						} else {
							payload["tools"] = []any{tool}
						}
					}
					encoded, _ := json.Marshal(payload)
					r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
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
					if invocations != 1 {
						t.Fatalf("unexpected model invocations: %d", invocations)
					}
					calls := readSpawnWireCalls(t, endpoint, stream, w.Body.String())
					if len(calls) != 3 {
						t.Fatalf("got %d structured calls, want 3; body=%s", len(calls), w.Body.String())
					}
					ids := map[string]bool{}
					for i, c := range calls {
						if c.id == "" || ids[c.id] {
							t.Fatalf("missing or duplicate call ID: %q", c.id)
						}
						ids[c.id] = true
						if c.name != spawnRegressionName {
							t.Fatalf("tool name lost: %q", c.name)
						}
						if c.input == nil {
							if err := json.Unmarshal([]byte(c.arguments), &c.input); err != nil {
								t.Fatal(err)
							}
						}
						if c.input["fork_context"] != true || c.input["message"] != messages[i] {
							t.Fatalf("call %d arguments/order changed: %#v", i, c.input)
						}
					}
				})
			}
		}
	}
}

func readSpawnWireCalls(t *testing.T, endpoint string, stream bool, body string) []spawnWireCall {
	t.Helper()
	decode := func(text string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var result map[string]any
	var calls []spawnWireCall
	if !stream {
		result = decode(body)
	} else {
		byIndex := map[int]*spawnWireCall{}
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			e := decode(strings.TrimPrefix(line, "data: "))
			if e["error"] != nil || e["type"] == "response.failed" {
				t.Fatalf("stream failed: %#v", e)
			}
			if endpoint == "messages" {
				if e["type"] == "content_block_start" {
					b := e["content_block"].(map[string]any)
					if b["type"] != "tool_use" {
						t.Fatalf("spawn leaked into non-tool block: %#v", b)
					}
					byIndex[int(e["index"].(float64))] = &spawnWireCall{id: b["id"].(string), name: b["name"].(string)}
				}
				if e["type"] == "content_block_delta" {
					d := e["delta"].(map[string]any)
					if d["type"] != "input_json_delta" {
						t.Fatal("non-tool delta")
					}
					byIndex[int(e["index"].(float64))].arguments += d["partial_json"].(string)
				}
			} else if endpoint == "chat" {
				choices, _ := e["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				d, _ := choices[0].(map[string]any)["delta"].(map[string]any)
				if text, _ := d["content"].(string); text != "" {
					t.Fatalf("spawn leaked into content: %q", text)
				}
				raw, _ := d["tool_calls"].([]any)
				for _, v := range raw {
					c := v.(map[string]any)
					i := int(c["index"].(float64))
					if byIndex[i] == nil {
						byIndex[i] = &spawnWireCall{}
					}
					out := byIndex[i]
					if id, ok := c["id"].(string); ok {
						out.id = id
					}
					fn, _ := c["function"].(map[string]any)
					if n, ok := fn["name"].(string); ok {
						out.name += n
					}
					if a, ok := fn["arguments"].(string); ok {
						out.arguments += a
					}
				}
			} else if e["type"] == "response.completed" {
				result = e["response"].(map[string]any)
			}
		}
		if endpoint == "messages" || endpoint == "chat" {
			for i := 0; i < len(byIndex); i++ {
				if byIndex[i] == nil {
					t.Fatal("non-contiguous tool indices")
				}
				calls = append(calls, *byIndex[i])
			}
			return calls
		}
		if result == nil {
			t.Fatal("missing response.completed")
		}
	}
	switch endpoint {
	case "chat":
		msg, finish := openAIChoice(result)
		if finish != "tool_calls" || msg["content"] != nil {
			t.Fatalf("not a structured tool response: %#v", msg)
		}
		for _, v := range msg["tool_calls"].([]any) {
			c := v.(map[string]any)
			fn := c["function"].(map[string]any)
			calls = append(calls, spawnWireCall{id: c["id"].(string), name: fn["name"].(string), arguments: fn["arguments"].(string)})
		}
	case "messages":
		if result["stop_reason"] != "tool_use" {
			t.Fatal("wrong messages stop reason")
		}
		for _, v := range result["content"].([]any) {
			c := v.(map[string]any)
			if c["type"] != "tool_use" {
				t.Fatal("non-tool content block")
			}
			calls = append(calls, spawnWireCall{id: c["id"].(string), name: c["name"].(string), input: c["input"].(map[string]any)})
		}
	default:
		for _, v := range result["output"].([]any) {
			c := v.(map[string]any)
			if c["type"] != "function_call" {
				t.Fatalf("non-tool output: %#v", c)
			}
			name := c["name"].(string)
			if endpoint == "responses_namespace" {
				if c["namespace"] != "multi_agent_v1" {
					t.Fatalf("namespace missing: %#v", c)
				}
				name = "multi_agent_v1__" + name
			}
			calls = append(calls, spawnWireCall{id: c["call_id"].(string), name: name, arguments: c["arguments"].(string)})
		}
	}
	return calls
}
