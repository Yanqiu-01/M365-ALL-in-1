package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ownedSessionRequest(sessionID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "127.0.0.1:43210"
	r.Header.Set("User-Agent", "owner-isolation-test")
	if sessionID != "" {
		r.Header.Set(sessionHeaderName, sessionID)
	}
	return r
}

func TestSessionResolverIsolatesAPIKeyOwners(t *testing.T) {
	sr := &sessionResolver{
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         time.Hour,
		contextTTL:  time.Hour,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: func() error { return nil }}
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "private prompt"}}}
	sr.BindOwned("", "conversation-a", "account-a", "key-a", body, "", ownedSessionRequest("shared-session"))

	if result := sr.ResolveOwned(ownedSessionRequest("shared-session"), body, "key-b"); !result.IsNew {
		t.Fatal("different API key reused an explicitly addressed session")
	}
	if _, ok := sr.GetSessionForOwner("key-b", "shared-session"); ok {
		t.Fatal("different API key could read another owner's session")
	}
	if _, ok := sr.GetSessionForOwner("key-a", "shared-session"); !ok {
		t.Fatal("owning API key could not read its session")
	}
	if sr.DeleteSessionForOwner("key-b", "shared-session") {
		t.Fatal("different API key could delete another owner's session")
	}

	sr.BindOwned("", "conversation-b", "account-b", "key-b", body, "", ownedSessionRequest("shared-session"))
	ownerA := sr.ListSessionsForOwner("key-a")
	ownerB := sr.ListSessionsForOwner("key-b")
	if len(ownerA) != 1 || len(ownerB) != 1 {
		t.Fatalf("unexpected owner session counts: ownerA=%d ownerB=%d", len(ownerA), len(ownerB))
	}
	if ownerA[0].ConversationID != "conversation-a" || ownerB[0].ConversationID != "conversation-b" {
		t.Fatalf("owner sessions crossed: ownerA=%q ownerB=%q", ownerA[0].ConversationID, ownerB[0].ConversationID)
	}
	if ownerA[0].SessionID == ownerB[0].SessionID {
		t.Fatal("conflicting explicit session ID was reused across owners")
	}
}

func TestSessionsRouteScopesDataToAuthenticatedAPIKey(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	ownerA, rawA, err := store.create("owner-a")
	if err != nil {
		t.Fatal(err)
	}
	_, rawB, err := store.create("owner-b")
	if err != nil {
		t.Fatal(err)
	}

	resolver := &sessionResolver{
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         time.Hour,
		contextTTL:  time.Hour,
		maxSessions: defaultMaxSessions,
	}
	resolver.persist = &persistStore{flush: func() error { return nil }}
	resolver.BindOwned("", "conversation-a", "account-a", ownerA.ID, &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "private prompt"}}}, "", ownedSessionRequest("shared-session"))

	s := &Server{
		apiKeys:         store,
		sessionResolver: resolver,
		debug:           &debugStore{path: t.TempDir() + "/debug.jsonl"},
	}
	handler := s.Routes()
	request := func(method, target, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	if got := request(http.MethodGet, "/v1/sessions", "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("missing API key status=%d, want %d", got.Code, http.StatusUnauthorized)
	}
	if got := request(http.MethodGet, "/v1/sessions", rawB, ""); got.Code != http.StatusOK || strings.Contains(got.Body.String(), "shared-session") {
		t.Fatalf("owner B listed owner A's session: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request(http.MethodPost, "/v1/sessions", rawB, `{"session_id":"shared-session"}`); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"created"`) {
		t.Fatalf("owner B resolved owner A's session: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request(http.MethodDelete, "/v1/sessions/shared-session", rawB, ""); got.Code != http.StatusNotFound {
		t.Fatalf("owner B deleted owner A's session: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request(http.MethodPost, "/v1/sessions", rawA, `{"session_id":"shared-session"}`); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"active"`) {
		t.Fatalf("owner A could not resolve its session: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request(http.MethodDelete, "/v1/sessions/shared-session", rawA, ""); got.Code != http.StatusOK {
		t.Fatalf("owner A could not delete its session: status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestConversationCacheIsolatesAPIKeyOwners(t *testing.T) {
	c := newConversationCache()
	c.Store("key-a", "account-a", "m365-copilot", &cachedConversation{ConversationID: "conversation-a"})
	if c.Lookup("key-b", "account-a", "m365-copilot") != nil {
		t.Fatal("conversation cache leaked an entry to another API key")
	}
	if got := c.Lookup("key-a", "account-a", "m365-copilot"); got == nil || got.ConversationID != "conversation-a" {
		t.Fatal("owning API key could not read its conversation cache entry")
	}
}

func TestRequestKeyLoggingUsesNonSecretIdentity(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, raw, err := store.create("owner")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{apiKeys: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-API-Key", raw)
	identity, ok := s.apiKeyIdentityForRequest(req)
	if !ok {
		t.Fatal("registered API key was not authenticated")
	}
	req = withAPIKeyIdentity(req, identity)
	if got := extractAPIKey(req); got != record.Prefix {
		t.Fatalf("expected stored safe prefix %q, got %q", record.Prefix, got)
	}
	if got := extractAPIKey(req); strings.Contains(got, raw) {
		t.Fatal("raw API key appeared in a logging identity")
	}
}

func TestClientSessionStoresIsolateAPIKeyOwners(t *testing.T) {
	sessionStore := &sessionStore{
		data:    map[string]conversation{},
		persist: &persistStore{flush: func() error { return nil }},
	}
	sessionStore.upsertForOwner("key-a", conversation{ID: "shared-key", AccountID: "account-a"})
	sessionStore.upsertForOwner("key-b", conversation{ID: "shared-key", AccountID: "account-b"})
	if got, ok := sessionStore.getForOwner("key-a", "shared-key"); !ok || got.AccountID != "account-a" {
		t.Fatal("owner A did not get its own client session")
	}
	if got, ok := sessionStore.getForOwner("key-b", "shared-key"); !ok || got.AccountID != "account-b" {
		t.Fatal("owner B did not get its own client session")
	}

	userStore := &userSessionStore{
		data:    map[string]userSession{},
		ttl:     time.Hour,
		persist: &persistStore{flush: func() error { return nil }},
	}
	userStore.PutForOwner("key-a", "same-user", "conversation-a", "session-a", "account-a")
	userStore.PutForOwner("key-b", "same-user", "conversation-b", "session-b", "account-b")
	if got, ok := userStore.GetForOwner("key-a", "same-user"); !ok || got.ConversationID != "conversation-a" {
		t.Fatal("owner A did not get its own user session")
	}
	if got, ok := userStore.GetForOwner("key-b", "same-user"); !ok || got.ConversationID != "conversation-b" {
		t.Fatal("owner B did not get its own user session")
	}
}
