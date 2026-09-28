package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestRouterFatalPreservesFailureClassification(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, &UpstreamHTTPError{Status: 429, RetryAfter: 19}, &UpstreamHTTPError{Status: 401}, io.ErrUnexpectedEOF} {
		w := httptest.NewRecorder()
		writeRouterFatal(w, "router", err)
		if w.Code != upstreamStatus(err) {
			t.Fatalf("error=%v status=%d", err, w.Code)
		}
		if IsRateLimited(err) && w.Header().Get("Retry-After") != "19" {
			t.Fatal("router lost retry hint")
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if classifyRouterFailure(ctx, ctx.Err(), "auto") != routerFailureFatal {
		t.Fatal("spent request budget attempted an answer fallback")
	}
}

func TestAdaptersPreserveCancellationTimeoutAndRetryAfter(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/messages"} {
		for _, failure := range []error{context.Canceled, context.DeadlineExceeded, &UpstreamHTTPError{Status: 429, RetryAfter: 17}} {
			t.Run(endpoint+"/"+failure.Error(), func(t *testing.T) {
				s := newAnswerRetryServer(t)
				previous := answerChat
				t.Cleanup(func() { answerChat = previous })
				answerChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
					return chathub.Result{}, failure
				}
				body := `{"model":"gpt-5.6-sol","input":"Describe a semaphore.","stream":false}`
				if endpoint == "/v1/messages" {
					body = `{"model":"gpt-5.6-sol","max_tokens":128,"messages":[{"role":"user","content":"Describe a semaphore."}],"stream":false}`
				}
				r := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
				w := httptest.NewRecorder()
				if endpoint == "/v1/responses" {
					s.responses(w, r)
				} else {
					s.anthropicMessages(w, r)
				}
				if w.Code != upstreamStatus(failure) {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if failure == context.Canceled && w.Body.Len() != 0 {
					t.Fatal("canceled adapter wrote a body")
				}
				if IsRateLimited(failure) && w.Header().Get("Retry-After") != "17" {
					t.Fatal("adapter discarded Retry-After")
				}
			})
		}
	}
}

func TestResponsesStreamDoesNotCompleteAfterUpstreamFailure(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			t.Setenv("M365_UPSTREAM_RETRIES", "0")
			s := newAnswerRetryServer(t)
			previous := streamRecoveryChatWithEvents
			t.Cleanup(func() { streamRecoveryChatWithEvents = previous })
			streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
				if err := onEvent(chathub.StreamEvent{Kind: "text", Text: "A partial answer"}); err != nil {
					return chathub.Result{}, err
				}
				return chathub.Result{}, failure
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"Describe a semaphore.","stream":true}`))
			w := httptest.NewRecorder()
			s.responses(w, r)
			got := w.Body.String()
			if !strings.Contains(got, "response.failed") || strings.Contains(got, "response.completed") || !strings.Contains(got, upstreamStreamErrorCode(failure)) {
				t.Fatalf("stream hid failure: %s", got)
			}
		})
	}
}
