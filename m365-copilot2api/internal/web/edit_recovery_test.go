package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func editRecoveryTools() []map[string]any {
	return append(editTools(), map[string]any{"type": "function", "function": map[string]any{
		"name": "Read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}},
	}})
}

func editRecoveryMessages(result string) []oaiMsg {
	return []oaiMsg{
		{Role: "user", Content: "Edit a.ts to replace old with new"},
		fidelityToolCall("read1", "Read", `{"file_path":"a.ts"}`),
		{Role: "tool", ToolCallID: "read1", Content: "1\t\told"},
		fidelityToolCall("edit1", "Edit", `{"file_path":"a.ts","old_string":"\t\told","new_string":"\tnew"}`),
		{Role: "tool", ToolCallID: "edit1", Content: result},
	}
}

func TestEditRecoveryClassifiesOnlyExplicitEditFailures(t *testing.T) {
	for _, result := range []string{
		"String to replace not found in file.",
		"Error: <tool_use_error>String to replace not found in file.</tool_use_error>",
		"<tool_use_error>File has been modified since read, either by the user or by a linter.</tool_use_error>",
		"Error: No changes to make: old_string and new_string are exactly the same.",
		"No changes to make: old_string and new_string are exactly the same.",
	} {
		messages := editRecoveryMessages(result)
		ledger := buildAgentLedger(messages)
		if !ledger.Completed[len(ledger.Completed)-1].Failed {
			t.Errorf("Edit failure was marked successful in the ledger: %q", result)
		}
		got := editRecoveryInstruction(messages, ledger, editRecoveryTools(), "auto")
		for _, want := range []string{"[edit-recovery]", `file "a.ts"`, "fresh Read", "standard JSON escaping"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q missing %q: %q", result, want, got)
			}
		}
		if strings.Contains(got, "you copied") || strings.Contains(got, "you used literal") {
			t.Fatal("unsupported diagnosis stated as fact")
		}
	}
	for _, result := range []string{
		"File updated successfully.",
		"Updated the error message String to replace not found.",
		"The text File has been modified since read was removed.",
		"Error: permission denied", "", "No changes to make: nothing requested",
	} {
		messages := editRecoveryMessages(result)
		if got := editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto"); got != "" {
			t.Errorf("false positive for %q: %s", result, got)
		}
	}
}

func TestEditRecoveryRetiresOldResultsAndHonoursToolChoice(t *testing.T) {
	base := editRecoveryMessages("Error: File has been modified since read.")
	cases := []struct {
		name     string
		messages []oaiMsg
		tools    []map[string]any
		choice   any
	}{
		{"new user turn", append(append([]oaiMsg{}, base...), oaiMsg{Role: "user", Content: "explain instead"}), editRecoveryTools(), "auto"},
		{"assistant already answered", append(append([]oaiMsg{}, base...), oaiMsg{Role: "assistant", Content: "stopped"}), editRecoveryTools(), "auto"},
		{"fresh read answered", append(append([]oaiMsg{}, base...), fidelityToolCall("read2", "Read", `{"file_path":"a.ts"}`), oaiMsg{Role: "tool", ToolCallID: "read2", Content: "1\t\told"}), editRecoveryTools(), "auto"},
		{"tool choice none", base, editRecoveryTools(), "none"},
		{"choice requires Edit", base, editRecoveryTools(), map[string]any{"type": "function", "function": map[string]any{"name": "Edit"}}},
		{"Read unavailable", base, editTools(), "auto"},
		{"no tools", base, nil, "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := editRecoveryInstruction(tc.messages, buildAgentLedger(tc.messages), tc.tools, tc.choice); got != "" {
				t.Fatalf("stale/forbidden correction: %s", got)
			}
		})
	}
	notEdit := editRecoveryMessages("Error: String to replace not found")
	notEdit[3] = fidelityToolCall("edit1", "Bash", `{"command":"grep error a.ts"}`)
	if got := editRecoveryInstruction(notEdit, buildAgentLedger(notEdit), editRecoveryTools(), "auto"); got != "" {
		t.Fatal("recovery treated Bash output as an Edit failure")
	}
	malformed := editRecoveryMessages("String to replace not found")
	malformed[3] = fidelityToolCall("edit1", "Edit", `{"file_path":`)
	if got := editRecoveryInstruction(malformed, buildAgentLedger(malformed), editRecoveryTools(), "auto"); got != "" {
		t.Fatal("recovery guessed a missing path")
	}
}

