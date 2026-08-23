package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The poisoned pair must clear the tool-protocol gate on the real request path,
// not merely in a unit test of the sanitizer. The request still fails later --
// there is no upstream account in a test process -- so this asserts only that
// it is no longer rejected as a protocol error.
func TestChatSanitizesPoisonedToolHistory(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"","type":"function","function":{"name":"","arguments":""}}]},
		{"role":"tool","tool_call_id":"","content":""},
		{"role":"user","content":"carry on"}
	]}`
	s := &Server{sessionResolver: openSessionResolver()}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if strings.Contains(w.Body.String(), "tool_protocol_error") {
		t.Fatalf("poisoned history still rejected as protocol error: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "missing id at index") {
		t.Fatalf("the 400 is still reachable: %s", w.Body.String())
	}
}

// Sanitizing must not blunt the real guard: a well-formed call with no matching
// result is a genuine client bug and must still be refused.
func TestChatStillRejectsUnansweredToolCall(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"user","content":"carry on"}
	]}`
	s := &Server{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "tool_protocol_error") {
		t.Fatalf("unanswered tool call was not refused: status=%d body=%s", w.Code, w.Body.String())
	}
}
