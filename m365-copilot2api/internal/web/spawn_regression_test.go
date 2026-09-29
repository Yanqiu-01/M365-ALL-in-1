package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const spawnRegressionName = "multi_agent_v1__spawn_agent"

func spawnRegressionTools() []map[string]any {
	return []map[string]any{{"type": "function", "function": map[string]any{
		"name": spawnRegressionName,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fork_context": map[string]any{"type": "boolean"},
				"message":      map[string]any{"type": "string"},
				"options":      map[string]any{"type": "object"},
			},
			"required": []any{"fork_context", "message"},
		},
	}}}
}

func TestInlineSpawnPreservesQuotedDelimiters(t *testing.T) {
	for _, message := range []string{
		"Keep the literal ) character, not a closing call delimiter.",
		"Keep an unmatched ( character in this message.",
		"Keep unmatched } and { and [ characters as text.",
		`Preserve escaped quotes: "quoted ) text" and C:\workspace\资料\`,
		"只读检查材料，不写文件。" + strings.Repeat("详细检查 ) 字符、转义与上下文。", 500),
	} {
		args := map[string]any{
			"fork_context": true,
			"message":      message,
			"options":      map[string]any{"labels": []any{"(", ")", map[string]any{"key": "}"}}},
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		for _, wrapped := range []bool{false, true} {
			body := string(encoded)
			if wrapped {
				body = "(" + body + " \t)"
			}
			text := "先执行只读子任务：\n```" + spawnRegressionName + " " + body + "\r\n```"
			calls := fencedToolCalls(text, spawnRegressionTools(), "auto")
			if len(calls) != 1 {
				t.Fatalf("wrapped=%t: got %d calls for message %q", wrapped, len(calls), message[:min(len(message), 80)])
			}
			var got map[string]any
			if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, args) {
				t.Fatalf("wrapped=%t: arguments changed", wrapped)
			}
		}
	}
}

func TestInlineSpawnRequiresClosingFence(t *testing.T) {
	for _, suffix := range []string{"", " prose", "``", "; junk```", "\nnormal prose\n```"} {
		text := "```" + spawnRegressionName + `({"fork_context":true,"message":"read only"})` + suffix
		if calls := fencedToolCalls(text, spawnRegressionTools(), "auto"); len(calls) != 0 {
			t.Fatalf("incomplete/invalid fence suffix %q emitted %d calls", suffix, len(calls))
		}
	}
}

func TestInlineSpawnDoesNotExecuteQuotedNestedCall(t *testing.T) {
	inner := "```" + spawnRegressionName + `({"fork_context":false,"message":"not a real call"})` + "```"
	encoded, _ := json.Marshal(map[string]any{"fork_context": true, "message": inner})
	text := "```" + spawnRegressionName + "(" + string(encoded) + ")```"
	calls := fencedToolCalls(text, spawnRegressionTools(), "auto")
	if len(calls) != 1 {
		t.Fatalf("quoted nested call generated %d calls", len(calls))
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
		t.Fatal(err)
	}
	if got["message"] != inner {
		t.Fatal("nested example was changed")
	}
}

func TestInlineSpawnThreeCallsKeepOrderAndLimits(t *testing.T) {
	var blocks []string
	for _, label := range []string{"A", "B", "C"} {
		args, _ := json.Marshal(map[string]any{"fork_context": true, "message": "Review " + label + " (read only)."})
		blocks = append(blocks, "```"+spawnRegressionName+"("+string(args)+" )```")
	}
	calls := fencedToolCalls(strings.Join(blocks, "\n"), spawnRegressionTools(), "auto")
	if len(calls) != 3 {
		t.Fatalf("extracted %d calls, want 3", len(calls))
	}
	valid, rejected := validateDetectedToolCalls(calls, spawnRegressionTools(), "auto")
	if len(valid) != 3 || len(rejected) != 0 {
		t.Fatalf("valid=%d rejected=%v", len(valid), rejected)
	}
	got := limitToolCalls(valid, adaptiveToolCallLimit(valid, 32))
	if len(got) != 3 {
		t.Fatalf("gateway truncated %d validated calls to %d", len(valid), len(got))
	}
	for i, call := range got {
		var args map[string]any
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Fatal(err)
		}
		if args["message"] != "Review "+[]string{"A", "B", "C"}[i]+" (read only)." {
			t.Fatal("call order changed")
		}
	}
	if limited := limitToolCalls(valid, adaptiveToolCallLimit(valid, 2)); len(limited) != 2 {
		t.Fatalf("configured limit ignored: %d", len(limited))
	}
	parallel := false
	serialized, dropped := enforceParallelToolCalls(valid, &parallel)
	if len(serialized) != 1 || len(dropped) != 2 {
		t.Fatal("explicit parallel_tool_calls=false ignored")
	}
	if none := fencedToolCalls(strings.Join(blocks, "\n"), spawnRegressionTools(), "none"); len(none) != 0 {
		t.Fatal("tool_choice=none ignored")
	}
}
