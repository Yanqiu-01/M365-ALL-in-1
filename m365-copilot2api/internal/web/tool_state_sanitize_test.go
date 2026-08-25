package web

import "testing"

// The pair a relay leaves behind when it translates finish_reason but loses the
// tool_calls payload: an assistant call with no id and no name, answered by a
// tool result with an empty tool_call_id. The client replays both forever.
func poisonedHistory() []oaiMsg {
	return []oaiMsg{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: nil, ToolCalls: []map[string]any{
			{"id": "", "type": "function", "function": map[string]any{"name": "", "arguments": ""}},
		}},
		{Role: "tool", ToolCallID: "", Content: ""},
		{Role: "user", Content: "carry on"},
	}
}

func TestValidateRejectsPoisonedHistory(t *testing.T) {
	if err := validateToolConversation(poisonedHistory()); err == nil {
		t.Fatal("validator accepted an empty-id tool call; the 400 guard is gone")
	}
}

func TestSanitizeRecoversPoisonedHistory(t *testing.T) {
	cleaned, notes := sanitizeToolConversation(poisonedHistory())
	// three events: the empty call, the assistant turn it left hollow, the orphan result
	if len(notes) != 3 {
		t.Fatalf("want 3 drops, got %d: %v", len(notes), notes)
	}
	if len(cleaned) != 2 {
		t.Fatalf("want the 2 user turns kept, got %d: %#v", len(cleaned), cleaned)
	}
	if err := validateToolConversation(cleaned); err != nil {
		t.Fatalf("sanitized history still fails validation: %v", err)
	}
}

func TestSanitizeKeepsWellFormedCalls(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather"}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
	}
	cleaned, notes := sanitizeToolConversation(msgs)
	if len(notes) != 0 {
		t.Fatalf("healthy history was modified: %v", notes)
	}
	if len(cleaned) != 2 || len(cleaned[0].ToolCalls) != 1 {
		t.Fatalf("cleaned=%#v", cleaned)
	}
	if err := validateToolConversation(cleaned); err != nil {
		t.Fatalf("healthy history failed validation: %v", err)
	}
}

// A partially-bad assistant turn must lose only the bad call, and must survive
// as a message when it still carries prose.
func TestSanitizeDropsOnlyTheBadCall(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", Content: "checking", ToolCalls: []map[string]any{
			{"id": "call_ok", "type": "function", "function": map[string]any{"name": "get_weather"}},
			{"id": "", "type": "function", "function": map[string]any{"name": ""}},
		}},
		{Role: "tool", ToolCallID: "call_ok", Content: "sunny"},
	}
	cleaned, notes := sanitizeToolConversation(msgs)
	if len(notes) != 1 {
		t.Fatalf("want 1 drop, got %d: %v", len(notes), notes)
	}
	if len(cleaned) != 2 || len(cleaned[0].ToolCalls) != 1 {
		t.Fatalf("cleaned=%#v", cleaned)
	}
	if id, _ := cleaned[0].ToolCalls[0]["id"].(string); id != "call_ok" {
		t.Fatalf("wrong call survived: %#v", cleaned[0].ToolCalls)
	}
	if err := validateToolConversation(cleaned); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
}

// A named call with no id is equally unmatchable and must not slip through.
func TestSanitizeDropsNamedCallMissingID(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "  ", "type": "function", "function": map[string]any{"name": "get_weather"}},
		}},
	}
	cleaned, notes := sanitizeToolConversation(msgs)
	if len(notes) != 2 || len(cleaned) != 0 {
		t.Fatalf("notes=%v cleaned=%#v", notes, cleaned)
	}
}
