package web

import (
	"encoding/json"
	"testing"
)

func TestValidateDetectedToolCallsRejectsUndeclaredName(t *testing.T) {
	calls := []detectedToolCall{{
		ID:        "tool_call_0",
		Name:      "unknown_tool",
		Arguments: json.RawMessage(`{"path":"E:\\SoarClient-fork","pattern":"*.jsonl"}`),
	}}

	valid, rejected := validateDetectedToolCalls(calls, testTools(), "auto")
	if len(valid) != 0 {
		t.Fatalf("undeclared call escaped validation: %#v", valid)
	}
	if len(rejected) != 1 || rejected[0].Name != "unknown_tool" {
		t.Fatalf("rejected=%#v", rejected)
	}
}

func TestValidateDetectedToolCallsRejectsInvalidArguments(t *testing.T) {
	calls := []detectedToolCall{{
		ID:        "tool_call_0",
		Name:      "get_weather",
		Arguments: json.RawMessage(`{"city":2}`),
	}}

	valid, rejected := validateDetectedToolCalls(calls, testTools(), "auto")
	if len(valid) != 0 || len(rejected) != 1 {
		t.Fatalf("valid=%#v rejected=%#v", valid, rejected)
	}
}

func TestValidateDetectedToolCallsAcceptsDeclaredCall(t *testing.T) {
	calls := []detectedToolCall{{
		Name:      "get_weather",
		Arguments: json.RawMessage(`{"city":"Paris"}`),
	}}

	valid, rejected := validateDetectedToolCalls(calls, testTools(), "auto")
	if len(rejected) != 0 || len(valid) != 1 {
		t.Fatalf("valid=%#v rejected=%#v", valid, rejected)
	}
	if valid[0].ID == "" || valid[0].Type != "function" {
		t.Fatalf("call was not normalized: %#v", valid[0])
	}
}

func TestParseNaturalToolDecisionRejectsBadSchema(t *testing.T) {
	calls, parsed := parseModelToolDecision(`CALL_TOOL: get_weather({"city":2})`, testTools(), "auto")
	if parsed || len(calls) != 0 {
		t.Fatalf("calls=%#v parsed=%v", calls, parsed)
	}
}

func TestValidateDetectedToolCallsCanonicalizesAndDeduplicatesTerminalFrames(t *testing.T) {
	calls := []detectedToolCall{
		{ID: "call_replayed", Type: "fabricated", Name: "GET_WEATHER", Arguments: json.RawMessage(`{"city":"Paris"}`)},
		{ID: "call_replayed", Name: "get_weather", Arguments: json.RawMessage(` { "city" : "Paris" } `)},
	}
	valid, rejected := validateDetectedToolCalls(calls, testTools(), "auto")
	if len(rejected) != 0 || len(valid) != 1 {
		t.Fatalf("valid=%#v rejected=%#v", valid, rejected)
	}
	if valid[0].Name != "get_weather" || valid[0].Type != "function" || string(valid[0].Arguments) != `{"city":"Paris"}` {
		t.Fatalf("call was not canonicalized: %#v", valid[0])
	}
}

func TestValidateDetectedToolCallsRejectsEmptyNameWithoutDroppingValidEmptyArgs(t *testing.T) {
	calls := []detectedToolCall{
		{Name: "", Arguments: json.RawMessage(`{}`)},
		{Name: "get_time", Arguments: json.RawMessage(` null `)},
	}
	valid, rejected := validateDetectedToolCalls(calls, testTools(), "auto")
	if len(rejected) != 1 || rejected[0].Name != "" {
		t.Fatalf("empty name was not rejected: %#v", rejected)
	}
	if len(valid) != 1 || valid[0].Name != "get_time" || string(valid[0].Arguments) != `{}` {
		t.Fatalf("valid empty-argument call was lost: %#v", valid)
	}
}
