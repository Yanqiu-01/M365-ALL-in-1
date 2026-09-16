package web

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func fidelityToolCall(id, name, arguments string) oaiMsg {
	return oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
		"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments},
	}}}
}

func TestNormalizeReadGutterPreservesContent(t *testing.T) {
	cases := []struct{ name, tool, input, want string }{
		{"indentation", "Read", "  116\t\t\t\t\t<textarea\n  117\t  \tvalue\t", "  116→\t\t\t\t<textarea\n  117→  \tvalue\t"},
		{"CRLF and empty numbered line", "Read", "1\talpha\r\n2\t\r\n3\t\tbeta\r\n", "1→alpha\r\n2→\r\n3→\tbeta\r\n"},
		{"blank separators", "Read", "\n1\ta\n\n2\t\tb\n\n", "\n1→a\n\n2→\tb\n\n"},
		{"single line with tab padding", "read", "\t37\t\tx", "\t37→\tx"},
		{"range starts past one", "Read", "500\tx\n502\ty", "500→x\n502→y"},
		{"reminder is metadata", "Read", "1\tx\n<system-reminder>\n9\tmetadata\n</system-reminder>\n", "1→x\n<system-reminder>\n9\tmetadata\n</system-reminder>\n"},
		{"inline reminder", "Read", "1\tx\n<system-reminder>note</system-reminder>", "1→x\n<system-reminder>note</system-reminder>"},
		{"already normalised", "Read", "1→\tx\n2→y", "1→\tx\n2→y"},
		{"other tool", "Bash", "1\tx\n2\ty", "1\tx\n2\ty"},
		{"unknown read format", "read_file", "1\tx\n2\ty", "1\tx\n2\ty"},
		{"unknown tool", "", "1\tx\n2\ty", "1\tx\n2\ty"},
		{"decreasing", "Read", "2\tx\n1\ty", "2\tx\n1\ty"},
		{"duplicate", "Read", "1\tx\n1\ty", "1\tx\n1\ty"},
		{"zero", "Read", "0\tx", "0\tx"},
		{"overflow", "Read", "999999999999999999999999999999\tx", "999999999999999999999999999999\tx"},
		{"unrecognised header", "Read", "some raw content\n1\tx", "some raw content\n1\tx"},
		{"error", "Read", "Error: no such file\n1\tx", "Error: no such file\n1\tx"},
		{"unfinished reminder", "Read", "1\tx\n<system-reminder>note", "1\tx\n<system-reminder>note"},
		{"empty", "Read", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeReadGutter(tc.tool, tc.input)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if again := normalizeReadGutter(tc.tool, got); again != got {
				t.Fatalf("not idempotent: %q", again)
			}
		})
	}
}

func TestReadGutterCoversFullIncrementAndLedger(t *testing.T) {
	raw := "116\t\t\t\t\t<textarea\n117\t\t\t\t\t</textarea>"
	want := "116→\t\t\t\t<textarea\n117→\t\t\t\t</textarea>"
	messages := []oaiMsg{
		{Role: "user", Content: "edit the textarea"},
		fidelityToolCall("r1", "Read", `{"file_path":"C:\\work\\a.ts"}`),
		{Role: "tool", ToolCallID: "r1", Content: raw},
	}
	original := mustJSON(messages)
	full, _ := flattenPromptMessages(messages, nil)
	names := toolCallNames(messages[:2])
	increment, _ := flattenPromptMessagesWithToolNames(messages[2:], nil, names)
	ledger := buildAgentLedger(messages)
	for label, text := range map[string]string{"full": full, "increment": increment, "ledger": ledger.Completed[0].Result} {
		if !strings.Contains(text, want) {
			t.Errorf("%s lost the normalised listing: %q", label, text)
		}
	}
	if original != mustJSON(messages) {
		t.Fatal("rendering mutated wire messages")
	}
	if ledger.Completed[0].Failed {
		t.Fatal("Read was marked failed")
	}
	// The seed belongs to the caller and can be reused for several increments.
	_, _ = flattenPromptMessagesWithToolNames([]oaiMsg{fidelityToolCall("x", "Bash", `{}`)}, nil, names)
	if len(names) != 1 || names["r1"] != "Read" {
		t.Fatalf("seed map mutated: %v", names)
	}
	// Unknown IDs cannot borrow the identity of an unrelated earlier Read.
	unknown, _ := flattenPromptMessages([]oaiMsg{messages[1], {Role: "tool", ToolCallID: "other", Content: raw}}, nil)
	if !strings.Contains(unknown, raw) {
		t.Fatal("unmatched tool output was rewritten")
	}
}

