package chathub

import "testing"

func TestEmitUpdateEventsDeduplicatesNativeToolAcrossArgumentAndMessages(t *testing.T) {
	nestedTool := map[string]any{
		"functionName":      "list_files",
		"functionArguments": map[string]any{"path": "."},
	}
	messageTool := map[string]any{
		"functionName":      "list_files",
		"functionArguments": map[string]any{"path": "."},
	}
	arg := map[string]any{
		"plugin": nestedTool,
		"messages": []any{
			map[string]any{
				"text":                "checking files",
				"messageType":         "Progress",
				"contentOrigin":       "ChainOfThoughtSummary",
				"addToChainOfThought": true,
			},
			messageTool,
		},
	}

	var got []StreamEvent
	err := emitUpdateEvents(arg, arg["messages"].([]any), map[string]bool{}, func(event StreamEvent) error {
		got = append(got, event)
		return nil
	})
	if err != nil {
		t.Fatalf("emit update events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("same native tool reached handler %d times: %#v", len(got), got)
	}
	if got[0].Kind != "tool" || got[0].ToolName != "list_files" {
		t.Fatalf("unexpected handler event: %#v", got[0])
	}
}

func TestEmitUpdateEventsKeepsDistinctNativeTools(t *testing.T) {
	first := map[string]any{
		"functionName":      "list_files",
		"functionArguments": map[string]any{"path": "."},
	}
	second := map[string]any{
		"functionName":      "read_file",
		"functionArguments": map[string]any{"path": "README.md"},
	}
	arg := map[string]any{
		"plugin":   first,
		"messages": []any{first, second},
	}

	var got []StreamEvent
	err := emitUpdateEvents(arg, arg["messages"].([]any), map[string]bool{}, func(event StreamEvent) error {
		got = append(got, event)
		return nil
	})
	if err != nil {
		t.Fatalf("emit update events: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("distinct native tools did not both reach handler: %#v", got)
	}
	if got[0].ToolName != "list_files" || got[1].ToolName != "read_file" {
		t.Fatalf("unexpected handler events: %#v", got)
	}
}
