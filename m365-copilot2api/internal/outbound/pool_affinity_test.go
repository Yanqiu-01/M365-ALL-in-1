package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPoolHTTPAccountAffinityFailsOverAndRebinds(t *testing.T) {
	firstCalls, secondCalls := 0, 0
	firstErr := errors.New("sticky exit failed")
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients((&net.Dialer{}).DialContext, testRoundTripper(func(*http.Request) (*http.Response, error) {
				firstCalls++
				return nil, firstErr
			}))},
			{raw: "second", clients: testClients((&net.Dialer{}).DialContext, testRoundTripper(func(*http.Request) (*http.Response, error) {
				secondCalls++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}))},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}
	ctx := WithAccountAffinity(context.Background(), "account-1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := pool.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("failover calls = %d/%d, want 1/1", firstCalls, secondCalls)
	}
	response, err = pool.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if secondCalls != 2 {
		t.Fatalf("sticky rebinding calls = first:%d second:%d, want second=2", firstCalls, secondCalls)
	}
}

func TestPoolWebSocketAccountAffinitySkipsCooledExit(t *testing.T) {
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients((&net.Dialer{}).DialContext, http.DefaultTransport)},
			{raw: "second", clients: testClients((&net.Dialer{}).DialContext, http.DefaultTransport)},
		},
		wsLimit:   2,
		wsChanged: make(chan struct{}),
		sticky:    map[string]string{"account-1": "first"},
	}
	if got := pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{}); got == nil || got.raw != "first" {
		t.Fatalf("sticky entry = %#v, want first", got)
	}
	pool.entries[0].cooldown = time.Now().Add(time.Minute)
	if got := pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{}); got == nil || got.raw != "second" {
		t.Fatalf("cooled failover entry = %#v, want second", got)
	}
}
