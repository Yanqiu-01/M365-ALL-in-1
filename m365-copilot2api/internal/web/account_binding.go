package web

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

// Request-supplied account/cloud IDs are constraints; recovered session IDs are
// hints. Keep that distinction before restoring body.AccountID from a cache.
type accountBinding struct {
	fixed           bool
	accountID       string
	conversationID  string
	sessionID       string
	fullPrompt      string
	replayPrompt    string
	fullAttachments []chathub.Attachment
	migrated        bool
}

type accountBindingKey struct{}

func accountTraceID(accountID string) string {
	if accountID == "" {
		return "none"
	}
	digest := sha256.Sum256([]byte(accountID))
	return fmt.Sprintf("%x", digest[:6])
}

func requestAccountBinding(ctx context.Context) *accountBinding {
	binding, _ := ctx.Value(accountBindingKey{}).(*accountBinding)
	return binding
}

func accountFailoverAllowed(ctx context.Context) bool {
	binding := requestAccountBinding(ctx)
	return binding == nil || !binding.fixed
}

func (b *accountBinding) selectAccount(body *oaiReq, accountID string) bool {
	changed := body.AccountID != "" && body.AccountID != accountID
	if changed {
		body.ConversationID = ""
		body.SessionID = ""
		body.Attachments = append([]chathub.Attachment(nil), b.fullAttachments...)
		b.migrated = true
	}
	body.AccountID = accountID
	return changed
}

// A new account cannot read the previous account's cloud history. Preserve the
// request's options, but replay the full evidence rather than only the increment.
func accountReplayRequest(ctx context.Context, request chathub.Request) chathub.Request {
	request.ConversationID = ""
	request.SessionID = ""
	request.Started = true
	if binding := requestAccountBinding(ctx); binding != nil {
		request.Text = firstNonEmpty(binding.replayPrompt, binding.fullPrompt)
		request.Attachments = append([]chathub.Attachment(nil), binding.fullAttachments...)
	}
	return request
}

func (s *Server) resolveBoundAccount(accountID string, fixed bool) (auth.AccountToken, error) {
	if fixed || accountID == "" {
		return s.resolveAccount(accountID)
	}
	var original error
	if s.accountAvailable(accountID) {
		if account, err := s.tokens.EnsureValid(accountID); err == nil {
			return account, nil
		} else {
			original = err
		}
	}
	account, err := s.nextHealthyAccount(accountID)
	if err == nil {
		log.Printf("[account-route] source=automatic_session action=failover reason=bound_account_unavailable from_hash=%s to_hash=%s", accountTraceID(accountID), accountTraceID(account.ID))
		return account, nil
	}
	if original != nil {
		return auth.AccountToken{}, original
	}
	return auth.AccountToken{}, err
}

func (s *Server) nextHealthyAccountExcept(excluded map[string]bool) (auth.AccountToken, error) {
	if s == nil || s.tokens == nil {
		return auth.AccountToken{}, errNoAccounts
	}
	accounts := s.tokens.List()
	if len(accounts) == 0 {
		return auth.AccountToken{}, errNoAccounts
	}
	// Snapshot candidates once: another concurrent request advancing Store.Next
	// must not make this request miss a healthy candidate or retry a failed one.
	var firstErr error
	for _, account := range accounts {
		if excluded[account.ID] || !s.accountAvailable(account.ID) {
			continue
		}
		validated, err := s.tokens.EnsureValid(account.ID)
		if err == nil {
			return validated, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return auth.AccountToken{}, firstErr
	}
	retry := 0
	consider := func(until time.Time) {
		seconds := int(time.Until(until).Seconds()) + 1
		if seconds > 0 && (retry == 0 || seconds < retry) {
			retry = seconds
		}
	}
	if s.accountPool != nil {
		for _, state := range s.accountPool.Snapshot() {
			if until, ok := state["cooldownUntil"].(time.Time); ok {
				consider(until)
			}
		}
	}
	if s.upstreamCooldown != nil {
		for _, until := range s.upstreamCooldown.snapshot() {
			consider(until)
		}
	}
	if retry < 5 {
		retry = 5
	}
	return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: retry, Body: "no healthy account available for failover"}
}

// Only retire the exact old binding after the replacement succeeded. A sibling
// request may already have advanced it, in which case its state belongs to it.
func (s *Server) finishAccountMigration(rctx context.Context) {
	binding := requestAccountBinding(rctx)
	if binding == nil || !binding.migrated || binding.sessionID == "" || s.sessionResolver == nil {
		return
	}
	s.sessionResolver.retireAccountBinding(binding.sessionID, binding.accountID, binding.conversationID)
}