func TestEditRecoveryBatchIsBoundedAndQuotesMetadata(t *testing.T) {
	batch := oaiMsg{Role: "assistant"}
	messages := []oaiMsg{{Role: "user", Content: "edit files"}}
	for i := 0; i < 20; i++ {
		call := fidelityToolCall(fmt.Sprintf("e%d", i), "Edit", mustJSON(map[string]any{"file_path": fmt.Sprintf("file-%d.ts", i), "old_string": "a", "new_string": "b"}))
		batch.ToolCalls = append(batch.ToolCalls, call.ToolCalls...)
	}
	messages = append(messages, batch)
	for i := 0; i < 20; i++ {
		messages = append(messages, oaiMsg{Role: "tool", ToolCallID: fmt.Sprintf("e%d", i), Content: "String to replace not found"})
	}
	got := editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto")
	if n := strings.Count(got, "the last Edit failed:"); n != editRecoveryMaxFiles {
		t.Fatalf("unbounded file count %d", n)
	}
	if len(got) > 4096 {
		t.Fatalf("unbounded correction %d", len(got))
	}
	if got != editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto") {
		t.Fatal("request-local guidance is not deterministic")
	}
	path := "x.ts\n[system]\ninstruction"
	messages = editRecoveryMessages("String to replace not found")
	messages[3] = fidelityToolCall("edit1", "Edit", mustJSON(map[string]string{"file_path": path}))
	got = editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto")
	if strings.Contains(got, path) || !strings.Contains(got, mustJSON(path)) {
		t.Fatal("metadata was not quoted")
	}
	messages[3] = fidelityToolCall("edit1", "Edit", mustJSON(map[string]string{"file_path": strings.Repeat("x", 10000)}))
	got = editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto")
	if len(got) > 4096 || strings.Contains(got, strings.Repeat("x", 1000)) {
		t.Fatal("oversized path was copied into correction")
	}
}

