package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func editTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "Edit",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
					"new_string": map[string]any{"type": "string"},
				},
				"required": []any{"file_path", "old_string", "new_string"},
			},
		}},
	}
}

func TestInlineFenceParensExtracted(t *testing.T) {
	bt := "```"
	jsonArgs := `{"file_path":"C:\\Users\\ad.md","old_string":"a","new_string":"b"}`
	text := "好的，我来续写：\n" + bt + "Edit(" + jsonArgs + ")\n" + bt
	calls := fencedToolCalls(text, editTools(), "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d (want 1), text=%q", len(calls), text)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	if args["file_path"] != `C:\Users\ad.md` {
		t.Fatalf("file_path=%v", args["file_path"])
	}
}

func TestInlineFenceBraceExtracted(t *testing.T) {
	bt := "```"
	jsonArgs := `{"file_path":"x.md","old_string":"a","new_string":"b"}`
	text := bt + "Edit " + jsonArgs + "\n" + bt
	calls := fencedToolCalls(text, editTools(), "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d (want 1)", len(calls))
	}
}

func TestCanonicalFenceStillSingle(t *testing.T) {
	bt := "```"
	jsonArgs := `{"file_path":"x.md","old_string":"a","new_string":"b"}`
	text := bt + "Edit\n" + jsonArgs + "\n" + bt
	calls := fencedToolCalls(text, editTools(), "auto")
	if len(calls) != 1 {
		t.Fatalf("canonical fence duplicated: %d", len(calls))
	}
}

func TestOrdinaryCodeFenceNotATool(t *testing.T) {
	bt := "```"
	text := bt + "json\n" + `{"command":"ls"}` + "\n" + bt
	calls := fencedToolCalls(text, editTools(), "auto")
	if len(calls) != 0 {
		t.Fatalf("json fence misread as call: %+v", calls)
	}
}

func TestShellCommandDialectNotDamaged(t *testing.T) {
	bt := "```"
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "PowerShell",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}},
		}},
	}
	text := bt + "PowerShell\n" + `{"command":":GetFolderPath('Desktop')"}` + "\n" + bt
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	if !strings.Contains(args["command"].(string), "[System.Environment]::GetFolderPath") {
		t.Fatalf("repair lost: %v", args["command"])
	}
}
