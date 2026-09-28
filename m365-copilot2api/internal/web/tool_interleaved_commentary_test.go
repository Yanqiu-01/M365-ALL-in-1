package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeToolHistoryMergesInterleavedCommentary(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "run two commands"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call-1", "function": map[string]any{"name": "exec", "arguments": "ls"}}}},
		{Role: "assistant", Content: "I will check the directory first."},
		{Role: "tool", ToolCallID: "call-1", Content: "file1.txt\nfile2.txt"},
		{Role: "assistant", Content: "The directory contains two files."},
	}
	normalized := normalizeToolHistory(messages)
	if len(normalized) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(normalized), normalized)
	}
	if normalized[1].Role != "assistant" || len(normalized[1].ToolCalls) == 0 {
		t.Fatal("tool call was not preserved")
	}
	if !messageHasContent(normalized[1].Content) || !contentContains(normalized[1].Content, "check the directory") {
		t.Fatalf("commentary was not merged: %v", normalized[1].Content)
	}
	if err := validateToolConversation(normalized); err != nil {
		t.Fatalf("normalized history rejected: %v", err)
	}
}

func contentContains(c any, needle string) bool {
	return strings.Contains(contentToString(c), needle)
}

func TestValidateAllowsNormalizedHistory(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "run two commands"},
		{Role: "assistant", Content: "I will check the directory first.", ToolCalls: []map[string]any{{"id": "call-1", "function": map[string]any{"name": "exec", "arguments": "ls"}}}},
		{Role: "tool", ToolCallID: "call-1", Content: "file1.txt\nfile2.txt"},
		{Role: "assistant", Content: "The directory contains two files."},
	}
	if err := validateToolConversation(messages); err != nil {
		t.Fatalf("validator rejected legitimate normalized history: %v", err)
	}
}

func TestValidateRejectsCallWithoutResult(t *testing.T) {
	messages := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call-1", "function": map[string]any{"name": "exec"}}}},
		{Role: "assistant", Content: "next turn without result"},
	}
	if err := validateToolConversation(messages); err == nil || !strings.Contains(err.Error(), "tool results missing") {
		t.Fatalf("validator allowed missing result: %v", err)
	}
}

func TestAnthropicSSETextBlockStartIsEmpty(t *testing.T) {
	src := map[string]any{
		"id": "chatcmpl-test",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "This is the complete answer.",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
	body := strings.Builder{}
	writeAnthropicResult(&testResponseWriter{w: &body}, "test-model", true, src)
	var startText, deltaText string
	for _, line := range strings.Split(body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
			continue
		}
		if e["type"] == "content_block_start" {
			if block, ok := e["content_block"].(map[string]any); ok && block["type"] == "text" {
				startText, _ = block["text"].(string)
			}
		}
		if e["type"] == "content_block_delta" {
			if delta, ok := e["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
				deltaText, _ = delta["text"].(string)
			}
		}
	}
	if startText != "" {
		t.Fatalf("text block start was not empty: %q", startText)
	}
	if deltaText != "This is the complete answer." {
		t.Fatalf("delta text incorrect: %q", deltaText)
	}
}

type testResponseWriter struct {
	w          *strings.Builder
	headerSent bool
}

func (t *testResponseWriter) Header() http.Header { return http.Header{} }
func (t *testResponseWriter) Write(b []byte) (int, error) {
	return t.w.Write(b)
}
func (t *testResponseWriter) WriteHeader(int) {}

func TestResponsesInputWithInterleavedCommentary(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "run ls"}}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "exec_command", "arguments": map[string]any{"command": "ls"}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Checking directory."}}},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "file.txt"},
	}
	req := responsesRequest{Input: input}
	body, err := req.openAI()
	if err != nil {
		t.Fatal(err)
	}
	body.Messages = normalizeToolHistory(body.Messages)
	if err := validateToolConversation(body.Messages); err != nil {
		t.Fatalf("responses input with commentary rejected: %v", err)
	}
	foundCommentary := false
	for _, m := range body.Messages {
		if m.Role == "assistant" && strings.Contains(contentToString(m.Content), "Checking") {
			foundCommentary = true
		}
	}
	if !foundCommentary {
		t.Fatal("commentary was not preserved during conversion")
	}
}
