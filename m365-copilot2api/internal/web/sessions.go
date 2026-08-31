package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
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

// conversationIndexPath 决定 sessionStore 的落盘位置。
//
// 这里以前直接用 M365_SESSION_CACHE，和 openSessionResolver 是同一个变量 ——
// 而两者的 JSON 形状不兼容：sessionStore 写 map[string]conversation（对象），
// sessionResolver 写 []sessionBinding（数组）。一旦运维设了这个变量，两个 store
// 就指向同一个文件，各自 flush 时整文件覆写对方的内容，重启后 Unmarshal 失败
// 且错误被忽略，于是至少有一方静默地读出空数据。docker-compose.yml 里
// M365_SESSION_CACHE=/data/sessions.json 正好命中这条路径。
//
// README 把 M365_SESSION_CACHE 记作「会话绑定缓存（默认 sessions.json）」，
// 也就是 sessionResolver 的语义，所以那个变量归 resolver。sessionStore 改为
// 在同一目录下另取一个文件名：仍然只需要配一个变量就能把两份状态一起挪走
// （测试里也就仍然一起落在 t.TempDir() 内），但两者永不共用同一个文件。
// 需要单独指定时用 M365_CONVERSATION_INDEX_CACHE 显式覆盖。
func conversationIndexPath() string {
	if explicit := strings.TrimSpace(os.Getenv("M365_CONVERSATION_INDEX_CACHE")); explicit != "" {
		return explicit
	}
	if shared := strings.TrimSpace(os.Getenv("M365_SESSION_CACHE")); shared != "" {
		dir, file := filepath.Split(shared)
		ext := filepath.Ext(file)
		stem := strings.TrimSuffix(file, ext)
		if stem == "" {
			stem = "sessions"
		}
		if ext == "" {
			ext = ".json"
		}
		return filepath.Join(dir, stem+"-conversations"+ext)
	}
	return filepath.Join(os.TempDir(), "m365-copilot2api-sessions.json")
}

func openSessionStore() *sessionStore {
	path := conversationIndexPath()
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

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *sessionStore) upsert(v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[v.ID] = v
	s.persist.markDirty()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return false
	}
	delete(s.data, id)
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

func (s *userSessionStore) Get(user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	v, ok := s.data[user]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[user] = v
		s.persist.markDirty()
	}
	return v, ok
}

func (s *userSessionStore) Put(user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[user] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.persist.markDirty()
}

func (s *userSessionStore) Delete(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, user)
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
