package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestAnthropicReasoningEffortControls(t *testing.T) {
	for _, payload := range []string{
		`{"model":"gpt-5.6-reasoning","output_config":{"effort":"max"}}`,
		`{"model":"gpt-5.6-reasoning","reasoning_effort":"max"}`,
		`{"model":"gpt-5.6-reasoning","reasoning_effort":"max","output_config":{"effort":"max"}}`,
	} {
		var request anthropicRequest
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			t.Fatal(err)
		}
		got, err := request.openAI()
		if err != nil || got.ReasoningEffort != "max" {
			t.Fatalf("effort was lost: %+v err=%v", got, err)
		}
	}
	for _, payload := range []string{
		`{"reasoning_effort":"high","output_config":{"effort":"max"}}`,
		`{"output_config":{"effort":"unknown"}}`,
		`{"reasoning_effort":"unknown"}`,
	} {
		var request anthropicRequest
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			t.Fatal(err)
		}
		if _, err := request.openAI(); err == nil {
			t.Fatalf("invalid/conflicting effort accepted: %s", payload)
		}
	}
}

func TestAnthropicParallelControl(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		got, err := (anthropicRequest{ToolChoice: map[string]any{"type": "auto", "disable_parallel_tool_use": disabled}}).openAI()
		if err != nil || got.ParallelToolCalls == nil || *got.ParallelToolCalls == disabled {
			t.Fatalf("parallel flag lost: %+v %v", got, err)
		}
		calls := []detectedToolCall{{Name: "Read"}, {Name: "Read"}, {Name: "Read"}}
		kept, _ := enforceParallelToolCalls(calls, got.ParallelToolCalls)
		want := 3
		if disabled {
			want = 1
		}
		if len(kept) != want {
			t.Fatalf("calls=%d want %d", len(kept), want)
		}
	}
	if _, err := (anthropicRequest{ToolChoice: map[string]any{"type": "auto", "disable_parallel_tool_use": "true"}}).openAI(); err == nil {
		t.Fatal("non-boolean parallel flag accepted")
	}
}

func TestBufferedAdaptersKeepRouterCancellationStatus(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		for _, deadline := range []bool{false, true} {
			t.Run(endpoint+map[bool]string{false: "/canceled", true: "/deadline"}[deadline], func(t *testing.T) {
				s := newAnswerRetryServer(t)
				s.settings.v.ToolPlanningMode = "router"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if deadline {
					var stop context.CancelFunc
					ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					defer stop()
				}
				old := routerFailoverChat
				t.Cleanup(func() { routerFailoverChat = old })
				routerFailoverChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
					cancel()
					return chathub.Result{}, ctx.Err()
				}
				path, payload := editGuardProtocolRequest(endpoint, false, true)
				r := httptest.NewRequest("POST", path, strings.NewReader(payload)).WithContext(ctx)
				r.Header.Set(internalCallHeader, "1")
				w := httptest.NewRecorder()
				if endpoint == "messages" {
					s.anthropicMessages(w, r)
				} else {
					s.responses(w, r)
				}
				want := statusClientClosedRequest
				if deadline {
					want = 504
				}
				if w.Code != want {
					t.Fatalf("status=%d want %d body=%s", w.Code, want, w.Body.String())
				}
				if !deadline && w.Body.Len() != 0 {
					t.Fatalf("canceled request wrote an error body: %s", w.Body.String())
				}
			})
		}
	}
}
