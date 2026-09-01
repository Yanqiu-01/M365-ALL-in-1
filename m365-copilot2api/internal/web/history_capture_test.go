package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newHistoryTestResolver() *sessionResolver {
	return &sessionResolver{
		sessions:                  map[string]sessionBinding{},
		byExplicit:                map[string]string{},
		byUserField:               map[string]string{},
		byIPFinger:                map[string]string{},
		byContext:                 map[string]string{},
		ttl:                       2 * time.Hour,
		contextTTL:                2 * time.Hour,
		maxSessions:               100,
		compressionThresholdBytes: 1024,
		compressionRatio:          0.75,
		persist:                   &persistStore{flush: func() error { return nil }},
	}
}

func historyTestRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.50:12345"
	r.Header.Set("User-Agent", "history-test/1.0")
	return r
}

func TestCompressionEventUsesConversationIDAndRetainsDroppedMessages(t *testing.T) {
	sr := newHistoryTestResolver()
	old := []oaiMsg{
		{Role: "user", Content: strings.Repeat("old context ", 160)},
		{Role: "assistant", Content: strings.Repeat("old answer ", 160)},
		{Role: "user", Content: "recent question"},
	}
	sr.Bind("sess-compress", "conv-compress", "acc1", &oaiReq{Messages: old}, "", historyTestRequest())

	newContext := []oaiMsg{
		{Role: "system", Content: "compressed summary of the prior conversation"},
		{Role: "user", Content: "recent question"},
	}
	// The upstream session id may rotate after client-side compression. The
	// cloud conversation id remains the identity that ties both contexts
	// together, so this must still be detected without merging independent
	// per-session bindings.
	event := sr.Bind("sess-compress-rotated", "conv-compress", "acc1", &oaiReq{Messages: newContext}, "", historyTestRequest())
	if event == nil {
		t.Fatal("same conversation with a large context shrink should emit a compression event")
	}
	if event.ConversationID != "conv-compress" {
		t.Fatalf("event conversation id=%q", event.ConversationID)
	}
	if len(event.BeforeMessages) != len(old) || len(event.AfterMessages) != len(newContext) {
		t.Fatalf("before/after message counts=%d/%d", len(event.BeforeMessages), len(event.AfterMessages))
	}
	if len(event.DroppedMessages) != 2 {
		t.Fatalf("dropped messages=%d, want 2", len(event.DroppedMessages))
	}
	if _, ok := sr.GetSession("sess-compress-rotated"); !ok || len(sr.ListSessions()) != 2 {
		t.Fatalf("rotated session binding was not preserved: %#v", sr.ListSessions())
	}
}

func TestCompressionEventDoesNotCrossConversationID(t *testing.T) {
	sr := newHistoryTestResolver()
	old := []oaiMsg{{Role: "user", Content: strings.Repeat("large ", 500)}}
	sr.Bind("sess-id", "conv-before", "acc1", &oaiReq{Messages: old}, "", historyTestRequest())
	newContext := []oaiMsg{{Role: "system", Content: "short summary"}}
	if event := sr.Bind("sess-id", "conv-after", "acc1", &oaiReq{Messages: newContext}, "", historyTestRequest()); event != nil {
		t.Fatalf("changed conversation id must not be treated as compression: %#v", event)
	}
}

