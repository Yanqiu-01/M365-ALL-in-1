package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countTokensRequest posts a body to the count_tokens handler and returns the
// status plus the decoded input_tokens (0 when the body carried no count).
func countTokensRequest(t *testing.T, body string) (int, int, map[string]any) {
	t.Helper()
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.anthropicCountTokens(w, r)

	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	count := 0
	if v, ok := decoded["input_tokens"].(float64); ok {
		count = int(v)
	}
	return w.Code, count, decoded
}

// The endpoint Claude Code actually calls has to answer 200 with a positive
// count. It answered 404 for the whole of the measured session because the
// route was never registered.
func TestCountTokensAnswersWithAPositiveCount(t *testing.T) {
	status, count, decoded := countTokensRequest(t, `{"model":"m365-copilot","messages":[{"role":"user","content":"hello world"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", status, decoded)
	}
	if count < 1 {
		t.Fatalf("input_tokens = %d, want at least 1", count)
	}
}

// Tool schemas are input the model has to read. A count that ignored them would
// let a client fill the window with history and then be pushed over by its own
// declarations -- 36 tools is not a rounding error.
func TestCountTokensIncludesToolSchemas(t *testing.T) {
	const messages = `"messages":[{"role":"user","content":"read the file"}]`
	_, without, _ := countTokensRequest(t, `{"model":"m365-copilot",`+messages+`}`)
	_, with, _ := countTokensRequest(t, `{"model":"m365-copilot",`+messages+`,"tools":[{"name":"read_file","description":"Read a file from disk and return its contents","input_schema":{"type":"object","properties":{"path":{"type":"string","description":"absolute path to read"}},"required":["path"]}}]}`)
	if with <= without {
		t.Fatalf("count with tools = %d, without = %d; declaring a tool must raise the count", with, without)
	}
}

// A system prompt is part of the input too. anthropicRequest.openAI() folds it
// into the message list, so this also pins that the conversion is what gets
// counted rather than only body.Messages.
func TestCountTokensIncludesSystemPrompt(t *testing.T) {
	_, without, _ := countTokensRequest(t, `{"model":"m365-copilot","messages":[{"role":"user","content":"hi"}]}`)
	_, with, _ := countTokensRequest(t, `{"model":"m365-copilot","system":"You are a careful assistant that explains its reasoning at length.","messages":[{"role":"user","content":"hi"}]}`)
	if with <= without {
		t.Fatalf("count with system = %d, without = %d; a system prompt must raise the count", with, without)
	}
}

// Content blocks are the shape Claude Code actually sends once a turn carries
// tool results, so the handler must not reject them as bad json.
func TestCountTokensAcceptsContentBlocks(t *testing.T) {
	status, count, decoded := countTokensRequest(t, `{"model":"m365-copilot","messages":[{"role":"user","content":[{"type":"text","text":"what is in this file"}]},{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"a.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"build_serial: 9PW8R2OZIYDB"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", status, decoded)
	}
	if count < 1 {
		t.Fatalf("input_tokens = %d, want at least 1", count)
	}
}

// A longer history must count higher than a shorter one, or the client cannot
// use the endpoint to decide what to trim.
func TestCountTokensGrowsWithHistory(t *testing.T) {
	_, short, _ := countTokensRequest(t, `{"model":"m365-copilot","messages":[{"role":"user","content":"hi"}]}`)
	_, long, _ := countTokensRequest(t, `{"model":"m365-copilot","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Hello, how can I help you today with your codebase?"},{"role":"user","content":"Explain the router contract in detail, including the CALL_TOOL and NO_TOOL_NEEDED shapes."}]}`)
	if long <= short {
		t.Fatalf("long history = %d, short = %d; more history must count higher", long, short)
	}
}

func TestCountTokensRejectsNonPostAndBadJSON(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.anthropicCountTokens(w, httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}

	if status, _, _ := countTokensRequest(t, `{"model":`); status != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", status)
	}
}

// The route has to be reachable through the real mux, not just callable
// directly -- the defect was registration, not the handler.
func TestCountTokensRouteIsRegistered(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m365-copilot","messages":[{"role":"user","content":"hello"}]}`))
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if w.Code == http.StatusNotFound {
		t.Fatal("/v1/messages/count_tokens still 404s through the mux")
	}
}
