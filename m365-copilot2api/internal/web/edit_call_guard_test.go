package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func failedEditCandidate() detectedToolCall {
	return detectedToolCall{ID: "next-edit", Name: "Edit", Arguments: json.RawMessage(`{"file_path":"a.ts","old_string":"\t\told","new_string":"\tnew"}`)}
}

func TestEditGuardRequiresLaterSuccessfulRead(t *testing.T) {
	base := editRecoveryMessages("String to replace not found in file.")
	for _, tc := range []struct {
		name, file, result string
		allow              bool
	}{
		{"no read", "", "", false},
		{"other file", "b.ts", "1\told", false},
		{"read failed", "a.ts", "Error: file not found", false},
		{"read error envelope", "a.ts", "<tool_use_error>File does not exist.</tool_use_error>", false},
		{"fresh successful read", "a.ts", "1\t\told", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := append([]oaiMsg(nil), base...)
			if tc.file != "" {
				messages = append(messages, fidelityToolCall("read2", "Read", mustJSON(map[string]string{"file_path": tc.file})), oaiMsg{Role: "tool", ToolCallID: "read2", Content: tc.result})
			}
			candidate := failedEditCandidate()
			out, err := newEditCallGuard(messages).prepare([]detectedToolCall{candidate}, editRecoveryTools(), "auto")
			if err != nil || len(out) != 1 {
				t.Fatalf("calls=%v err=%v", out, err)
			}
			want := "Read"
			if tc.allow {
				want = "Edit"
			}
			if out[0].Name != want {
				t.Fatalf("tool=%s want %s", out[0].Name, want)
			}
			if !tc.allow && out[0].ID == candidate.ID {
				t.Fatal("recovery reused failed edit id")
			}
		})
	}
}

func TestEditGuardDoesNotUnlockFromSameBatch(t *testing.T) {
	messages := editRecoveryMessages("String to replace not found in file.")
	read := fidelityToolCall("concurrent-read", "Read", `{"file_path":"a.ts"}`)
	messages[3].ToolCalls = append(messages[3].ToolCalls, read.ToolCalls...)
	messages = append(messages, oaiMsg{Role: "tool", ToolCallID: "concurrent-read", Content: "1\told"})
	out, err := newEditCallGuard(messages).prepare([]detectedToolCall{failedEditCandidate()}, editRecoveryTools(), "auto")
	if err != nil || len(out) != 1 || out[0].Name != "Read" {
		t.Fatalf("concurrent read cleared stale edit: %v %v", out, err)
	}
}

func TestEditGuardSurvivesCommentaryAndDedupeOverride(t *testing.T) {
	messages := editRecoveryMessages("String to replace not found in file.")
	messages = append(messages, oaiMsg{Role: "assistant", Content: "Checking again."}, oaiMsg{Role: "user", Content: "Continue."})
	candidate := failedEditCandidate()
	candidates := dedupeCompletedCalls("guard-test", []detectedToolCall{candidate}, buildAgentLedger(messages))
	out, err := newEditCallGuard(messages).prepare(candidates, editRecoveryTools(), "auto")
	if err != nil || len(out) != 1 || out[0].Name != "Read" {
		t.Fatalf("guard bypassed: %v %v", out, err)
	}
}

func TestEditGuardHonoursToolChoiceAndSchema(t *testing.T) {
	guard := newEditCallGuard(editRecoveryMessages("String to replace not found in file."))
	for _, choice := range []any{"none", map[string]any{"type": "function", "function": map[string]any{"name": "Edit"}}} {
		out, err := guard.prepare([]detectedToolCall{failedEditCandidate()}, editRecoveryTools(), choice)
		if err == nil || len(out) != 0 {
			t.Fatalf("forbidden recovery escaped tool_choice: %v %v", out, err)
		}
	}
	if _, err := guard.prepare([]detectedToolCall{failedEditCandidate()}, editTools(), "auto"); err == nil {
		t.Fatal("missing Read did not fail closed")
	}
	read := map[string]any{"type": "function", "function": map[string]any{"name": "Read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"}}, "required": []any{"file_path", "offset"}}}}
	if _, err := guard.prepare([]detectedToolCall{failedEditCandidate()}, append(editTools(), read), "auto"); err == nil {
		t.Fatal("recovery ignored required Read arguments")
	}
}

func TestEditGuardPreservesUnrelatedToolsAndRetries(t *testing.T) {
	base := editRecoveryMessages("String to replace not found in file.")
	candidate := failedEditCandidate()
	other := candidate
	other.Arguments = json.RawMessage(strings.ReplaceAll(string(other.Arguments), "a.ts", "b.ts"))
	out, err := newEditCallGuard(base).prepare([]detectedToolCall{candidate, other}, editRecoveryTools(), "auto")
	if err != nil || len(out) != 2 || out[0].Name != "Read" || out[1].Name != "Edit" {
		t.Fatalf("unrelated edit lost: %v %v", out, err)
	}
	base = append(base, fidelityToolCall("read2", "Read", `{"file_path":"a.ts"}`), oaiMsg{Role: "tool", ToolCallID: "read2", Content: "1\told"})
	kept := filterCompletedCalls([]detectedToolCall{candidate, other}, buildAgentLedger(base))
	out, err = newEditCallGuard(base).prepare(kept, editRecoveryTools(), "auto")
	if err != nil || len(out) != 2 || out[0].Name != "Edit" {
		t.Fatalf("legal retry after Read lost: %v %v", out, err)
	}
}

func TestEditGuardDoesNotDuplicateSelectedRead(t *testing.T) {
	guard := newEditCallGuard(editRecoveryMessages("String to replace not found in file."))
	read := detectedToolCall{ID: "selected-read", Name: "Read", Arguments: json.RawMessage(`{"file_path":"a.ts"}`)}
	edit := failedEditCandidate()
	for _, calls := range [][]detectedToolCall{{read, edit}, {edit, read}} {
		out, err := guard.prepare(calls, editRecoveryTools(), "auto")
		if err != nil || len(out) != 1 || out[0].ID != read.ID {
			t.Fatalf("selected Read duplicated: %v %v", out, err)
		}
	}
}

func TestEditGuardWindowsPathAliases(t *testing.T) {
	base := editRecoveryMessages("String to replace not found in file.")
	base[3] = fidelityToolCall("edit1", "Edit", `{"file_path":"C:\\work\\a.ts","old_string":"a","new_string":"b"}`)
	candidate := detectedToolCall{Name: "Edit", Arguments: json.RawMessage(`{"file_path":"c:/work/./a.ts","old_string":"a","new_string":"c"}`)}
	out, err := newEditCallGuard(base).prepare([]detectedToolCall{candidate}, editRecoveryTools(), "auto")
	if err != nil || len(out) != 1 || out[0].Name != "Read" {
		t.Fatalf("Windows alias bypassed recovery: %v %v", out, err)
	}
}
