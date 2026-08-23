package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"hash,omitempty"`
	Raw        string     `json:"raw,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	// Revoked 是启用状态的唯一真相源。对外暴露的 enabled 字段由它派生
	// （见 apiKeyView），PATCH 收到 enabled 时也只是写回本字段，
	// 避免两个可写字段互相矛盾。
	Revoked bool `json:"revoked"`
	// AllowedModelIDs 为空（nil 或长度 0）表示不限制可用模型；非空则只允许
	// 列表内的 model id。旧版 api-keys.json 缺少该字段时按不限制读入。
	AllowedModelIDs []string `json:"allowedModelIds,omitempty"`
	// MaxConcurrent 为 0 表示沿用全局默认并发；>0 则为该 key 的并发上限。
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// RPMLimit 为 0 表示不限每分钟请求数。
	RPMLimit int `json:"rpmLimit,omitempty"`
}

// apiKeyView 是 API key 的对外形状。enabled 是 Revoked 的派生只读投影，
// 不参与持久化，因此磁盘上不存在第二份可能过期的启用状态。
type apiKeyView struct {
	apiKeyRecord
	Enabled bool `json:"enabled"`
}

func newAPIKeyView(r apiKeyRecord) apiKeyView {
	r.Hash = ""
	r.Raw = ""
	if r.AllowedModelIDs == nil {
		r.AllowedModelIDs = []string{}
	}
	return apiKeyView{apiKeyRecord: r, Enabled: !r.Revoked}
}

// apiKeyPatch 用指针区分「字段未提供」与「显式设为零值」。
type apiKeyPatch struct {
	Name            *string   `json:"name"`
	AllowedModelIDs *[]string `json:"allowedModelIds"`
	MaxConcurrent   *int      `json:"maxConcurrent"`
	RPMLimit        *int      `json:"rpmLimit"`
	Revoked         *bool     `json:"revoked"`
	Enabled         *bool     `json:"enabled"`
}

// resolveRevoked 把 revoked / enabled 折叠成单一真相。两者同时出现且互相
// 矛盾时报错，而不是任选其一。
func (p apiKeyPatch) resolveRevoked() (*bool, error) {
	switch {
	case p.Revoked == nil && p.Enabled == nil:
		return nil, nil
	case p.Revoked != nil && p.Enabled != nil:
		if *p.Revoked == !*p.Enabled {
			return p.Revoked, nil
		}
		return nil, errConflictingKeyState
	case p.Revoked != nil:
		return p.Revoked, nil
	default:
		revoked := !*p.Enabled
		return &revoked, nil
	}
}

type apiKeyStore struct {
	mu      sync.Mutex
	Path    string
	Keys    []apiKeyRecord `json:"keys"`
	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-copilot2api", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		migrated := false
		for i := range s.Keys {
			if s.Keys[i].Raw != "" {
				if s.Keys[i].Hash == "" {
					s.Keys[i].Hash = keyHash(s.Keys[i].Raw)
				}
				s.Keys[i].Raw = ""
				migrated = true
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: raw[:12], Hash: keyHash(raw), CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		s.Keys = s.Keys[:len(s.Keys)-1]
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	r.Hash = ""
	r.Raw = ""
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		out[i].Hash = ""
		out[i].Raw = ""
	}
	return out
}

// listViews 返回带派生 enabled 字段的对外视图。
func (s *apiKeyStore) listViews() []apiKeyView {
	records := s.list()
	out := make([]apiKeyView, 0, len(records))
	for _, r := range records {
		out = append(out, newAPIKeyView(r))
	}
	return out
}

func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			s.mu.Unlock()
			if err := s.persist.flushNowBlocking(); err != nil {
				s.mu.Lock()
				s.Keys[i].Revoked = false
				s.mu.Unlock()
				return false, err
			}
			return true, nil
		}
	}
	s.mu.Unlock()
	return false, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		removed := s.Keys[i]
		s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
		s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = append(s.Keys[:i], append([]apiKeyRecord{removed}, s.Keys[i:]...)...)
			s.mu.Unlock()
			return false, err
		}
		return true, nil
	}
	s.mu.Unlock()
	return false, nil
}

// update 按 patch 做部分更新，落盘失败时回滚整条记录。
func (s *apiKeyStore) update(id string, patch apiKeyPatch) (apiKeyRecord, bool, error) {
	revoked, err := patch.resolveRevoked()
	if err != nil {
		return apiKeyRecord{}, false, err
	}
	s.mu.Lock()
	index := -1
	var previous apiKeyRecord
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		index = i
		previous = s.Keys[i]
		previous.AllowedModelIDs = append([]string(nil), s.Keys[i].AllowedModelIDs...)
		if patch.Name != nil && strings.TrimSpace(*patch.Name) != "" {
			s.Keys[i].Name = strings.TrimSpace(*patch.Name)
		}
		if patch.AllowedModelIDs != nil {
			s.Keys[i].AllowedModelIDs = normalizeModelIDs(*patch.AllowedModelIDs)
		}
		if patch.MaxConcurrent != nil {
			s.Keys[i].MaxConcurrent = clampNonNegative(*patch.MaxConcurrent)
		}
		if patch.RPMLimit != nil {
			s.Keys[i].RPMLimit = clampNonNegative(*patch.RPMLimit)
		}
		if revoked != nil {
			s.Keys[i].Revoked = *revoked
		}
		break
	}
	if index < 0 {
		s.mu.Unlock()
		return apiKeyRecord{}, false, nil
	}
	updated := s.Keys[index]
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		if index < len(s.Keys) && s.Keys[index].ID == id {
			s.Keys[index] = previous
		}
		s.mu.Unlock()
		return apiKeyRecord{}, false, err
	}
	updated.Hash = ""
	updated.Raw = ""
	return updated, true, nil
}

func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// normalizeModelIDs 去空白、去重并排序，让白名单在磁盘上有稳定形状。
// 返回长度 0 时表示「不限制」，与 nil 等价。
func normalizeModelIDs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookup 只读地按明文 key 找出记录，不更新 LastUsedAt。鉴权判定（valid）
// 与策略判定（lookup）分开，避免策略查询污染最近使用时间。
func (s *apiKeyStore) lookup(raw string) (apiKeyRecord, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return apiKeyRecord{}, false
	}
	h := keyHash(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].Hash == h {
			out := s.Keys[i]
			out.Hash = ""
			out.Raw = ""
			out.AllowedModelIDs = append([]string(nil), s.Keys[i].AllowedModelIDs...)
			return out, true
		}
	}
	return apiKeyRecord{}, false
}

func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	h := keyHash(raw)
	found := false
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			found = true
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.persist.markDirty()
	}
	return found
}
