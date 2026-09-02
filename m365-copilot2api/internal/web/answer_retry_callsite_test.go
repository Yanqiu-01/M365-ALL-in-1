package web

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

// The helper being wired in is only half the fix; the other half is that the
// NON-STREAMING answer turn actually goes through it. That call site had no seam
// at all, which is why nothing could catch the defect: a dropped ChatHub socket
// there became a hard 502 while the streaming path retried the identical error.
//
// The request deliberately declares no tools. That keeps len(toolMaps) == 0 so the
// router branch (server.go:1900 / :2250) is skipped and the turn lands straight on
// the answer call -- the exact shape that produced the live 502s.
func newAnswerRetryServer(t *testing.T) *Server {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID: "answer-retry-oid", TenantID: "answer-retry-tenant",
		Email: "answer@example.invalid", AccessToken: "answer-access",
		RefreshToken: "answer-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// defaultRuntimeSettings() keeps planningMode at "router", which is what the
	// live deployment runs, so this test exercises the same branch production does.
	return &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		upstreamCooldown:   newAccountCooldown(),
		accountConcurrency: newAccountConcurrency(),
		sessionResolver:    openSessionResolver(),
		settings:           &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), v: defaultRuntimeSettings()},
	}
}

const answerRetryBody = `{"model":"gpt-5.6-sol","stream":false,"messages":[{"role":"user","content":"in one sentence, what is a semaphore?"}]}`

func TestNonStreamingAnswerRetriesADroppedSocket(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "3") // four total attempts
	s := newAnswerRetryServer(t)

	previous := answerChat
	t.Cleanup(func() { answerChat = previous })

	attempts := 0
	answerChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request) (chathub.Result, error) {
		attempts++
		if attempts < 3 {
			// Verbatim shape of the live failure.
			return chathub.Result{}, fmt.Errorf("ws read before completion: %w",
				errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"))
		}
		return chathub.Result{Text: "A counter guarding a shared resource.", RequestID: "answer-ok"}, nil
	}

	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(answerRetryBody))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)

	if attempts < 2 {
		t.Fatalf("the dropped socket was not retried at all (attempts=%d) -- this is the 502 defect", attempts)
	}
	if w.Code != 200 {
		t.Errorf("status=%d want 200 after a recoverable drop; body=%s", w.Code, truncateForTest(w.Body.String()))
	}
	if !strings.Contains(w.Body.String(), "counter guarding") {
		t.Errorf("the successful retry's text did not reach the client; body=%s", truncateForTest(w.Body.String()))
	}
}

// The counterpart: an error that needs a different account must NOT spend the
// backoff on the same one. It has to reach the account-failover branch that sits
// immediately after this call.
func TestNonStreamingAnswerDoesNotRetryRateLimitOnTheSameAccount(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "3")
	s := newAnswerRetryServer(t)

	previous := answerChat
	t.Cleanup(func() { answerChat = previous })

	attempts := 0
	answerChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request) (chathub.Result, error) {
		attempts++
		// Renders as "ws dial: upstream 429", so it also matches the retryable
		// marker list. Only the ordering inside retryTransportOnly stops it here.
		return chathub.Result{}, &chathub.DialError{Status: 429}
	}

	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(answerRetryBody))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)

	if attempts != 1 {
		t.Errorf("a rate-limited account was retried %d times here; it must fall through to the account failover instead", attempts)
	}
}

func truncateForTest(s string) string {
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}
