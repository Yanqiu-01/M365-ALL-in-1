package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func affinityResolverFixture(t *testing.T, cloudID string, history []oaiMsg) (*sessionResolver, *http.Request) {
	t.Helper()
	t.Setenv("M365_CONTEXT_SIMILARITY", "0.6")
	sr := &sessionResolver{
		sessions: map[string]sessionBinding{}, ttl: 2 * time.Hour,
		contextTTL: 2 * time.Hour, maxSessions: defaultMaxSessions,
		persist: &persistStore{flush: func() error { return nil }},
	}
	r := resolverTestRequest("203.0.113.10", "affinity-regression", "")
	sr.Bind("local-session", cloudID, "account-first", &oaiReq{Messages: history}, "", r)
	return sr, r
}

func TestAffinityWeakMatchRejectsLocalOnlyRecord(t *testing.T) {
	history := []oaiMsg{
		{Role: "user", Content: "alpha beta gamma delta"},
		{Role: "assistant", Content: "epsilon zeta eta theta"},
	}
	sr, r := affinityResolverFixture(t, "", history)
	messages := []oaiMsg{history[1], {Role: "user", Content: "alpha beta gamma"}}
	if score := contextSimilarity(history, messages); score < 0.6 {
		t.Fatalf("fixture must exercise weak matching, score=%f", score)
	}
	before, _ := sr.GetSession("local-session")
	got := sr.Resolve(r, &oaiReq{Messages: messages})
	if !got.IsNew || got.AccountID != "" {
		t.Fatalf("local-only record captured unrelated account affinity: %+v", got)
	}
	after, ok := sr.GetSession("local-session")
	if !ok || !after.LastUsedAt.Equal(before.LastUsedAt) {
		t.Fatal("rejected candidate must remain stored without extending its lifetime")
	}
}

func TestAffinityWeakMatchRejectsFirstTurn(t *testing.T) {
	history := []oaiMsg{
		{Role: "user", Content: "alpha beta gamma delta epsilon zeta eta theta"},
		{Role: "assistant", Content: "alpha beta gamma delta epsilon zeta eta theta"},
	}
	sr, r := affinityResolverFixture(t, "cloud-conversation", history)
	messages := []oaiMsg{{Role: "user", Content: "alpha beta gamma delta epsilon zeta eta"}}
	if score := contextSimilarity(history, messages); score < 0.6 {
		t.Fatalf("fixture must exercise weak matching, score=%f", score)
	}
	if got := sr.Resolve(r, &oaiReq{Messages: messages}); !got.IsNew || got.AccountID != "" {
		t.Fatalf("first-turn vocabulary overlap must not select a previous account: %+v", got)
	}
}

func TestAffinitySharedInstructionsDoNotIdentifyConversation(t *testing.T) {
	var common strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&common, "shared-instruction-%d ", i)
	}
	history := []oaiMsg{
		{Role: "system", Content: common.String()},
		{Role: "developer", Content: common.String()},
		{Role: "user", Content: "apple orchard harvest"},
		{Role: "assistant", Content: "fruit pruning branches"},
	}
	sr, r := affinityResolverFixture(t, "cloud-conversation", history)
	messages := []oaiMsg{
		history[0], history[1],
		{Role: "assistant", Content: "database query indexes"},
		{Role: "user", Content: "network packet routing"},
	}
	if got := sr.Resolve(r, &oaiReq{Messages: messages}); !got.IsNew || got.AccountID != "" {
		t.Fatalf("shared harness instructions captured account affinity: %+v", got)
	}
	if score := contextSimilarity(history, messages); score >= 0.6 {
		t.Fatalf("instructions must not dominate similarity, score=%f", score)
	}
}

func TestAffinityReliableLocalContinuationKeepsAccountAndFullHistory(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			history := []oaiMsg{{Role: "user", Content: "inspect source"}, {Role: "assistant", Content: "read completed"}}
			sr, r := affinityResolverFixture(t, "", history)
			if explicit {
				r.Header.Set("X-M365-Session-Id", "local-session")
			}
			messages := append(cloneMessages(history), oaiMsg{Role: "user", Content: "continue inspection"})
			got := sr.Resolve(r, &oaiReq{Messages: messages})
			if got.IsNew || got.AccountID != "account-first" || got.ConversationID != "" || got.HistoryLen != 0 {
				t.Fatalf("local continuation must keep account but replay all history: %+v", got)
			}
		})
	}
}

func TestAffinityCloudSimilarityPreservesAccountTuple(t *testing.T) {
	history := []oaiMsg{{Role: "user", Content: "alpha beta gamma delta"}, {Role: "assistant", Content: "epsilon zeta eta theta"}}
	sr, r := affinityResolverFixture(t, "cloud-conversation", history)
	messages := []oaiMsg{history[1], {Role: "user", Content: "alpha beta gamma"}}
	got := sr.Resolve(r, &oaiReq{Messages: messages})
	if got.IsNew || got.AccountID != "account-first" || got.ConversationID != "cloud-conversation" || got.HistoryLen != 0 || !strings.HasPrefix(got.MatchedBy, "context_similar_") {
		t.Fatalf("valid cloud continuation stopped working: %+v", got)
	}
	got = sr.Resolve(r, &oaiReq{AccountID: "account-other", Messages: messages})
	if !got.IsNew {
		t.Fatalf("weak matching paired another account with a foreign cloud conversation: %+v", got)
	}
}
