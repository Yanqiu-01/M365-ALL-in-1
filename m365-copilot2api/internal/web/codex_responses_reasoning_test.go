package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesResultPreservesReasoningSummary(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.6-reasoning", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"content":           "answer",
			"reasoning_content": "reasoning summary",
		}}},
	}, nil)
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("reasoning output missing: %#v", response)
	}
	reasoning, _ := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("first output is not reasoning: %#v", output)
	}
	summary, _ := reasoning["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("reasoning summary missing: %#v", reasoning)
	}
	part, _ := summary[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "reasoning summary" {
		t.Fatalf("unexpected reasoning summary: %#v", part)
	}
}

func TestStreamingResponsesResultEmitsReasoningEvents(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.6-reasoning", true, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"content":           "answer",
			"reasoning_content": "reasoning summary",
		}}},
	}, nil)
	body := rr.Body.String()
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		`"type":"reasoning"`,
		`"text":"reasoning summary"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q: %s", want, body)
		}
	}
}