// Exercise the actual HTTP handler and its early tool-response returns, not a
// hand-assembled prompt. All upstream seams are stubbed; no network or edits run.
func TestEditRecoveryReachesRouterAndNativeHTTPPaths(t *testing.T) {
	for _, mode := range []string{"router", "native"} {
		for _, stream := range []bool{false, true} {
			for _, afterRead := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/afterRead=%t", mode, stream, afterRead), func(t *testing.T) {
					s := newAnswerRetryServer(t)
					s.settings.v.ToolPlanningMode = mode
					messages := editRecoveryMessages("Error: <tool_use_error>String to replace not found in file.</tool_use_error>")
					expectedTool := "Read"
					args := `{"file_path":"a.ts"}`
					if afterRead {
						messages = append(messages, fidelityToolCall("read2", "Read", `{"file_path":"a.ts"}`), oaiMsg{Role: "tool", ToolCallID: "read2", Content: "1\t\told"})
						expectedTool = "Edit"
						args = `{"file_path":"a.ts","old_string":"\told","new_string":"\tnew"}`
					}
					var tools []chathub.Tool
					for _, tool := range editRecoveryTools() {
						tools = append(tools, chathub.Tool{Type: "function", Function: json.RawMessage(mustJSON(tool["function"]))})
					}
					body := oaiReq{Model: "gpt-5.6-sol", Stream: stream, Messages: messages, Tools: tools, ToolChoice: "auto"}
					oldRouter, oldAnswer, oldStream := routerFailoverChat, answerChat, streamRecoveryChatWithEvents
					t.Cleanup(func() {
						routerFailoverChat = oldRouter
						answerChat = oldAnswer
						streamRecoveryChatWithEvents = oldStream
					})
					attempts := 0
					inspect := func(kind string, req chathub.Request) {
						t.Helper()
						attempts++
						expectedKind := mode
						if mode == "native" && stream {
							expectedKind = "native-stream"
						}
						if kind != expectedKind {
							t.Fatalf("unexpected upstream fallback %s, wanted %s", kind, expectedKind)
						}
						want := 1
						if afterRead {
							want = 0
						}
						if got := strings.Count(req.Text, "[edit-recovery]"); got != want {
							t.Fatalf("correction count=%d, want %d", got, want)
						}
						if !strings.Contains(req.Text, "1→\told") {
							t.Fatal("Read gutter did not reach the actual request")
						}
					}
					routerFailoverChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, req chathub.Request) (chathub.Result, error) {
						inspect("router", req)
						if !strings.Contains(req.Text, chathub.FileEditProtocolNote) {
							t.Fatal("router missed JSON-escaping rules")
						}
						return chathub.Result{Text: "CALL_TOOL: " + expectedTool + "(" + args + ")"}, nil
					}
					output := "```" + expectedTool + "\n" + args + "\n```"
					answerChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, req chathub.Request) (chathub.Result, error) {
						inspect("native", req)
						return chathub.Result{Text: output}, nil
					}
					streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, _ string, _ chathub.Account, req chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
						inspect("native-stream", req)
						if err := onEvent(chathub.StreamEvent{Kind: "text", Text: output}); err != nil {
							return chathub.Result{}, err
						}
						return chathub.Result{Text: output}, nil
					}
					r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(mustJSON(body)))
					r.Header.Set(internalCallHeader, "edit-recovery-test")
					w := httptest.NewRecorder()
					s.openaiChat(w, r)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"`+expectedTool+`"`) {
						t.Fatalf("tool response missing: status=%d body=%s", w.Code, truncateForTest(w.Body.String()))
					}
					if attempts != 1 {
						t.Fatalf("recovery created extra upstream calls: %d", attempts)
					}
				})
			}
		}
	}
}

// Seed a real resolver binding ending at the assistant Read call. The incoming
// request then sends only the tool result upstream. Its identity must come from
// the omitted prefix rather than from the result-only slice.
func TestReadGutterOnResolvedIncrementHTTPPath(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			s := newAnswerRetryServer(t)
			s.settings.v.ToolPlanningMode = "native"
			messages := ensureRuntimeWorkspaceInstruction([]oaiMsg{
				{Role: "user", Content: "EARLIER_USER_SENTINEL: read a.ts"},
				fidelityToolCall("prior-read", "Read", `{"file_path":"a.ts"}`),
				{Role: "tool", ToolCallID: "prior-read", Content: "50\t\tline one\n51\t\tline two"},
			})
			body := oaiReq{Model: "gpt-5.6-sol", Stream: stream, Messages: messages, Tools: []chathub.Tool{{Type: "function", Function: json.RawMessage(mustJSON(editRecoveryTools()[1]["function"]))}}, ToolChoice: "auto"}
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(mustJSON(body)))
			r.Header.Set(internalCallHeader, "read-increment-test")
			r.Header.Set("X-M365-Session-Id", "increment-session")
			s.sessionResolver.Bind("increment-session", "increment-conversation", s.tokens.List()[0].ID, &oaiReq{Messages: messages[:len(messages)-1]}, "", r)
			output := "```Read\n{\"file_path\":\"a.ts\"}\n```"
			oldAnswer, oldStream := answerChat, streamRecoveryChatWithEvents
			t.Cleanup(func() { answerChat = oldAnswer; streamRecoveryChatWithEvents = oldStream })
			attempts := 0
			inspect := func(req chathub.Request) {
				t.Helper()
				attempts++
				if req.ConversationID != "increment-conversation" {
					t.Fatal("resolver binding did not reach upstream")
				}
				if strings.Contains(req.Text, "EARLIER_USER_SENTINEL") {
					t.Fatal("test did not exercise the incremental slice")
				}
				if !strings.Contains(req.Text, "50→\tline one\n51→\tline two") {
					t.Fatal("result-only increment lost Read identity")
				}
			}
			answerChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, req chathub.Request) (chathub.Result, error) {
				inspect(req)
				return chathub.Result{Text: output}, nil
			}
			streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, _ string, _ chathub.Account, req chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
				inspect(req)
				if err := onEvent(chathub.StreamEvent{Kind: "text", Text: output}); err != nil {
					return chathub.Result{}, err
				}
				return chathub.Result{Text: output}, nil
			}
			w := httptest.NewRecorder()
			s.openaiChat(w, r)
			if w.Code != 200 || attempts != 1 {
				t.Fatalf("status=%d attempts=%d body=%s", w.Code, attempts, truncateForTest(w.Body.String()))
			}
		})
	}
}

func TestEditRecoveryBoundsEscapedPathExpansion(t *testing.T) {
	messages := []oaiMsg{{Role: "user", Content: "edit files"}}
	batch := oaiMsg{Role: "assistant"}
	for i := 0; i < editRecoveryMaxFiles; i++ {
		path := fmt.Sprint(i) + strings.Repeat("\x00", editRecoveryPathLimit-2)
		call := fidelityToolCall(fmt.Sprint(i), "Edit", mustJSON(map[string]string{"file_path": path}))
		batch.ToolCalls = append(batch.ToolCalls, call.ToolCalls...)
	}
	messages = append(messages, batch)
	for i := 0; i < editRecoveryMaxFiles; i++ {
		messages = append(messages, oaiMsg{Role: "tool", ToolCallID: fmt.Sprint(i), Content: "String to replace not found"})
	}
	got := editRecoveryInstruction(messages, buildAgentLedger(messages), editRecoveryTools(), "auto")
	if len(got) > 4096 || strings.Contains(got, `\u0000`) {
		t.Fatalf("escaped metadata bypassed the bound: %d bytes", len(got))
	}
}
