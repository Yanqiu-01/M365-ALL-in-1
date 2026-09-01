package web

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The retained caches must scale with the configured windows, not with the
// entire transcript. Keep this deliberately below the per-session context cap:
// it verifies the two derived-cache bounds independently of oversized-session
// eviction, which has its own safety test below.
func TestSessionResolverBoundsDerivedHistoryCaches(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_SESSION_SIMILARITY_WINDOW_MESSAGES", "3")
	t.Setenv("M365_SESSION_SIMILARITY_WINDOW_BYTES", "4096")

	history := make([]oaiMsg, 0, 40)
	for i := 0; i < 40; i++ {
		history = append(history, oaiMsg{
			Role:    "user",
			Content: fmt.Sprintf("old-%d %s", i, strings.Repeat(fmt.Sprintf("unique%d ", i), 32)),
		})
	}
	sr := openSessionResolver()
	sr.Bind("bounded-caches", "conv-bounded-caches", "acc", &oaiReq{Messages: history}, "",
		resolverTestRequest("203.0.113.80", "memory-test", "alice"))

	sess, ok := sr.GetSession("bounded-caches")
	if !ok {
		t.Fatal("normal-sized session was not retained")
	}
	if len(sess.contentHashes) != len(history) {
		t.Fatalf("digest count = %d, want %d", len(sess.contentHashes), len(history))
	}
	for i, digest := range sess.contentHashes {
		if len(digest) != 64 {
			t.Fatalf("digest[%d] length = %d, want fixed SHA-256 hex width 64", i, len(digest))
		}
		if strings.Contains(digest, "unique") {
			t.Fatalf("digest[%d] retained message text", i)
		}
	}
	if len(sess.contextTokens) == 0 {
		t.Fatal("recent-window token set must retain the continuation signal")
	}
	if len(sess.contextTokens) > 100 {
		t.Fatalf("token set = %d entries, want bounded recent-window footprint", len(sess.contextTokens))
	}
	if sess.contextTokens["unique0"] {
		t.Fatal("token set retained an old message outside the configured suffix")
	}
	if !sess.contextTokens["unique39"] {
		t.Fatal("token set omitted the most recent message")
	}

	// Digesting only changes the representation of the equality key. For a
	// normal retained history it must preserve the exact incremental boundary.
	next := append(append([]oaiMsg(nil), history...), oaiMsg{Role: "user", Content: "normal next turn"})
	res := sr.Resolve(resolverTestRequest("203.0.113.80", "memory-test", "alice"), &oaiReq{Messages: next})
	if res.IsNew || res.HistoryLen != len(history) {
		t.Fatalf("normal prefix resolved %#v, want retained boundary %d", res, len(history))
	}
}

// Oversized histories must not be truncated in place: a truncated suffix is not
// a valid request prefix, so reporting its length as HistoryLen would make the
// caller drop or duplicate conversation turns. The resolver evicts instead;
// Resolve then returns IsNew/HistoryLen zero and the caller sends the full body.
func TestSessionResolverOversizedHistoryDoesNotInventIncrementalBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_SESSION_MAX_CONTEXT_BYTES", "65536")

	history := buildLongHistory(4, 24) // just above the accepted minimum bound
	req := resolverTestRequest("203.0.113.81", "memory-test", "alice")
	sr := openSessionResolver()
	sr.Bind("oversized", "conv-oversized", "acc", &oaiReq{Messages: history}, "", req)
	if _, ok := sr.GetSession("oversized"); ok {
		t.Fatal("oversized history must be ineligible for incremental reuse, not retained truncated")
	}

	res := sr.Resolve(req, &oaiReq{Messages: append(history, oaiMsg{Role: "user", Content: "next"})})
	if !res.IsNew || res.HistoryLen != 0 {
		t.Fatalf("oversized history resolved %#v; want new/full-body path with HistoryLen 0", res)
	}
}

func TestReindexRemovesSupersededDiagnosticKeys(t *testing.T) {
	sr := &sessionResolver{
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
	}
	old := sessionBinding{SessionID: "s", UserField: "old-user", IPFingerprint: "old-ip", ContextFinger: "old-context"}
	sr.reindexLocked(old)
	updated := sessionBinding{SessionID: "s", UserField: "new-user", IPFingerprint: "new-ip", ContextFinger: "new-context"}
	sr.reindexLocked(updated)

	for name, index := range map[string]map[string]string{
		"user":    sr.byUserField,
		"ip":      sr.byIPFinger,
		"context": sr.byContext,
	} {
		for _, stale := range []string{"old-user", "old-ip", "old-context"} {
			if index[stale] != "" {
				t.Fatalf("%s index retained stale key %q", name, stale)
			}
		}
	}
	if sr.byUserField["new-user"] != "s" || sr.byIPFinger["new-ip"] != "s" || sr.byContext["new-context"] != "s" {
		t.Fatalf("new diagnostic keys not indexed: user=%q ip=%q context=%q", sr.byUserField["new-user"], sr.byIPFinger["new-ip"], sr.byContext["new-context"])
	}
}
