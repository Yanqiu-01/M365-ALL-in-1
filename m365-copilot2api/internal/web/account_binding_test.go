package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func addBindingAccount(t *testing.T, s *Server, id string, expiry time.Time) auth.AccountToken {
	t.Helper()
	account, err := s.tokens.Upsert(auth.TokenSet{HomeOID: id, TenantID: "test-tenant", Email: id + "@example.invalid", AccessToken: "synthetic", RefreshToken: "synthetic", ExpiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestAutomaticBindingRespectsCooldownAndExplicitSelection(t *testing.T) {
	s := newAnswerRetryServer(t)
	original := s.tokens.List()[0]
	backup := addBindingAccount(t, s, "backup", time.Now().Add(time.Hour))
	s.accountPool.MarkFailure(original.ID, &UpstreamHTTPError{Status: 429}, time.Minute)
	got, err := s.resolveBoundAccount(original.ID, false)
	if err != nil || got.ID != backup.ID {
		t.Fatalf("automatic account=%s err=%v", got.ID, err)
	}
	got, err = s.resolveBoundAccount(original.ID, true)
	if err != nil || got.ID != original.ID {
		t.Fatalf("explicit selection changed: account=%s err=%v", got.ID, err)
	}
}

func TestAutomaticBindingRecoversRefreshFailure(t *testing.T) {
	failingTokenEndpoint(t)
	s := &Server{tokens: expiredAndValidAccounts(t), accountPool: newAccountHealth()}
	got, err := s.resolveBoundAccount("u-1", false)
	if err != nil || got.ID == "u-1" {
		t.Fatalf("automatic refresh failover account=%s err=%v", got.ID, err)
	}
	if _, err := s.resolveBoundAccount("u-1", true); err == nil {
		t.Fatal("explicit account silently failed over")
	}
}

func TestNextHealthyAccountSkipsBrokenRefreshCandidate(t *testing.T) {
	failingTokenEndpoint(t)
	s := newAnswerRetryServer(t)
	old := s.tokens.List()[0]
	addBindingAccount(t, s, "backup-a-expired", time.Now().Add(-time.Hour))
	good := addBindingAccount(t, s, "backup-b-valid", time.Now().Add(time.Hour))
	got, err := s.nextHealthyAccount(old.ID)
	if err != nil || got.ID != good.ID {
		t.Fatalf("later healthy candidate was skipped: account=%s err=%v", got.ID, err)
	}
}

func TestAutomaticBindingWithoutAlternativeReturnsBackoff(t *testing.T) {
	s := newAnswerRetryServer(t)
	account := s.tokens.List()[0]
	s.accountPool.MarkFailure(account.ID, &UpstreamHTTPError{Status: 429, RetryAfter: 25}, time.Minute)
	_, err := s.resolveBoundAccount(account.ID, false)
	if !IsRateLimited(err) || RetryAfterSeconds(err) < 5 {
		t.Fatalf("expected bounded backoff, got %v", err)
	}
}

func TestAccountReplayRestoresHistoryAndAttachments(t *testing.T) {
	binding := &accountBinding{fullPrompt: "full history", replayPrompt: "full history with tool protocol", fullAttachments: []chathub.Attachment{{Type: "image", URL: "https://example.invalid/history.png"}}}
	body := oaiReq{AccountID: "old", ConversationID: "old-conv", SessionID: "old-session"}
	if !binding.selectAccount(&body, "new") || body.ConversationID != "" || body.SessionID != "" || len(body.Attachments) != 1 {
		t.Fatalf("migration left stale cloud state: %+v", body)
	}
	ctx := context.WithValue(context.Background(), accountBindingKey{}, binding)
	got := accountReplayRequest(ctx, chathub.Request{Text: "increment only", ConversationID: "old-conv", SessionID: "old-session", Tone: "magic"})
	if got.Text != binding.replayPrompt || got.ConversationID != "" || got.SessionID != "" || len(got.Attachments) != 1 || got.Tone != "magic" {
		t.Fatalf("incomplete replay: %+v", got)
	}
}

func TestOpenAIContinuationMigratesAndRebinds(t *testing.T) {
	for _, failDuringAnswer := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "preflight", true: "answer"}[failDuringAnswer], map[bool]string{false: "json", true: "stream"}[stream]}, "/"), func(t *testing.T) {
				s := newAnswerRetryServer(t)
				t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
				t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
				t.Setenv("M365_CLEANUP_MODE", "keep_n")
				t.Setenv("M365_UPSTREAM_RETRIES", "1")
				s.sessions = openSessionStore()
				s.userSessions = openUserSessionStore(time.Hour)
				s.conversationManager = openConversationManager()
				old := s.tokens.List()[0]
				backup := addBindingAccount(t, s, "backup", time.Now().Add(time.Hour))
				history := []oaiMsg{{Role: "system", Content: runtimeWorkspaceInstruction()}, {Role: "user", Content: "Remember the codeword cobalt."}, {Role: "assistant", Content: "I will remember cobalt."}}
				request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				request.Header.Set(internalCallHeader, "1")
				s.sessionResolver.Bind("old-session", "old-conv", old.ID, &oaiReq{Messages: history}, "", request)
				s.sessions.upsert(conversation{ID: "local-key", AccountID: old.ID, ConversationID: "old-conv", SessionID: "old-session"})
				if !failDuringAnswer {
					s.accountPool.MarkFailure(old.ID, &UpstreamHTTPError{Status: 429}, time.Minute)
				}
				calls := 0
				invoke := func(accountID string, req chathub.Request) (chathub.Result, error) {
					calls++
					if accountID == old.ID {
						if !failDuringAnswer || calls != 1 {
							t.Fatal("selected unavailable old account again")
						}
						if req.ConversationID != "old-conv" || strings.Contains(req.Text, "codeword cobalt") {
							t.Fatalf("healthy binding did not send the increment: %+v", req)
						}
						return chathub.Result{}, &UpstreamHTTPError{Status: 429}
					}
					if accountID != backup.ID || req.ConversationID != "" || req.SessionID != "" || !strings.Contains(req.Text, "codeword cobalt") || !strings.Contains(req.Text, "What was the codeword") {
						t.Fatalf("bad migrated request: account=%s req=%+v", accountID, req)
					}
					return chathub.Result{Text: "The codeword was cobalt.", ConversationID: "new-conv", SessionID: "new-session"}, nil
				}
				previousAnswer, previousStream := answerChat, streamRecoveryChatWithEvents
				t.Cleanup(func() { answerChat = previousAnswer; streamRecoveryChatWithEvents = previousStream })
				answerChat = func(_ context.Context, _ *Server, id string, _ chathub.Account, req chathub.Request) (chathub.Result, error) {
					return invoke(id, req)
				}
				streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, id string, _ chathub.Account, req chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
					res, err := invoke(id, req)
					if err == nil {
						err = onEvent(chathub.StreamEvent{Kind: "text", Text: res.Text})
					}
					return res, err
				}
				body := oaiReq{Model: "gpt-5.6-sol", Stream: stream, SessionKey: "local-key", User: "logical-user", Messages: append(cloneMessages(history), oaiMsg{Role: "user", Content: "What was the codeword?"})}
				data, _ := json.Marshal(body)
				request.Body = io.NopCloser(strings.NewReader(string(data)))
				w := httptest.NewRecorder()
				s.openaiChat(w, request)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "cobalt") {
					t.Fatalf("status=%d body=%s", w.Code, truncateForTest(w.Body.String()))
				}
				if failDuringAnswer && calls != 2 || !failDuringAnswer && calls != 1 {
					t.Fatalf("calls=%d", calls)
				}
				if _, ok := s.sessionResolver.GetSession("old-session"); ok {
					t.Fatal("old binding survived successful migration")
				}
				if got, ok := s.sessions.get("local-key"); !ok || got.AccountID != backup.ID || got.ConversationID != "new-conv" {
					t.Fatalf("SessionKey not rebound: %+v", got)
				}
				if got, ok := s.userSessions.Get("logical-user"); !ok || got.AccountID != backup.ID || got.ConversationID != "new-conv" {
					t.Fatalf("user not rebound: %+v", got)
				}
				body.Messages = append(body.Messages, oaiMsg{Role: "assistant", Content: "The codeword was cobalt."}, oaiMsg{Role: "user", Content: "Thanks; continue."})
				resolved := s.sessionResolver.Resolve(request, &body)
				if resolved.IsNew || resolved.AccountID != backup.ID || resolved.ConversationID != "new-conv" {
					t.Fatalf("next turn reselected stale binding: %+v", resolved)
				}
			})
		}
	}
}

