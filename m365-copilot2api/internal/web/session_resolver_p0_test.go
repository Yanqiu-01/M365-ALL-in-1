package web

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// buildLongHistory mirrors the shape that saturated the CPU: a long history
// of large messages, persisted and then reloaded.
func buildLongHistory(messages, sizeKB int) []oaiMsg {
	filler := strings.Repeat("x", sizeKB*1024)
	out := make([]oaiMsg, 0, messages)
	for i := 0; i < messages; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, oaiMsg{Role: role, Content: fmt.Sprintf("m%d-%s", i, filler)})
	}
	return out
}

// P0-1: contentHashes/contextTokens are json:"-", so a reloaded session used to
// come back with an empty cache and fall back to re-materialising its whole
// history on every request. That is the CPU spike, and it returned on restart.
func TestLoadedSessionRehydratesCaches(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "sessions.json")
	t.Setenv("M365_SESSION_CACHE", cache)
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))

	history := buildLongHistory(24, 1)
	first := openSessionResolver()
	first.Bind("sess-reload", "conv-reload", "acc1", &oaiReq{Messages: history}, "",
		resolverTestRequest("203.0.113.20", "client-reload", "alice"))
	if err := first.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reloaded := openSessionResolver()
	sess, ok := reloaded.GetSession("sess-reload")
	if !ok {
		t.Fatal("session did not survive the reload")
	}
	if len(sess.contentHashes) != len(sess.ContextHistory) {
		t.Fatalf("contentHashes not rebuilt on load: got %d want %d", len(sess.contentHashes), len(sess.ContextHistory))
	}
	if len(sess.contextTokens) == 0 {
		t.Fatal("contextTokens not rebuilt on load")
	}

	// The rebuilt cache must still produce the same incremental boundary.
	next := append(append([]oaiMsg(nil), history...), oaiMsg{Role: "user", Content: "next turn"})
	res := reloaded.Resolve(resolverTestRequest("203.0.113.20", "client-reload", "alice"), &oaiReq{Messages: next})
	if res.IsNew {
		t.Fatal("reloaded session must still match by strict prefix")
	}
	if res.HistoryLen != len(history) {
		t.Fatalf("HistoryLen = %d, want %d", res.HistoryLen, len(history))
	}
}

// P0-4: the cached comparison returned on hash equality alone. The hash covers
// only role + text, so two assistant turns with different tool_calls compared
// equal and an unrelated turn could join an existing conversation.
func TestMessagesEqualCachedStillComparesToolCalls(t *testing.T) {
	call := func(name string) []map[string]any {
		return []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}}
	}
	a := oaiMsg{Role: "assistant", Content: "same text", ToolCalls: call("read_file")}
	b := oaiMsg{Role: "assistant", Content: "same text", ToolCalls: call("delete_file")}

	aHashes := computeContentHashes([]oaiMsg{a})
	bHashes := computeContentHashes([]oaiMsg{b})
	if aHashes[0] != bHashes[0] {
		t.Fatal("precondition: the text hash is expected to collide here")
	}
	if messagesEqualCached(a, b, 0, aHashes, bHashes) {
		t.Fatal("different tool_calls must not compare equal on the cached path")
	}
	if !messagesEqualCached(a, a, 0, aHashes, aHashes) {
		t.Fatal("identical messages must still compare equal")
	}
}

// P0-2: the similarity fallback must agree with the uncached implementation.
func TestContextSimilarityCachedMatchesUncached(t *testing.T) {
	hist := []oaiMsg{{Role: "user", Content: "alpha beta gamma"}, {Role: "assistant", Content: "delta epsilon"}}
	msgs := []oaiMsg{{Role: "user", Content: "alpha beta gamma"}, {Role: "assistant", Content: "delta zeta"}}

	want := contextSimilarity(hist, msgs)
	got := contextSimilarityCached(hist, messageTokenSet(hist), msgs, messageTokenSet(msgs))
	if got != want {
		t.Fatalf("cached similarity = %v, want %v", got, want)
	}
	if fallback := contextSimilarityCached(hist, nil, msgs, nil); fallback != want {
		t.Fatalf("nil-cache fallback = %v, want %v", fallback, want)
	}
	if empty := contextSimilarityCached(nil, nil, msgs, messageTokenSet(msgs)); empty != 0 {
		t.Fatalf("empty history similarity = %v, want 0", empty)
	}
}

// BenchmarkResolveLongHistory is the regression guard for the CPU spike: a
// 148-message history resolved once per request.
func BenchmarkResolveLongHistory(b *testing.B) {
	b.Setenv("M365_SESSION_CACHE", filepath.Join(b.TempDir(), "sessions.json"))
	b.Setenv("M365_CONVERSATION_CACHE", filepath.Join(b.TempDir(), "conversations.json"))
	b.Setenv("M365_USER_SESSION_CACHE", filepath.Join(b.TempDir(), "users.json"))

	history := buildLongHistory(148, 4)
	sr := openSessionResolver()
	sr.Bind("sess-bench", "conv-bench", "acc1", &oaiReq{Messages: history}, "",
		resolverTestRequest("203.0.113.30", "client-bench", "alice"))
	next := append(append([]oaiMsg(nil), history...), oaiMsg{Role: "user", Content: "next turn"})
	req := resolverTestRequest("203.0.113.30", "client-bench", "alice")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := sr.Resolve(req, &oaiReq{Messages: next}); res.IsNew {
			b.Fatal("benchmark must exercise the matched path")
		}
	}
}

// BenchmarkSimilarityFallbackManySessions is the P0-2 guard: a strict-prefix
// miss used to re-flatten and re-tokenise every stored session per request.
func BenchmarkSimilarityFallbackManySessions(b *testing.B) {
	b.Setenv("M365_SESSION_CACHE", filepath.Join(b.TempDir(), "sessions.json"))
	b.Setenv("M365_CONVERSATION_CACHE", filepath.Join(b.TempDir(), "conversations.json"))
	b.Setenv("M365_USER_SESSION_CACHE", filepath.Join(b.TempDir(), "users.json"))

	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.40", "client-sim", "alice")
	for i := 0; i < 20; i++ {
		history := buildLongHistory(40, 2)
		history[0] = oaiMsg{Role: "user", Content: fmt.Sprintf("session-%d root", i)}
		sr.Bind(fmt.Sprintf("sess-sim-%d", i), fmt.Sprintf("conv-sim-%d", i), "acc1",
			&oaiReq{Messages: history}, "", req)
	}

	// Drop the first message so no session is a strict prefix; this forces the
	// similarity scan across all 20 stored sessions.
	probe := buildLongHistory(40, 2)[1:]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sr.Resolve(req, &oaiReq{Messages: probe})
	}
}
