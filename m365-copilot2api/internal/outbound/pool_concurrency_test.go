package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type testPipeDialer struct {
	mu    sync.Mutex
	calls int
	peers []net.Conn
}

func (d *testPipeDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	client, peer := net.Pipe()
	d.mu.Lock()
	d.calls++
	d.peers = append(d.peers, peer)
	d.mu.Unlock()
	return client, nil
}

func (d *testPipeDialer) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *testPipeDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, peer := range d.peers {
		_ = peer.Close()
	}
	d.peers = nil
}

func testClients(dial func(context.Context, string, string) (net.Conn, error), transport http.RoundTripper) *Clients {
	return &Clients{
		HTTP: &http.Client{Transport: transport, Timeout: time.Second},
		WebSocket: &websocket.Dialer{
			HandshakeTimeout: time.Second,
			NetDialContext:   dial,
		},
	}
}

func TestPoolWebSocketDialerBalancesAndReleasesSlots(t *testing.T) {
	first, second := &testPipeDialer{}, &testPipeDialer{}
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients(first.DialContext, http.DefaultTransport)},
			{raw: "second", clients: testClients(second.DialContext, http.DefaultTransport)},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}
	dialer := pool.WebSocketDialer()

	conn1, err := dialer.NetDialContext(context.Background(), "tcp", "upstream.test:443")
	if err != nil {
		t.Fatal(err)
	}
	conn2, err := dialer.NetDialContext(context.Background(), "tcp", "upstream.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if first.Calls() != 1 || second.Calls() != 1 {
		t.Fatalf("connections were not balanced: first=%d second=%d", first.Calls(), second.Calls())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := dialer.NetDialContext(ctx, "tcp", "upstream.test:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capacity wait error = %v, want deadline exceeded", err)
	}

	if err := conn1.Close(); err != nil {
		t.Fatal(err)
	}
	conn3, err := dialer.NetDialContext(context.Background(), "tcp", "upstream.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if first.Calls() != 2 {
		t.Fatalf("released exit was not reused: first=%d", first.Calls())
	}
	_ = conn2.Close()
	_ = conn3.Close()

	for _, status := range pool.List() {
		if active, _ := status["activeWebSockets"].(int); active != 0 {
			t.Fatalf("slot leaked: %#v", status)
		}
	}
}

func TestPoolRoundTripRetriesSafeNilBodyRequest(t *testing.T) {
	firstCalls, secondCalls := 0, 0
	failure := errors.New("exit closed")
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients((&net.Dialer{}).DialContext, testRoundTripper(func(*http.Request) (*http.Response, error) {
				firstCalls++
				return nil, failure
			}))},
			{raw: "second", clients: testClients((&net.Dialer{}).DialContext, testRoundTripper(func(*http.Request) (*http.Response, error) {
				secondCalls++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}))},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}
	response, err := pool.HTTPClient().Get("https://upstream.test/resource")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("safe retry = first:%d second:%d, want 1/1", firstCalls, secondCalls)
	}
}

func TestOutboundTransportLimitsAreConfigured(t *testing.T) {
	t.Setenv(EnvOutboundMaxIdleConns, "12")
	t.Setenv(EnvOutboundMaxIdleConnsPerHost, "18")
	t.Setenv(EnvOutboundMaxConnsPerHost, "24")
	t.Setenv(EnvOutboundHTTPTimeoutSeconds, "9")
	clients := directClients()
	transport := clients.HTTP.Transport.(*http.Transport)
	if transport.MaxIdleConns != 12 || transport.MaxIdleConnsPerHost != 12 || transport.MaxConnsPerHost != 24 {
		t.Fatalf("transport limits = %d/%d/%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost)
	}
	if got := int(clients.HTTP.Timeout.Seconds()); got != 9 {
		t.Fatalf("HTTP timeout = %d, want 9", got)
	}
}

func TestPoolWebSocketDialerRetriesDifferentExitAfterDialFailure(t *testing.T) {
	firstCalls := 0
	second := &testPipeDialer{}
	t.Cleanup(second.Close)
	failure := errors.New("proxy CONNECT rejected")
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients(func(context.Context, string, string) (net.Conn, error) {
				firstCalls++
				return nil, failure
			}, http.DefaultTransport)},
			{raw: "second", clients: testClients(second.DialContext, http.DefaultTransport)},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}

	conn, err := pool.WebSocketDialer().NetDialContext(context.Background(), "tcp", "upstream.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || second.Calls() != 1 {
		t.Fatalf("dial retry = first:%d second:%d, want 1/1", firstCalls, second.Calls())
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, status := range pool.List() {
		if active, _ := status["activeWebSockets"].(int); active != 0 {
			t.Fatalf("slot leaked after retry: %#v", status)
		}
	}
}

func TestPoolWebSocketDialerDoesNotRepeatFailedExit(t *testing.T) {
	firstCalls, secondCalls, thirdCalls := 0, 0, 0
	firstFailure := errors.New("first failed")
	secondFailure := errors.New("second failed")
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients(func(context.Context, string, string) (net.Conn, error) {
				firstCalls++
				return nil, firstFailure
			}, http.DefaultTransport)},
			{raw: "second", clients: testClients(func(context.Context, string, string) (net.Conn, error) {
				secondCalls++
				return nil, secondFailure
			}, http.DefaultTransport)},
			{raw: "third", clients: testClients(func(context.Context, string, string) (net.Conn, error) {
				thirdCalls++
				return nil, errors.New("third must not be attempted")
			}, http.DefaultTransport)},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}

	_, err := pool.WebSocketDialer().NetDialContext(context.Background(), "tcp", "upstream.test:443")
	if !errors.Is(err, secondFailure) && !errors.Is(err, firstFailure) {
		t.Fatalf("dial error = %v, want a proxy failure", err)
	}
	if firstCalls != 1 || secondCalls != 1 || thirdCalls != 0 {
		t.Fatalf("unexpected attempts: first:%d second:%d third:%d", firstCalls, secondCalls, thirdCalls)
	}
}

func TestPoolWebSocketDialerHonorsCanceledContextBeforeDial(t *testing.T) {
	calls := 0
	pool := &Pool{
		entries: []*poolEntry{{raw: "only", clients: testClients(func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("must not dial")
		}, http.DefaultTransport)}},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pool.WebSocketDialer().NetDialContext(ctx, "tcp", "upstream.test:443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error = %v, want context canceled", err)
	}
	if calls != 0 {
		t.Fatalf("dial was called %d times after cancellation", calls)
	}
}