func TestCaptureConversationsWritesOneJSONPerRecentConversation(t *testing.T) {
	// 捕获入口随对话管理面板一起默认关闭；显式打开，保持验的是真实的落盘
	// 行为。默认关闭的断言在 conversations_disabled_test.go。
	t.Setenv(envConversationPanel, "1")
	dir := t.TempDir()
	sr := newHistoryTestResolver()
	archive := &historyArchiveStore{dir: dir, maxSnapshots: 10}
	s := &Server{sessionResolver: sr, historyArchive: archive}

	// The second session points at the same conversation and is newer; capture
	// must deduplicate by conversation ID rather than write two files.
	sr.Bind("sess-old", "conv-one", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "old"}}}, "", historyTestRequest())
	sr.Bind("sess-new", "conv-one", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "new"}}}, "", historyTestRequest())
	sr.Bind("sess-two", "conv-two", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "two"}}}, "", historyTestRequest())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/capture", bytes.NewBufferString(`{"limit":2}`))
	s.captureConversations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Selected int      `json:"selected"`
		Captured int      `json:"captured"`
		Files    []string `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != 2 || response.Captured != 2 || len(response.Files) != 2 {
		t.Fatalf("capture response=%s", rec.Body.String())
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("archive files=%d, want 2 (%v)", len(files), files)
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc historyArchiveDocument
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if doc.SchemaVersion != 1 || doc.ConversationID == "" || len(doc.Snapshots) != 1 {
			t.Fatalf("archive %s=%s", path, string(b))
		}
		if doc.Snapshots[0].Reason != "manual" || len(doc.CurrentContext) != 1 {
			t.Fatalf("archive snapshot=%s", string(b))
		}
	}
}

func TestCaptureConversationsRejectsInvalidLimitAndMethod(t *testing.T) {
	// 同上：这里验的是参数校验，得先让请求走到校验那一步。
	t.Setenv(envConversationPanel, "1")
	s := &Server{sessionResolver: newHistoryTestResolver(), historyArchive: &historyArchiveStore{dir: t.TempDir(), maxSnapshots: 10}}
	for _, test := range []struct {
		name   string
		method string
		body   string
		status int
	}{
		{name: "method", method: http.MethodGet, body: `{"limit":1}`, status: http.StatusMethodNotAllowed},
		{name: "zero-negative", method: http.MethodPost, body: `{"limit":-1}`, status: http.StatusBadRequest},
		{name: "too-large", method: http.MethodPost, body: `{"limit":101}`, status: http.StatusBadRequest},
		{name: "bad-json", method: http.MethodPost, body: `{`, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.captureConversations(rec, httptest.NewRequest(test.method, "/api/conversations/capture", bytes.NewBufferString(test.body)))
			if rec.Code != test.status {
				t.Fatalf("status=%d want %d body=%s", rec.Code, test.status, rec.Body.String())
			}
		})
	}
}

func TestCompressionArchiveStoresBeforeAndAfterContext(t *testing.T) {
	dir := t.TempDir()
	archive := &historyArchiveStore{dir: dir, maxSnapshots: 10}
	event := &compressionEvent{
		SessionID:          "sess-archive",
		ConversationID:     "conv-archive",
		AccountID:          "acc1",
		CreatedAt:          time.Now().UTC().Add(-time.Minute),
		DetectedAt:         time.Now().UTC(),
		BeforeContextBytes: 5000,
		AfterContextBytes:  500,
		BeforeMessages:     []oaiMsg{{Role: "user", Content: "before"}},
		AfterMessages:      []oaiMsg{{Role: "system", Content: "summary"}},
		DroppedMessages:    []oaiMsg{{Role: "user", Content: "before"}},
	}
	path, added, err := archive.recordCompression(event, "user@example.com")
	if err != nil || !added {
		t.Fatalf("record compression path=%s added=%t err=%v", path, added, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc historyArchiveDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.CompressionCount != 1 || len(doc.Snapshots) != 1 {
		t.Fatalf("archive=%s", string(b))
	}
	snapshot := doc.Snapshots[0]
	if snapshot.Reason != "compression" || len(snapshot.BeforeContext) != 1 || len(snapshot.AfterContext) != 1 || len(snapshot.DroppedMessages) != 1 {
		t.Fatalf("compression snapshot=%s", string(b))
	}
}

func TestHistoryArchiveUpdatesExistingConversationFile(t *testing.T) {
	dir := t.TempDir()
	archive := &historyArchiveStore{dir: dir, maxSnapshots: 10}
	sess := sessionBinding{
		SessionID:      "sess-update",
		ConversationID: "conv-update",
		AccountID:      "acc1",
		CreatedAt:      time.Now().UTC().Add(-time.Minute),
		LastUsedAt:     time.Now().UTC(),
		ContextHistory: []oaiMsg{{Role: "user", Content: "first"}},
	}
	firstPath, added, err := archive.captureSession(sess, "")
	if err != nil || !added {
		t.Fatalf("first capture path=%s added=%t err=%v", firstPath, added, err)
	}
	sess.ContextHistory = append(sess.ContextHistory, oaiMsg{Role: "assistant", Content: "second"})
	sess.LastUsedAt = time.Now().UTC().Add(time.Second)
	secondPath, added, err := archive.captureSession(sess, "")
	if err != nil || !added || secondPath != firstPath {
		t.Fatalf("second capture path=%s added=%t err=%v", secondPath, added, err)
	}
	b, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc historyArchiveDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Snapshots) != 2 || len(doc.CurrentContext) != 2 {
		t.Fatalf("updated archive=%s", string(b))
	}
}

func TestSessionResolverAppliesConfiguredMaxOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	now := time.Now().UTC()
	seed := []sessionBinding{
		{SessionID: "s1", ConversationID: "c1", LastUsedAt: now.Add(-3 * time.Minute), ContextHistory: []oaiMsg{{Role: "user", Content: "one"}}},
		{SessionID: "s2", ConversationID: "c2", LastUsedAt: now.Add(-2 * time.Minute), ContextHistory: []oaiMsg{{Role: "user", Content: "two"}}},
		{SessionID: "s3", ConversationID: "c3", LastUsedAt: now.Add(-1 * time.Minute), ContextHistory: []oaiMsg{{Role: "user", Content: "three"}}},
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_SESSION_CACHE", path)
	t.Setenv("M365_SESSION_MAX", "2")
	sr := openSessionResolver()
	if got := len(sr.ListSessions()); got != 2 {
		t.Fatalf("loaded sessions=%d, want 2", got)
	}
	if _, ok := sr.GetSession("s1"); ok {
		t.Fatal("least-recently-used session should be evicted during load")
	}
}

func TestSessionResolverSetMaxSessionsEvictsLRUAndPersists(t *testing.T) {
	dir := t.TempDir()
	sr := newHistoryTestResolver()
	sr.path = filepath.Join(dir, "sessions.json")
	sr.persist = &persistStore{flush: sr.flush}
	now := time.Now().UTC()
	sr.sessions = map[string]sessionBinding{
		"s-old": {SessionID: "s-old", ConversationID: "c-old", LastUsedAt: now.Add(-3 * time.Minute)},
		"s-mid": {SessionID: "s-mid", ConversationID: "c-mid", LastUsedAt: now.Add(-2 * time.Minute)},
		"s-new": {SessionID: "s-new", ConversationID: "c-new", LastUsedAt: now.Add(-1 * time.Minute)},
	}
	sr.maxSessions = 100

	if got := sr.SetMaxSessions(2); got != 1 {
		t.Fatalf("evicted=%d, want 1", got)
	}
	if sr.MaxSessions() != 2 || len(sr.ListSessions()) != 2 {
		t.Fatalf("resolver state max=%d sessions=%d", sr.MaxSessions(), len(sr.ListSessions()))
	}
	if _, ok := sr.GetSession("s-old"); ok {
		t.Fatal("least-recently-used session was not evicted")
	}

	b, err := os.ReadFile(sr.path)
	if err != nil {
		t.Fatalf("read persisted sessions: %v", err)
	}
	var persisted []sessionBinding
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatalf("decode persisted sessions: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted sessions=%d, want 2", len(persisted))
	}
}

func TestCompressionEventDoesNotCrossTenant(t *testing.T) {
	sr := newHistoryTestResolver()
	oldReq := historyTestRequest()
	oldReq.Header.Set("X-API-Key", "tenant-a-secret")
	old := []oaiMsg{{Role: "user", Content: strings.Repeat("large context ", 500)}}
	sr.Bind("sess-tenant-a", "conv-shared", "acc1", &oaiReq{Messages: old}, "", oldReq)

	newReq := historyTestRequest()
	newReq.Header.Set("X-API-Key", "tenant-b-secret")
	newContext := []oaiMsg{{Role: "system", Content: "short summary"}}
	if event := sr.Bind("sess-tenant-b", "conv-shared", "acc1", &oaiReq{Messages: newContext}, "", newReq); event != nil {
		t.Fatalf("different tenants must not be treated as compression: %#v", event)
	}
}
