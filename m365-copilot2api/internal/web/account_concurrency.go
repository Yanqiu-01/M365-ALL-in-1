package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"
)

const (
	defaultAccountConcurrency = 64
	maxAccountConcurrency     = 128
)

type accountConcurrency struct {
	mu       sync.Mutex
	limit    int
	inflight map[string]int
	changed  chan struct{}
}

func newAccountConcurrency() *accountConcurrency {
	limit := defaultAccountConcurrency
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_DEFAULT_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= maxAccountConcurrency {
			limit = parsed
		}
	}
	return &accountConcurrency{limit: limit, inflight: map[string]int{}, changed: make(chan struct{})}
}

func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID] < c.limit
}

func (c *accountConcurrency) Acquire(ctx context.Context, accountID string) (func(), error) {
	if c == nil || accountID == "" {
		return func() {}, nil
	}
	for {
		c.mu.Lock()
		if c.inflight[accountID] < c.limit {
			c.inflight[accountID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					if c.inflight[accountID] <= 1 {
						delete(c.inflight, accountID)
					} else {
						c.inflight[accountID]--
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{"limit": defaultAccountConcurrency, "inflight": map[string]int{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	return map[string]any{"limit": c.limit, "inflight": inflight}
}

func (s *Server) accountAvailable(accountID string) bool {
	if !s.accountPool.Available(accountID) || !s.accountConcurrency.Available(accountID) {
		return false
	}
	if s.upstreamCooldown == nil || s.tokens == nil {
		return true
	}
	account, ok := s.tokens.Get(accountID)
	return !ok || !s.upstreamCooldown.blocked(account.Email)
}

// recordUpstreamCooldown applies the APK's email-keyed backoff only to an
// early retryable transport close. Ordinary application errors continue to be
// tracked by accountHealth at their existing call sites.
func (s *Server) recordUpstreamCooldown(accountID string, err error) {
	if s == nil || s.upstreamCooldown == nil || s.tokens == nil {
		return
	}
	account, ok := s.tokens.Get(accountID)
	if !ok || account.Email == "" {
		return
	}
	if err == nil {
		s.upstreamCooldown.clear(account.Email)
		return
	}
	if isEarlyUpstreamClose(err) {
		s.upstreamCooldown.penalise(account.Email)
	}
}

func (s *Server) chatWithAccount(ctx context.Context, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	ctx = outbound.WithAccountAffinity(ctx, accountID)
	globalRelease, err := acquireChatSlotOrError(ctx)
	if err != nil {
		return chathub.Result{}, err
	}
	defer globalRelease()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	result, err := s.chat.Chat(ctx, account, request)
	s.recordUpstreamCooldown(accountID, err)
	return result, err
}

// routerChatWithFailover runs one router / tool-routing turn with transparent
// account and outbound-exit failover.
//
// Retrying here is safe against prompt duplication: chathub dials and
// initializes the WebSocket before it writes the chat payload, so an attempt
// that fails during connect/handshake never delivered a single byte of the
// conversation payload upstream. Once upstream output has started, the failure
// is no longer a connect-stage failure and the existing stream_recovery path
// owns it; this helper is used only for the payload-free router turns.
//
// After a failed attempt the account is swapped (excluding the one that just
// failed) so the retry lands on a different healthy account. The proxy pool
// unbinds its per-account sticky exit inside markFor/unstick when the dial
// fails, so the replacement attempt also re-binds to a different exit instead
// of deadlocking on the same dead one. When no replacement account exists the
// current one is reused: the exit can still differ, which is the whole point of
// retrying a transport failure.
// routerFailoverChat is a narrow seam for failover regression tests. Production
// delegates directly to the account-aware ChatHub path.
var routerFailoverChat = func(ctx context.Context, s *Server, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	return s.chatWithAccount(ctx, accountID, account, request)
}

func (s *Server) routerChatWithFailover(ctx context.Context, stage string, acc auth.AccountToken, request chathub.Request) (chathub.Result, auth.AccountToken, error) {
	current := acc
	var result chathub.Result
	err := retryUpstream(ctx, stage, func(attempt int) error {
		if attempt > 1 {
			if next, nextErr := s.nextHealthyAccount(current.ID); nextErr == nil {
				current = next
			}
		}
		account := chathub.Account{AccessToken: current.AccessToken, OID: current.OID, TID: current.TID}
		res, chatErr := routerFailoverChat(ctx, s, current.ID, account, request)
		result = res
		if chatErr != nil {
			s.accountPool.MarkFailure(current.ID, chatErr, rateLimitCooldown)
			return chatErr
		}
		s.accountPool.MarkSuccess(current.ID)
		return nil
	})
	return result, current, err
}
func (s *Server) chatWithAccountEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	ctx = outbound.WithAccountAffinity(ctx, accountID)
	globalRelease, err := acquireChatSlotOrError(ctx)
	if err != nil {
		return chathub.Result{}, err
	}
	defer globalRelease()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	result, err := s.chat.ChatWithEvents(ctx, account, request, onEvent)
	s.recordUpstreamCooldown(accountID, err)
	return result, err
}

func (s *Server) chatWithAccountReasoning(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onDelta, onReasoning func(string) error) (chathub.Result, error) {
	ctx = outbound.WithAccountAffinity(ctx, accountID)
	globalRelease, err := acquireChatSlotOrError(ctx)
	if err != nil {
		return chathub.Result{}, err
	}
	defer globalRelease()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	result, err := s.chat.ChatWithReasoning(ctx, account, request, onDelta, onReasoning)
	s.recordUpstreamCooldown(accountID, err)
	return result, err
}
