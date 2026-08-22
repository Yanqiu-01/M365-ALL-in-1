package outbound

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type reliabilityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f reliabilityRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type reliabilityBody struct{ closed atomic.Bool }

func (b *reliabilityBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *reliabilityBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestPoolRoundTripperRetriesNilBodyGETOnlyOnce(t *testing.T) {
	var first, second, third atomic.Int32
	p := &Pool{}
	e1 := &poolEntry{raw: "http://one.invalid"}
	e2 := &poolEntry{raw: "http://two.invalid"}
	e3 := &poolEntry{raw: "http://three.invalid"}
	e1.clients = &Clients{HTTP: &http.Client{Transport: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		first.Add(1)
		return nil, errors.New("synthetic first proxy failure")
	})}}
	e2.clients = &Clients{HTTP: &http.Client{Transport: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		second.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}}
	e3.clients = &Clients{HTTP: &http.Client{Transport: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		third.Add(1)
		return nil, errors.New("unexpected third proxy attempt")
	})}}
	p.entries = []*poolEntry{e1, e2, e3}
	resp, err := (&poolRoundTripper{pool: p, entry: e1, base: e1.clients.HTTP.Transport}).RoundTrip(httptestNewRequest(t, http.MethodGet, "http://service.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("retry returned nil response")
	}
	_ = resp.Body.Close()
	if first.Load() != 1 || second.Load() != 1 || third.Load() != 0 {
		t.Fatalf("attempts first=%d second=%d third=%d", first.Load(), second.Load(), third.Load())
	}
}

func TestPoolRoundTripperDoesNotRetryPOSTOrCanceledRequest(t *testing.T) {
	var retries atomic.Int32
	p := &Pool{}
	e1 := &poolEntry{raw: "http://one.invalid"}
	e2 := &poolEntry{raw: "http://two.invalid"}
	e1.clients = &Clients{HTTP: &http.Client{Transport: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic failure")
	})}}
	e2.clients = &Clients{HTTP: &http.Client{Transport: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		retries.Add(1)
		return nil, errors.New("unexpected retry")
	})}}
	p.entries = []*poolEntry{e1, e2}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		ctx := context.Background()
		if method == http.MethodGet {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		req := httptestNewRequest(t, method, "http://service.invalid")
		req = req.WithContext(ctx)
		_, _ = (&poolRoundTripper{pool: p, entry: e1, base: e1.clients.HTTP.Transport}).RoundTrip(req)
	}
	if retries.Load() != 0 {
		t.Fatalf("unexpected retries: %d", retries.Load())
	}
}

func TestPoolRoundTripperClosesErrorResponseBody(t *testing.T) {
	body := &reliabilityBody{}
	e := &poolEntry{raw: "http://one.invalid"}
	p := &Pool{entries: []*poolEntry{e}}
	_, _ = (&poolRoundTripper{pool: p, entry: e, base: reliabilityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: body}, errors.New("synthetic response failure")
	})}).RoundTrip(httptestNewRequest(t, http.MethodGet, "http://service.invalid"))
	if !body.closed.Load() {
		t.Fatal("error response body was not closed")
	}
}

func httptestNewRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
