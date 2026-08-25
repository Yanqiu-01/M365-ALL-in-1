package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

// 自定义 key 口令的形状约束。下界保证熵不低到可枚举，上界避免把
// Authorization 头撑爆；字符集限制在 ASCII 可见范围（0x21-0x7e）内，因为
// 空格与控制字符会直接破坏 "Authorization: Bearer <key>" 的解析。
const (
	customKeySecretMinLen = 16
	customKeySecretMaxLen = 128
)

// 这两个哨兵错误让 HTTP 层把 400 与 409 分开。消息里刻意不回显调用方提交的
// 明文片段，因此错误可以安全地直接写给客户端。
var (
	errKeySecretInvalid   = errors.New("自定义密钥必须是 16-128 个 ASCII 可见字符（0x21-0x7e），不能包含空格或控制字符")
	errKeySecretDuplicate = errors.New("该密钥已存在，请换一个")
)

// validateKeySecret 只判定形状，不返回任何明文内容。
func validateKeySecret(secret string) error {
	if n := len(secret); n < customKeySecretMinLen || n > customKeySecretMaxLen {
		return errKeySecretInvalid
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x21 || secret[i] > 0x7e {
			return errKeySecretInvalid
		}
	}
	return nil
}

// keySecretHTTPStatus 把上面两个哨兵错误映射成状态码。其它错误返回 0，
// 由调用方按内部错误处理。
func keySecretHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errKeySecretInvalid):
		return http.StatusBadRequest
	case errors.Is(err, errKeySecretDuplicate):
		return http.StatusConflict
	}
	return 0
}

// keyPrefix 取列表展示用的前缀。前缀是有意可见的（管理台靠它认 key），
// 但仍要防越界：短于 12 字节时取全长。
func keyPrefix(raw string) string {
	const n = 12
	if len(raw) < n {
		return raw
	}
	return raw[:n]
}

func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	return s.createWithSecret(name, "")
}

// createWithSecret 在 secret 为空时生成随机 key（与原 create 行为一致），
// 否则把 secret 当作完整 key 明文使用。落盘的仍然只有 sha256 摘要。
//
// ID 一律独立于 key 明文随机生成。旧实现从随机 key 的前 8 字节派生 ID，在
// 随机路径上无害，但若对自定义 secret 沿用同一做法，公开字段 ID 就变成了
// 明文前 8 字节的可逆泄漏。
func (s *apiKeyStore) createWithSecret(name, secret string) (apiKeyRecord, string, error) {
	raw := secret
	custom := secret != ""
	if custom {
		if err := validateKeySecret(secret); err != nil {
			return apiKeyRecord{}, "", err
		}
	} else {
		b := make([]byte, 32)
		if _, e := rand.Read(b); e != nil {
			return apiKeyRecord{}, "", e
		}
		raw = "m365_" + hex.EncodeToString(b)
	}
	idBytes := make([]byte, 8)
	if _, e := rand.Read(idBytes); e != nil {
		return apiKeyRecord{}, "", e
	}
	hash := keyHash(raw)
	r := apiKeyRecord{ID: hex.EncodeToString(idBytes), Name: name, Prefix: keyPrefix(raw), Hash: hash, CreatedAt: time.Now()}
	s.mu.Lock()
	if custom {
		// 唯一性只按 hash 比对，等价于按明文比对，但过程中不需要任何明文。
		for i := range s.Keys {
			if s.Keys[i].Hash == hash {
				s.mu.Unlock()
				return apiKeyRecord{}, "", errKeySecretDuplicate
			}
		}
	}
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

// ---------------------------------------------------------------------------
// POST /api/accounts/refresh-all
//
// 放在 keys.go 而不是 server.go，只因为本次改动的写区把 server.go 限制为
// adminKeys 与路由注册行；语义上它属于账号刷新一族（refreshAccount）。
// ---------------------------------------------------------------------------

// refreshAllConcurrency 限制同时打向 Microsoft 令牌端点的刷新数。几百个账号
// 齐发会直接被限流，反而把本来能刷新的账号也拖成失败。
const refreshAllConcurrency = 4

// refreshAllErrorMaxLen 截断单条错误消息。上游 error_description 可能很长，
// 而汇总结果是给管理台看的，不需要完整堆栈。
const refreshAllErrorMaxLen = 200

type refreshAllEntry struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// refreshAllBudget 给整体刷新一个上限：基准 90s，账号多时按每账号 2s 放大，
// 最多 10 分钟。超时后返回已完成的部分结果，未完成的标记为 timeout。
func refreshAllBudget(accounts int) time.Duration {
	const base = 90 * time.Second
	const perAccount = 2 * time.Second
	const max = 10 * time.Minute
	budget := base
	if scaled := time.Duration(accounts) * perAccount; scaled > budget {
		budget = scaled
	}
	if budget > max {
		budget = max
	}
	return budget
}

// sanitizeRefreshError 把上游错误压成一行短消息，并抹掉可能被回显的刷新令牌。
// 调用方保证不把 access/refresh token 传进 message 之外的任何字段。
func sanitizeRefreshError(err error, refreshToken string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.TrimSpace(refreshToken) != "" {
		msg = strings.ReplaceAll(msg, refreshToken, "[redacted]")
	}
	msg = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(msg)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "refresh failed"
	}
	if len(msg) > refreshAllErrorMaxLen {
		// 按 rune 边界截断，避免切出半个 UTF-8 字符。
		cut := msg[:refreshAllErrorMaxLen]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		msg = cut
	}
	return msg
}

// refreshAllAccounts 并发刷新全部账号的令牌。单个账号失败或卡死都不影响其余
// 账号，也不会把 handler 挂死：ForceRefresh 不接受 context，因此每个账号跑在
// 独立 goroutine 里，handler 只等到总预算用尽为止。
func (s *Server) refreshAllAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.tokens == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", "账号存储不可用")
		return
	}
	list := s.tokens.List()
	entries := make([]refreshAllEntry, len(list))
	filled := make([]bool, len(list))
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(r.Context(), refreshAllBudget(len(list)))
	defer cancel()

	record := func(i int, ok bool, message string) {
		mu.Lock()
		defer mu.Unlock()
		if filled[i] {
			return
		}
		filled[i] = true
		entries[i].OK = ok
		entries[i].Error = message
	}

	for i := range list {
		entries[i] = refreshAllEntry{ID: list[i].ID, Email: list[i].Email}
	}

	sem := make(chan struct{}, refreshAllConcurrency)
	var wg sync.WaitGroup
	for i := range list {
		acc := list[i]
		index := i
		wg.Add(1)
		safeGoWithCleanup("accounts.refresh-all", func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// 预算已用尽，排队中的账号不再发起刷新。
				return
			}
			if _, err := s.tokens.ForceRefresh(acc.ID); err != nil {
				record(index, false, sanitizeRefreshError(err, acc.RefreshToken))
				return
			}
			record(index, true, "")
		}, func(any) {
			record(index, false, "internal error")
		})
	}

	done := make(chan struct{})
	safeGo("accounts.refresh-all-wait", func() {
		wg.Wait()
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
	}

	mu.Lock()
	out := make([]refreshAllEntry, 0, len(entries))
	refreshed, failed := 0, 0
	for i := range entries {
		entry := entries[i]
		if !filled[i] {
			entry.OK = false
			entry.Error = "timeout"
		}
		if entry.OK {
			refreshed++
		} else {
			failed++
		}
		out = append(out, entry)
	}
	mu.Unlock()

	jsonOut(w, map[string]any{
		"total":     len(out),
		"refreshed": refreshed,
		"failed":    failed,
		"results":   out,
	})
}