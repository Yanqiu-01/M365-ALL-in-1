package web

import (
	"path/filepath"
	"testing"
)

// A client re-sending the same request must not be folded into the conversation
// that already answered it. Jaccard similarity scores "history has one extra
// assistant reply" at 0.67, clearing the 0.6 reuse threshold, so the resend was
// routed into the existing upstream conversation and sent in full. Upstream saw
// content it had already answered and returned an empty completion, which the
// client surfaced as a blank reply.
func TestIdenticalResendStartsFreshConversation(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	req := resolverTestRequest("203.0.113.10", "client-a", "alice")
	first := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "say OK"}}}

	// Turn 1 completes: stored history is the user turn plus the reply.
	sr.Bind("sess-1", "conv-1", "acc1", first, "OK", req)

	// The same single-turn request arrives again.
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "say OK"}}})
	if !res.IsNew {
		t.Fatalf("identical resend reused conversation %s (matched=%s); upstream would see duplicate content and answer empty",
			res.ConversationID, res.MatchedBy)
	}
}

// A genuine follow-up still has to reuse the conversation: the fix must not
// turn every turn into a new upstream conversation.
func TestGenuineFollowUpStillReusesConversation(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	req := resolverTestRequest("203.0.113.10", "client-a", "alice")
	sr.Bind("sess-1", "conv-1", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "say OK"}}}, "OK", req)

	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "say OK"},
		{Role: "assistant", Content: "OK"},
		{Role: "user", Content: "now say PONG"},
	}})
	if res.IsNew {
		t.Fatal("a real follow-up must continue the existing conversation")
	}
	if res.HistoryLen != 2 {
		t.Errorf("increment should start after the 2 known messages, got HistoryLen=%d", res.HistoryLen)
	}
}