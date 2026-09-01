package web

import (
	"testing"
	"time"
)

func TestResponseHistoryIsTenantScopedAndReturnsCopies(t *testing.T) {
	s := &Server{}
	nestedContent := map[string]any{
		"nested": map[string]any{"value": "original"},
		"items":  []any{map[string]any{"value": "item-original"}},
	}
	toolCalls := []map[string]any{{
		"id": "call-1",
		"function": map[string]any{
			"name":      "lookup",
			"arguments": `{"q":"original"}`,
		},
	}}
	messages := []oaiMsg{
		{Role: "user", Content: nestedContent},
		{Role: "assistant", ToolCalls: toolCalls},
	}
	s.rememberResponse("tenant-a", "resp-a", messages)

	// Stored history must not share nested maps or slices with the caller.
	nestedContent["nested"].(map[string]any)["value"] = "mutated"
	nestedContent["items"].([]any)[0].(map[string]any)["value"] = "item-mutated"
	toolCalls[0]["id"] = "mutated"
	toolCalls[0]["function"].(map[string]any)["name"] = "mutated"
	messages = append(messages, oaiMsg{Role: "assistant", Content: "later"})

	got, ok := s.loadResponseHistory("tenant-a", "resp-a")
	if !ok || len(got) != 2 {
		t.Fatalf("stored history was not isolated from caller mutation: ok=%t messages=%#v", ok, got)
	}
	content, ok := got[0].Content.(map[string]any)
	if !ok {
		t.Fatalf("stored content type = %T, want map[string]any", got[0].Content)
	}
	nested, ok := content["nested"].(map[string]any)
	if !ok || nested["value"] != "original" {
		t.Fatalf("nested content was not copied: %#v", content)
	}
	items, ok := content["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("nested content items were not copied: %#v", content)
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["value"] != "item-original" {
		t.Fatalf("nested item was not copied: %#v", items)
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0]["id"] != "call-1" {
		t.Fatalf("tool call was not copied: %#v", got[1].ToolCalls)
	}
	function, ok := got[1].ToolCalls[0]["function"].(map[string]any)
	if !ok || function["name"] != "lookup" {
		t.Fatalf("nested tool call was not copied: %#v", got[1].ToolCalls)
	}
	if _, ok := s.loadResponseHistory("tenant-b", "resp-a"); ok {
		t.Fatal("a response id must not resolve across tenants")
	}

	// The reader must also receive a detached copy.
	nested["value"] = "changed by reader"
	function["name"] = "changed by reader"
	again, ok := s.loadResponseHistory("tenant-a", "resp-a")
	if !ok {
		t.Fatal("stored response disappeared after reader mutation")
	}
	againContent := again[0].Content.(map[string]any)
	againNested := againContent["nested"].(map[string]any)
	againFunction := again[1].ToolCalls[0]["function"].(map[string]any)
	if againNested["value"] != "original" || againFunction["name"] != "lookup" {
		t.Fatalf("reader received a live store reference: %#v", again)
	}
}

func TestResponseHistoryExpiresOnRead(t *testing.T) {
	s := &Server{responseMessages: map[string]map[string]respHistory{
		"tenant": {
			"expired": {At: time.Now().Add(-time.Hour - time.Second), Messages: []oaiMsg{{Role: "user", Content: "old"}}},
		},
	}}
	if _, ok := s.loadResponseHistory("tenant", "expired"); ok {
		t.Fatal("expired response history must not be reusable")
	}
	if _, exists := s.responseMessages["tenant"]["expired"]; exists {
		t.Fatal("expired response history was not removed on read")
	}
}

func TestResponseHistoryEvictsOldestPerTenant(t *testing.T) {
	s := &Server{}
	base := time.Now().Add(-time.Minute)
	s.responseMessages = map[string]map[string]respHistory{"tenant": {}}
	for i := 0; i < maxResponsesPerTenant; i++ {
		id := "resp-" + string(rune('a'+(i%26))) + ":" + string(rune(i))
		s.responseMessages["tenant"][id] = respHistory{
			At:       base.Add(time.Duration(i) * time.Millisecond),
			Messages: []oaiMsg{{Role: "user", Content: id}},
		}
	}
	oldest := "resp-a:" + string(rune(0))
	s.rememberResponse("tenant", "new", []oaiMsg{{Role: "user", Content: "new"}})
	if _, ok := s.responseMessages["tenant"][oldest]; ok {
		t.Fatalf("oldest response was not evicted: %q still present", oldest)
	}
	if _, ok := s.loadResponseHistory("tenant", "new"); !ok {
		t.Fatal("new response was not stored after eviction")
	}
}

func TestReasoningContentAcceptsInternalAliases(t *testing.T) {
	if got := reasoningContent(map[string]any{"reasoning_content": "summary"}); got != "summary" {
		t.Fatalf("reasoning_content=%q", got)
	}
	if got := reasoningContent(map[string]any{"reasoning": "summary"}); got != "summary" {
		t.Fatalf("reasoning=%q", got)
	}
	if got := reasoningContent(nil); got != "" {
		t.Fatalf("nil message reasoning=%q", got)
	}
}
