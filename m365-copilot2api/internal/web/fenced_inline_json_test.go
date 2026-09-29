package web

import (
	"testing"
)

func TestFencedInlineJSONParameters(t *testing.T) {
	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "multi_agent_v1__spawn_agent",
				"description": "spawn agent",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fork_context": map[string]any{"type": "boolean"},
						"message":      map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	tests := []struct {
		name     string
		text     string
		wantCall bool
		wantName string
	}{
		{
			name:     "inline with simple JSON",
			text:     "```multi_agent_v1__spawn_agent({\"fork_context\":true})\n```",
			wantCall: true,
			wantName: "multi_agent_v1__spawn_agent",
		},
		{
			name:     "inline with nested JSON",
			text:     "```multi_agent_v1__spawn_agent({\"fork_context\":true,\"message\":\"test {nested} content\"})\n```",
			wantCall: true,
			wantName: "multi_agent_v1__spawn_agent",
		},
		{
			name:     "inline without closing newline",
			text:     "```multi_agent_v1__spawn_agent({\"fork_context\":true})```",
			wantCall: true,
			wantName: "multi_agent_v1__spawn_agent",
		},
		{
			name: "inline with long message",
			text: `本轮先重开一个严格只读的反方子组：

` + "```multi_agent_v1__spawn_agent({\"fork_context\":true,\"message\":\"你是第7章只读反方审读者，不是写作者。严禁编写、改写或续写正文。\"})\n```",
			wantCall: true,
			wantName: "multi_agent_v1__spawn_agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := fencedToolCalls(tt.text, tools, "auto")
			if tt.wantCall {
				if len(calls) == 0 {
					t.Fatalf("expected to extract tool call, got none. Text: %q", tt.text[:min(len(tt.text), 100)])
				}
				if calls[0].Name != tt.wantName {
					t.Errorf("expected name %q, got %q", tt.wantName, calls[0].Name)
				}
				if len(calls[0].Arguments) == 0 {
					t.Error("extracted call has empty arguments")
				}
			} else {
				if len(calls) > 0 {
					t.Errorf("expected no calls, got %d: %+v", len(calls), calls)
				}
			}
		})
	}
}

func TestExtractBalancedBody(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		opener byte
		want   string
	}{
		{
			name:   "simple JSON",
			text:   `"fork_context":true}` + "```",
			opener: '{',
			want:   `{"fork_context":true}`,
		},
		{
			name:   "nested braces",
			text:   `"a":{"b":"c"}}` + "```",
			opener: '{',
			want:   `{"a":{"b":"c"}}`,
		},
		{
			name:   "parenthesized JSON",
			text:   "{\"fork_context\":true})```",
			opener: '(',
			want:   "{\"fork_context\":true}",
		},
		{
			name:   "escaped quote",
			text:   "\"msg\":\"say \\\"hello\\\"\"}```",
			opener: '{',
			want:   "{\"msg\":\"say \\\"hello\\\"\"}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBalancedBody(tt.text, tt.opener)
			if got != tt.want {
				t.Errorf("extractBalancedBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
