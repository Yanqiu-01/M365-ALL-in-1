package web

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func TestStreamRecoveryForwardsNewNonTextEventsWithoutReplayingOldOnes(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "1")

	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Upsert(auth.TokenSet{
		HomeOID:      "recovery-first",
		TenantID:     "recovery-tenant",
		Email:        "first@example.invalid",
		AccessToken:  "first-access-token",
		RefreshToken: "first-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Upsert(auth.TokenSet{
		HomeOID:      "recovery-second",
		TenantID:     "recovery-tenant",
		Email:        "second@example.invalid",
		AccessToken:  "second-access-token",
		RefreshToken: "second-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		accountConcurrency: newAccountConcurrency(),
	}

	previous := streamRecoveryChatWithEvents
	t.Cleanup(func() { streamRecoveryChatWithEvents = previous })

	oldProgress := chathub.StreamEvent{Kind: "progress", Text: "searching", MessageType: "Progress"}
	oldTool := chathub.StreamEvent{Kind: "tool", ToolName: "search", Arguments: []byte(`{"query":"cats"}`)}
	oldReasoning := chathub.StreamEvent{Kind: "reasoning", Text: "I will search first."}
	newProgress := chathub.StreamEvent{Kind: "progress", Text: "opening the matching result", MessageType: "Progress"}
	newTool := chathub.StreamEvent{Kind: "tool", ToolName: "open_result", Arguments: []byte(`{"id":"result-1"}`)}
	newReasoning := chathub.StreamEvent{Kind: "reasoning", Text: "The result answers the question."}

	calls := 0
	streamRecoveryChatWithEvents = func(_ context.Context, _ *Server, accountID string, _ chathub.Account, _ chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
		calls++
		if calls == 1 {
			if accountID != first.ID {
				t.Fatalf("first attempt account=%q want %q", accountID, first.ID)
			}
			for _, event := range []chathub.StreamEvent{oldProgress, oldTool, oldReasoning, {Kind: "text", Text: "partial answer"}} {
				if err := onEvent(event); err != nil {
					return chathub.Result{}, err
				}
			}
			return chathub.Result{}, errors.New("unexpected EOF")
		}
		if calls == 2 {
			if accountID != second.ID {
				t.Fatalf("recovery account=%q want %q", accountID, second.ID)
			}
			for _, event := range []chathub.StreamEvent{oldProgress, oldTool, oldReasoning, newProgress, newTool, newReasoning, {Kind: "text", Text: "a fresh continuation fragment"}} {
				if err := onEvent(event); err != nil {
					return chathub.Result{}, err
				}
			}
			return chathub.Result{Text: "a fresh continuation fragment", RequestID: "recovery-request"}, nil
		}
		t.Fatalf("unexpected recovery attempt %d", calls)
		return chathub.Result{}, nil
	}

	var got []chathub.StreamEvent
	result, used, err := s.streamChatWithRecovery(context.Background(), first, chathub.Request{Text: "answer the question"}, func(event chathub.StreamEvent) error {
		got = append(got, event)
		return nil
	})
	if err != nil {
		t.Fatalf("streamChatWithRecovery() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("attempts=%d want 2", calls)
	}
	if used.ID != second.ID {
		t.Fatalf("used account=%q want %q", used.ID, second.ID)
	}
	if result.RequestID != "recovery-request" {
		t.Fatalf("result request id=%q", result.RequestID)
	}

	want := []string{
		"progress|searching",
		"tool|search",
		"reasoning|I will search first.",
		"text|partial answer",
		"progress|opening the matching result",
		"tool|open_result",
		"reasoning|The result answers the question.",
		"text|a fresh continuation fragment",
	}
	if len(got) != len(want) {
		t.Fatalf("events=%d want %d: %#v", len(got), len(want), got)
	}
	for i, event := range got {
		actual := event.Kind + "|"
		if event.Kind == "tool" {
			actual += event.ToolName
		} else {
			actual += event.Text
		}
		if actual != want[i] {
			t.Fatalf("event[%d]=%q want %q; all=%#v", i, actual, want[i], got)
		}
	}
}
