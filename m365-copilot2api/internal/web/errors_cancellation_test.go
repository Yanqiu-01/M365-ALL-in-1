package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamCancellationAndDeadlineClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		typ    string
	}{
		{"canceled", context.Canceled, statusClientClosedRequest, ""},
		{"wrapped canceled", fmt.Errorf("transport: %w", context.Canceled), statusClientClosedRequest, ""},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, "timeout_error"},
		{"wrapped deadline", fmt.Errorf("ws dial: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "timeout_error"},
		{"early close", io.ErrUnexpectedEOF, http.StatusBadGateway, "upstream_error"},
		{"auth", &UpstreamHTTPError{Status: 401}, http.StatusUnauthorized, "upstream_error"},
		{"quota", &UpstreamHTTPError{Status: 429, RetryAfter: 23}, http.StatusTooManyRequests, "rate_limit_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamStatus(tc.err); got != tc.status {
				t.Fatalf("status=%d want %d", got, tc.status)
			}
			w := httptest.NewRecorder()
			writeUpstreamError(w, tc.err)
			if w.Code != tc.status {
				t.Fatalf("response status=%d want %d", w.Code, tc.status)
			}
			if tc.status == statusClientClosedRequest {
				if w.Body.Len() != 0 {
					t.Fatalf("canceled request wrote a body: %q", w.Body.String())
				}
				return
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != tc.typ {
				t.Fatalf("type=%q want %q", body.Error.Type, tc.typ)
			}
			if tc.status == http.StatusTooManyRequests && w.Header().Get("Retry-After") != "23" {
				t.Fatalf("Retry-After=%q", w.Header().Get("Retry-After"))
			}
		})
	}
}

func TestCancellationDescriptionDoesNotBecomeUpstreamFailure(t *testing.T) {
	if got := classifyUpstream(fmt.Errorf("ws dial: %w", context.Canceled)); got != "request canceled" {
		t.Fatalf("classification=%q", got)
	}
	if got := classifyUpstream(fmt.Errorf("ws dial: %w", context.DeadlineExceeded)); got != "upstream request timed out" {
		t.Fatalf("classification=%q", got)
	}
}