func TestMigrationDoesNotRetireConcurrentBinding(t *testing.T) {
	s := newAnswerRetryServer(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	s.sessionResolver.Bind("session", "newer-conv", "newer-account", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "newer history"}}}, "", r)
	s.sessionResolver.retireAccountBinding("session", "old-account", "old-conv")
	if _, ok := s.sessionResolver.GetSession("session"); !ok {
		t.Fatal("concurrent binding was deleted")
	}
}

func TestPinnedStreamDoesNotChangeAccount(t *testing.T) {
	s := newAnswerRetryServer(t)
	old := s.tokens.List()[0]
	addBindingAccount(t, s, "backup", time.Now().Add(time.Hour))
	t.Setenv("M365_UPSTREAM_RETRIES", "1")
	previous := streamRecoveryChatWithEvents
	t.Cleanup(func() { streamRecoveryChatWithEvents = previous })
	calls := 0
	streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, id string, _ chathub.Account, _ chathub.Request, _ func(chathub.StreamEvent) error) (chathub.Result, error) {
		calls++
		if id != old.ID {
			t.Fatal("explicit account changed")
		}
		return chathub.Result{}, io.ErrUnexpectedEOF
	}
	ctx := context.WithValue(context.Background(), accountBindingKey{}, &accountBinding{fixed: true})
	_, _, err := s.streamChatWithRecovery(ctx, old, chathub.Request{ConversationID: "fixed"}, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
