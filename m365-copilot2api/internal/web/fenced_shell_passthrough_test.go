package web

import (
	"encoding/json"
	"testing"
)

// omp-style bash schema requires ["command","i"]; the fence conversion must
// pass through every key the model wrote instead of rebuilding a whitelist.
func TestBashFencePassesRequiredFields(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"i":       map[string]any{"type": "string"},
					"cwd":     map[string]any{"type": "string"},
				},
				"required": []any{"command", "i"},
			},
		}},
	}
	text := "```bash\n" + `{"command":"ls","i":"list files","cwd":"E:\\x"}` + "\n```"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if args["command"] != "ls" {
		t.Fatalf("command should be the plain value, got: %#v", args["command"])
	}
	if args["i"] != "list files" {
		t.Fatalf("i was dropped: %v", args)
	}
	if args["cwd"] != `E:\x` {
		t.Fatalf("cwd was dropped: %v", args)
	}
}
