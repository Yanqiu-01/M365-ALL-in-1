package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHTTPTraceUsesListenerAddressNotHost(t *testing.T) {
	buf := captureLog(t)
	r := httptest.NewRequest(http.MethodGet, "http://untrusted.example:9999/v1/models?secret=not-for-logs", nil)
	r.Header.Set("Authorization", "Bearer not-for-logs")
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4140}))
	httpTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(httptest.NewRecorder(), r)
	got := buf.String()
	for _, want := range []string{`local="127.0.0.1:4140"`, fmt.Sprintf("pid=%d", os.Getpid()), "status=204", "canceled=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, forbidden := range []string{"untrusted.example", "9999", "not-for-logs", "Bearer"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("untrusted or sensitive data in trace: %q", got)
		}
	}
}

func TestHTTPTraceDoesNotGuessMissingListener(t *testing.T) {
	buf := captureLog(t)
	r := httptest.NewRequest(http.MethodGet, "http://untrusted.example:4141/v1/models", nil)
	httpTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(httptest.NewRecorder(), r)
	if got := buf.String(); !strings.Contains(got, `local="unknown"`) || strings.Contains(got, "4141") {
		t.Fatalf("trace guessed listener from Host: %q", got)
	}
}

func TestHTTPTraceCancellationPreservesStartedResponse(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			buf := captureLog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
			httpTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if started {
					w.WriteHeader(http.StatusOK)
				}
				cancel()
			})).ServeHTTP(httptest.NewRecorder(), r)
			want := statusClientClosedRequest
			if started {
				want = http.StatusOK
			}
			if got := buf.String(); !strings.Contains(got, fmt.Sprintf("status=%d", want)) || !strings.Contains(got, "canceled=true") {
				t.Fatalf("trace=%q", got)
			}
		})
	}
}
