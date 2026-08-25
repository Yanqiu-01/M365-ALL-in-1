package web

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func newRouterFailoverServer(t *testing.T) (*Server, auth.AccountToken, auth.AccountToken) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Upsert(auth.TokenSet{
		HomeOID: "router-first", TenantID: "router-tenant", Email: "first@example.invalid",
		AccessToken: "first-access", RefreshToken: "first-refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Upsert(auth.TokenSet{
		HomeOID: "router-second", TenantID: "router-tenant", Email: "second@example.invalid",
		AccessToken: "second-access", RefreshToken: "second-refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		upstreamCooldown:   newAccountCooldown(),
		accountConcurrency: newAccountConcurrency(),
	}, first, second
}

// A wrapped ws dial timeout must fail over to a different healthy account, and
// the router prompt must be sent exactly once per attempt: chathub dials and
// initializes the socket before writing the chat payload, so a connect-stage
// retry cannot duplicate an already-delivered payload upstream.
func TestRouterChatWithFailoverSwapsAccountAfterDialTimeout(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "1") // two total attempts
	s, first, second := newRouterFailoverServer(t)

	previous := routerFailoverChat
	t.Cleanup(func() { routerFailoverChat = previous })

	var usedAccounts []string
	var sentPrompts []string
	routerFailoverChat = func(_ context.Context, _ *Server, accountID string, _ chathub.Account, request chathub.Request) (chathub.Result, error) {
		usedAccounts = append(usedAccounts, accountID)
		sentPrompts = append(sentPrompts, request.Text)
		if len(usedAccounts) == 1 {
			return chathub.Result{}, fmt.Errorf("ws dial: %w", context.DeadlineExceeded)
		}
		return chathub.Result{Text: `{"calls":[]}`, RequestID: "router-ok"}, nil
	}

	result, used, err := s.routerChatWithFailover(context.Background(), "router", first, chathub.Request{Text: "route this turn"})
	if err != nil {
		t.Fatalf("routerChatWithFailover() error = %v, want failover success", err)
	}
	if len(usedAccounts) != 2 {
		t.Fatalf("attempts=%d want 2 (%v)", len(usedAccounts), usedAccounts)
	}
	if usedAccounts[0] != first.ID {
		t.Fatalf("first attempt account=%q want %q", usedAccounts[0], first.ID)
	}
	if usedAccounts[1] == first.ID {
		t.Fatalf("retry reused the account that just failed: %q", usedAccounts[1])
	}
	if usedAccounts[1] != second.ID {
		t.Fatalf("retry account=%q want %q", usedAccounts[1], second.ID)
	}
	if used.ID != second.ID {
		t.Fatalf("reported account=%q want %q", used.ID, second.ID)
	}
	if result.RequestID != "router-ok" {
		t.Fatalf("result request id=%q want router-ok", result.RequestID)
	}
	// One payload per attempt, never a replay stacked onto a live conversation.
	for i, prompt := range sentPrompts {
		if prompt != "route this turn" {
			t.Fatalf("attempt %d prompt was rewritten to %q; a connect-stage retry must resend the identical payload once", i+1, prompt)
		}
	}
}

// A cancelled parent context must not be retried even though the error text is
// a normally retryable transport failure.
func TestRouterChatWithFailoverStopsOnDoneParentContext(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "5")
	s, first, _ := newRouterFailoverServer(t)

	previous := routerFailoverChat
	t.Cleanup(func() { routerFailoverChat = previous })

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	routerFailoverChat = func(context.Context, *Server, string, chathub.Account, chathub.Request) (chathub.Result, error) {
		attempts++
		cancel()
		return chathub.Result{}, fmt.Errorf("ws dial: %w", context.DeadlineExceeded)
	}

	if _, _, err := s.routerChatWithFailover(ctx, "router", first, chathub.Request{Text: "route this turn"}); err == nil {
		t.Fatal("routerChatWithFailover() error = nil, want failure")
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want 1: a done parent context must not be retried", attempts)
	}
}