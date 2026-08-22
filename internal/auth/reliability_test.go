package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newExpiredReliabilityStore(t *testing.T) (*Store, AccountToken) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken:  "fake-access.invalid",
		RefreshToken: "fake-refresh.invalid",
		Email:        "reliability@example.invalid",
		HomeOID:      "reliability-oid.invalid",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, acc
}

func TestEnsureValidContextCoalescesConcurrentRefresh(t *testing.T) {
	store, acc := newExpiredReliabilityStore(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.refresh = func(ctx context.Context, refreshToken string) (TokenSet, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
			return TokenSet{
				AccessToken:  "refreshed-access.invalid",
				RefreshToken: "rotated-refresh.invalid",
				ExpiresAt:    time.Now().Add(time.Hour),
			}, nil
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		}
	}

	const workers = 8
	var ready sync.WaitGroup
	ready.Add(workers)
	begin := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			ready.Done()
			<-begin
			_, err := store.EnsureValidContext(context.Background(), acc.ID)
			results <- err
		}()
	}
	ready.Wait()
	close(begin)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent refresh failed: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh called %d times, want 1", got)
	}
}

func TestEnsureValidContextWaiterCanCancel(t *testing.T) {
	store, acc := newExpiredReliabilityStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	store.refresh = func(context.Context, string) (TokenSet, error) {
		close(started)
		<-release
		return TokenSet{AccessToken: "waiter-access.invalid", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := store.EnsureValidContext(context.Background(), acc.ID)
		leaderDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.EnsureValidContext(ctx, acc.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want context deadline exceeded", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader refresh failed: %v", err)
	}
}

func TestEnsureValidContextPreservesAccountOnTransientError(t *testing.T) {
	store, acc := newExpiredReliabilityStore(t)
	transient := errors.New("synthetic network failure")
	store.refresh = func(context.Context, string) (TokenSet, error) {
		return TokenSet{}, transient
	}
	_, err := store.EnsureValidContext(context.Background(), acc.ID)
	if !errors.Is(err, transient) {
		t.Fatalf("refresh error = %v, want transient error", err)
	}
	current, ok := store.Get(acc.ID)
	if !ok || current.Status == "expired" {
		t.Fatalf("transient failure marked account expired: %#v", current)
	}
}

func TestEnsureValidContextDoesNotOverwriteConcurrentUpdate(t *testing.T) {
	store, acc := newExpiredReliabilityStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	store.refresh = func(context.Context, string) (TokenSet, error) {
		close(started)
		<-release
		return TokenSet{
			AccessToken:  "stale-access.invalid",
			RefreshToken: "stale-refresh.invalid",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.EnsureValidContext(context.Background(), acc.ID)
		result <- err
	}()
	<-started
	if err := store.UpdateRefreshToken(acc.ID, "new-refresh.invalid"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("stale refresh unexpectedly succeeded")
	}
	current, ok := store.Get(acc.ID)
	if !ok || current.RefreshToken != "new-refresh.invalid" {
		t.Fatalf("concurrent refresh token update was lost: %#v", current)
	}
}

func TestEnsureValidContextDeleteDoesNotResurrect(t *testing.T) {
	store, acc := newExpiredReliabilityStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	store.refresh = func(context.Context, string) (TokenSet, error) {
		close(started)
		<-release
		return TokenSet{AccessToken: "deleted-access.invalid", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.EnsureValidContext(context.Background(), acc.ID)
		result <- err
	}()
	<-started
	if err := store.Delete(acc.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted account refresh error = %v, want not-exist", err)
	}
	if _, ok := store.Get(acc.ID); ok {
		t.Fatal("deleted account was resurrected")
	}
}

func TestPostAuthFormUsesContextAndBoundsResponse(t *testing.T) {
	started := make(chan struct{})
	releaseServer := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-releaseServer
	}))
	defer func() {
		close(releaseServer)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := postAuthForm(ctx, srv.URL, url.Values{"grant_type": {"refresh_token"}})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authentication request did not reach test server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not stop authentication request")
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(maxAuthResponseBytes)+1))
	}))
	defer large.Close()
	_, _, err := postAuthForm(context.Background(), large.URL, url.Values{"grant_type": {"fake"}})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("large response error = %v, want bounded-response error", err)
	}
}
