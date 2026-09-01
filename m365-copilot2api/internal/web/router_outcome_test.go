package web

import "testing"

func TestRouterOutcomeFieldsAreContentFreeAndClassified(t *testing.T) {
	o := newRouterOutcome("request-1", "router", 30, "auto", true)
	o.observeParsed(true, 3)
	o.observeValidated(2, 1, 1)
	fields := o.fields("validated", "emitted_tool_calls")

	want := map[string]any{
		"router_stage": "router", "router_substage": "validated", "disposition": "emitted_tool_calls",
		"declared_tools": 30, "tool_choice": "auto", "parsed": true,
		"raw_candidates": 3, "post_ledger": 2, "valid_calls": 1, "rejected_calls": 1, "tool_intent": true,
	}
	if len(fields) != len(want) {
		t.Fatalf("unexpected field count: got %d want %d (%#v)", len(fields), len(want), fields)
	}
	for key, value := range want {
		if fields[key] != value {
			t.Errorf("fields[%q] = %#v, want %#v", key, fields[key], value)
		}
	}
	for _, forbidden := range []string{"prompt", "text", "schema", "arguments", "tool_name", "account", "result"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("diagnostic fields must not contain %q", forbidden)
		}
	}
}

func TestRouterOutcomeNormalizesToolChoice(t *testing.T) {
	o := newRouterOutcome("request-1", "router", 1, map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}, false)
	if got := o.fields("parsed", "ordinary_answer_fallback")["tool_choice"]; got != "named:read_file" {
		t.Fatalf("tool choice = %#v, want named:read_file", got)
	}
}
