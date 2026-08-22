package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

type liveStreamRecorder struct {
	header http.Header

	mu     sync.Mutex
	status int
	body   bytes.Buffer

	firstFlush chan struct{}
	flushOnce  sync.Once
}

func newLiveStreamRecorder() *liveStreamRecorder {
	return &liveStreamRecorder{
		header:     make(http.Header),
		firstFlush: make(chan struct{}),
	}
}

func (w *liveStreamRecorder) Header() http.Header { return w.header }

func (w *liveStreamRecorder) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *liveStreamRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *liveStreamRecorder) Flush() {
	w.flushOnce.Do(func() { close(w.firstFlush) })
}

func (w *liveStreamRecorder) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func (w *liveStreamRecorder) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func TestChatStreamForwardsLiveEventsBeforeCompletionWithoutFixedDeadline(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID:      "stream-account",
		TenantID:     "stream-tenant",
		Email:        "stream@example.invalid",
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		accountConcurrency: newAccountConcurrency(),
		sessions: &sessionStore{
			data:    map[string]conversation{},
			persist: &persistStore{flush: func() error { return nil }},
		},
	}

	previous := chatStreamWithEvents
	t.Cleanup(func() { chatStreamWithEvents = previous })

	type invocation struct {
		ctx     context.Context
		request chathub.Request
	}
	seenInvocation := make(chan invocation, 1)
	firstEventReturned := make(chan struct{})
	finishUpstream := make(chan struct{})
	upstreamFinished := make(chan struct{})
	var finishOnce sync.Once
	releaseUpstream := func() { finishOnce.Do(func() { close(finishUpstream) }) }
	t.Cleanup(releaseUpstream)

	chatStreamWithEvents = func(ctx context.Context, _ *Server, _ string, _ chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
		defer close(upstreamFinished)
		seenInvocation <- invocation{ctx: ctx, request: request}
		if err := onEvent(chathub.StreamEvent{Kind: "text", Text: "first token"}); err != nil {
			return chathub.Result{}, err
		}
		close(firstEventReturned)
		<-finishUpstream
		if err := onEvent(chathub.StreamEvent{Kind: "progress", Text: "still working", MessageType: "Progress"}); err != nil {
			return chathub.Result{}, err
		}
		return chathub.Result{
			Text:           "complete answer",
			ConversationID: request.ConversationID,
			SessionID:      request.SessionID,
			RequestID:      "stream-request",
		}, nil
	}

	recorder := newLiveStreamRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(`{"accountId":"stream-account","message":"hello","sessionKey":"stream-session"}`))
	handlerDone := make(chan struct{})
	go func() {
		s.chatStream(recorder, req)
		close(handlerDone)
	}()

	select {
	case <-recorder.firstFlush:
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE frame was not flushed before upstream completion")
	}
	select {
	case <-firstEventReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("live event callback did not return")
	}

	var call invocation
	select {
	case call = <-seenInvocation:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not call the ChatWithEvents compatibility path")
	}
	if _, hasDeadline := call.ctx.Deadline(); hasDeadline {
		t.Fatal("chat stream added a fixed handler deadline instead of preserving the request context")
	}
	if !call.request.Started || call.request.ConversationID == "" || call.request.SessionID == "" {
		t.Fatalf("first stream request did not receive stable IDs: %+v", call.request)
	}
	select {
	case <-upstreamFinished:
		t.Fatal("handler completed upstream work before its first SSE event became visible")
	default:
	}

	releaseUpstream()
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after upstream completion")
	}

	if got := recorder.Status(); got != http.StatusOK {
		t.Fatalf("status=%d want %d", got, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q", got)
	}
	body := recorder.BodyString()
	for _, want := range []string{"event: event", `"kind":"text"`, `"kind":"progress"`, "event: done", `"requestId":"stream-request"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in SSE body: %s", want, body)
		}
	}
	if strings.Index(body, `"kind":"text"`) >= strings.Index(body, "event: done") {
		t.Fatalf("done preceded the first live event: %s", body)
	}
	stored, ok := s.sessions.get("stream-session")
	if !ok || stored.ConversationID != call.request.ConversationID || stored.SessionID != call.request.SessionID {
		t.Fatalf("session writeback=%+v ok=%t, request=%+v", stored, ok, call.request)
	}
}
