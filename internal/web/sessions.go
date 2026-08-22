package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
	OwnerID        string    `json:"ownerId,omitempty"`
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]conversation
	persist *persistStore
}

func openSessionStore() *sessionStore {
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-copilot2api-sessions.json")
	}
	s := &sessionStore{path: path, data: map[string]conversation{}}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *sessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		out = append(out, v)
	}
	return out
}

func scopedOwnerKey(ownerID, id string) string {
	if ownerID == "" {
		return id
	}
	return ownerID + "\x00" + id
}

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.data[id]; ok {
		return v, true
	}
	for _, v := range s.data {
		if v.ID == id {
			return v, true
		}
	}
	return conversation{}, false
}

func (s *sessionStore) getForOwner(ownerID, id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[scopedOwnerKey(ownerID, id)]
	if !ok || (ownerID != "" && v.OwnerID != ownerID) {
		return conversation{}, false
	}
	return v, true
}

func (s *sessionStore) upsert(v conversation) conversation {
	return s.upsertForOwner("", v)
}

func (s *sessionStore) upsertForOwner(ownerID string, v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	v.OwnerID = ownerID
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[scopedOwnerKey(ownerID, v.ID)] = v
	s.persist.markDirty()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; ok {
		delete(s.data, id)
		s.persist.markDirty()
		return true
	}
	deleted := false
	for key, v := range s.data {
		if v.ID == id {
			delete(s.data, key)
			deleted = true
		}
	}
	if deleted {
		s.persist.markDirty()
	}
	return deleted
}

func (s *sessionStore) deleteForOwner(ownerID, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scopedOwnerKey(ownerID, id)
	v, ok := s.data[key]
	if !ok || (ownerID != "" && v.OwnerID != ownerID) {
		return false
	}
	delete(s.data, key)
	s.persist.markDirty()
	return true
}

type userSession struct {
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	AccountID      string    `json:"accountId"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

type userSessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]userSession
	ttl     time.Duration
	persist *persistStore
}

func openUserSessionStore(ttl time.Duration) *userSessionStore {
	path := os.Getenv("M365_USER_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-copilot2api-user-sessions.json")
	}
	s := &userSessionStore{path: path, data: map[string]userSession{}, ttl: ttl}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	s.evictLocked()
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *userSessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *userSessionStore) evictLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.ttl)
	for k, v := range s.data {
		if v.LastUsedAt.Before(cutoff) {
			delete(s.data, k)
		}
	}
}

func userSessionKey(ownerID, user string) string {
	return scopedOwnerKey(ownerID, user)
}

func (s *userSessionStore) Get(user string) (userSession, bool) {
	return s.GetForOwner("", user)
}

func (s *userSessionStore) GetForOwner(ownerID, user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	key := userSessionKey(ownerID, user)
	v, ok := s.data[key]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[key] = v
		s.persist.markDirty()
	}
	return v, ok
}

func (s *userSessionStore) Put(user, conversationID, sessionID, accountID string) {
	s.PutForOwner("", user, conversationID, sessionID, accountID)
}

func (s *userSessionStore) PutForOwner(ownerID, user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[userSessionKey(ownerID, user)] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.persist.markDirty()
}

func (s *userSessionStore) Delete(user string) {
	s.DeleteForOwner("", user)
}

func (s *userSessionStore) DeleteForOwner(ownerID, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, userSessionKey(ownerID, user))
	s.persist.markDirty()
}

// ActiveConversations returns conversation IDs whose owning user used the
// session within the given window. The auto-cleanup skips these so a user's
// in-flight conversation is never removed while still in use.
func (s *userSessionStore) ActiveConversations(window time.Duration) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-window)
	out := map[string]bool{}
	for _, v := range s.data {
		if v.LastUsedAt.After(cutoff) {
			out[v.ConversationID] = true
		}
	}
	return out
}
