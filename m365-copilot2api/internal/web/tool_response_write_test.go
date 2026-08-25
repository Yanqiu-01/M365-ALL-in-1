package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

var errToolResponseWrite = errors.New("tool response write failed")

type toolResponseFailWriter struct {
	header http.Header
	failAt int
	writes int
	body   strings.Builder
}

func (w *toolResponseFailWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *toolResponseFailWriter) WriteHeader(int) {}

func (w *toolResponseFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errToolResponseWrite
	}
	return w.body.Write(p)
}

func TestWriteToolResponseStreamPropagatesWriteErrors(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"test"}`)}}
	for _, tc := range []struct {
		name           string
		failAt         int
		wantBody       string
		forbidBody     string
		wantWriteCount int
	}{
		{
			name:           "first ordinary chunk",
			failAt:         1,
			forbidBody:     "[DONE]",
			wantWriteCount: 1,
		},
		{
			name:           "tool call chunk",
			failAt:         2,
			forbidBody:     "[DONE]",
			wantWriteCount: 2,
		},
		{
			name:           "usage chunk",
			failAt:         3,
			forbidBody:     "[DONE]",
			wantWriteCount: 3,
		},
		{
			name:           "done chunk",
			failAt:         4,
			wantBody:       `"finish_reason":"tool_calls"`,
			forbidBody:     "[DONE]",
			wantWriteCount: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &toolResponseFailWriter{failAt: tc.failAt}
			err := writeToolResponse(w, "chatcmpl_test", "test", true, calls, chathub.Result{})
			if !errors.Is(err, errToolResponseWrite) {
				t.Fatalf("writeToolResponse() error = %v, want write error", err)
			}
			if w.writes != tc.wantWriteCount {
				t.Fatalf("write count = %d, want %d", w.writes, tc.wantWriteCount)
			}
			if got := w.body.String(); tc.wantBody != "" && !strings.Contains(got, tc.wantBody) {
				t.Fatalf("response missing %q: %q", tc.wantBody, got)
			}
			if got := w.body.String(); tc.forbidBody != "" && strings.Contains(got, tc.forbidBody) {
				t.Fatalf("response unexpectedly contains %q: %q", tc.forbidBody, got)
			}
		})
	}
}

func TestWriteToolResponseSuccessKeepsStreamAndNonStreamFormats(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"test"}`)}}

	t.Run("stream", func(t *testing.T) {
		w := &toolResponseFailWriter{}
		if err := writeToolResponse(w, "chatcmpl_test", "test", true, calls, chathub.Result{}); err != nil {
			t.Fatalf("writeToolResponse() error = %v", err)
		}
		if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want text/event-stream", got)
		}
		body := w.body.String()
		for _, want := range []string{
			`data: {"choices":[{"delta":{"content":null,"role":"assistant"},"finish_reason":null,"index":0}]`,
			`"tool_calls":[{"function":{"arguments":"{\"q\":\"test\"}","name":"lookup"},"id":"call_1","index":0,"type":"function"}]`,
			`"finish_reason":"tool_calls"`,
			"data: [DONE]\n\n",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("stream response missing %q: %q", want, body)
			}
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		w := &toolResponseFailWriter{}
		if err := writeToolResponse(w, "chatcmpl_test", "test", false, calls, chathub.Result{}); err != nil {
			t.Fatalf("writeToolResponse() error = %v", err)
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(w.body.String()), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if got := response["object"]; got != "chat.completion" {
			t.Fatalf("object = %#v, want chat.completion", got)
		}
		choice := response["choices"].([]any)[0].(map[string]any)
		if got := choice["finish_reason"]; got != "tool_calls" {
			t.Fatalf("finish_reason = %#v, want tool_calls", got)
		}
	})
}