func TestReadGutterRunsBeforeTruncationAndBudget(t *testing.T) {
	var raw strings.Builder
	for n := 1; n <= 5000; n++ {
		fmt.Fprintf(&raw, "%d\t\tconst sample = value;\n", n)
	}
	call := fidelityToolCall("large", "Read", `{"file_path":"x.ts"}`)
	result := oaiMsg{Role: "tool", ToolCallID: "large", Content: raw.String()}
	messages := []oaiMsg{call, result}
	names := toolCallNames(messages)
	want := compactToolResult(normalizeReadGutter("Read", raw.String()), ledgerResultLimit)
	text, _ := flattenPromptMessages(messages, nil)
	if !strings.Contains(text, want) {
		t.Fatal("flatten normalised only after splitting the listing")
	}
	if got := buildAgentLedger(messages).Completed[0].Result; got != want {
		t.Fatal("ledger and flatten disagree")
	}
	costText, _ := sendableContentTextWithToolNames(result, names)
	if costText != want {
		t.Fatal("budget and send path disagree")
	}
	_, groups := messageGroups(messages, "")
	wantCost := messageTokenCostWithToolNames(call, "", names) + messageTokenCostWithToolNames(result, "", names)
	if len(groups) != 1 || groups[0].cost != wantCost {
		t.Fatal("context trim did not carry tool identity to budgeting")
	}
}

func TestPromptHistoryArgumentsAreObjectsWithoutChangingValues(t *testing.T) {
	arguments := `{"file_path":"C:\\work\\a.ts","old_string":"\tfirst\n\tsecond","new_string":"\\t literal \\n","revision":9007199254740993}`
	message := fidelityToolCall("edit", "Edit", arguments)
	original := mustJSON(message)
	display := promptToolCalls(message.ToolCalls)
	encoded := mustJSON(display)
	var calls []struct {
		Function struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(encoded), &calls); err != nil {
		t.Fatal(err)
	}
	assertArguments := func(raw json.RawMessage) {
		t.Helper()
		var args map[string]json.RawMessage
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("arguments still a string: %s: %v", raw, err)
		}
		if string(args["revision"]) != "9007199254740993" {
			t.Fatal("large integer rounded")
		}
		var old, next string
		_ = json.Unmarshal(args["old_string"], &old)
		_ = json.Unmarshal(args["new_string"], &next)
		if old != "\tfirst\n\tsecond" || next != `\t literal \n` {
			t.Fatalf("inner escaping changed: old=%q new=%q", old, next)
		}
	}
	assertArguments(calls[0].Function.Arguments)
	full, _ := flattenPromptMessages([]oaiMsg{message}, nil)
	if !strings.Contains(full, encoded) {
		t.Fatal("flatten did not use the presentation object")
	}
	ledger := buildAgentLedger([]oaiMsg{message, {Role: "tool", ToolCallID: "edit", Content: "updated"}})
	evidence := strings.SplitN(ledger.RouterContext(), "EVIDENCE_LEDGER: ", 2)[1]
	var context struct {
		Completed []struct {
			Arguments json.RawMessage `json:"arguments"`
			ID        string          `json:"id"`
			Answered  bool            `json:"answered"`
		} `json:"completed"`
	}
	if err := json.Unmarshal([]byte(evidence), &context); err != nil {
		t.Fatal(err)
	}
	assertArguments(context.Completed[0].Arguments)
	if context.Completed[0].ID != "edit" || !context.Completed[0].Answered {
		t.Fatal("evidence identity lost")
	}
	if ledger.Completed[0].Arguments != arguments || original != mustJSON(message) {
		t.Fatal("raw history was mutated")
	}
	display[0]["id"] = "changed"
	display[0]["function"].(map[string]any)["name"] = "changed"
	if original != mustJSON(message) {
		t.Fatal("display maps alias original history")
	}
}

func TestPromptArgumentsKeepInvalidAndNonObjectHistory(t *testing.T) {
	for _, input := range []string{`{"x":`, `null`, `[1,2]`, `"literal"`, `123`, "{\"x\":\"raw\ttab\"}"} {
		if got := promptArguments(input); !reflect.DeepEqual(got, input) {
			t.Errorf("rewrote historical %q into %#v", input, got)
		}
	}
}

func TestReadGutterRetainsDelimiterOnFinalEmptyLine(t *testing.T) {
	for _, raw := range []string{"1\t\n", "1\t\tvalue\n2\t\n", "1\tvalue\r\n2\t\r\n"} {
		for _, content := range []any{raw, []any{map[string]any{"type": "text", "text": raw}}} {
			call := fidelityToolCall("empty-tail", "Read", `{"file_path":"a.ts"}`)
			result := oaiMsg{Role: "tool", ToolCallID: "empty-tail", Content: content}
			messages := []oaiMsg{call, result}
			want := compactToolResult(normalizeReadGutter("Read", raw), ledgerResultLimit)
			flat, _ := flattenPromptMessages(messages, nil)
			ledger := buildAgentLedger(messages)
			budget, _ := sendableContentTextWithToolNames(result, toolCallNames(messages))
			for label, got := range map[string]string{"flatten": flat, "ledger": ledger.Completed[0].Result, "budget": budget} {
				if !strings.Contains(got, want) {
					t.Errorf("%s consumed the final gutter before recognising it: got=%q want=%q", label, got, want)
				}
			}
		}
	}
}
