package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

var errConflictingKeyState = errors.New("revoked and enabled disagree; send only one of them")

// keyConcurrency 按 API key 限制在途请求数。结构与语义刻意对齐
// account_concurrency.go：同一把 mutex 保护计数表，等待方阻塞在一个每次
// 释放都会重建的 changed channel 上，因此不引入第二套锁模型。
//
// 与 accountConcurrency 的唯一差别是上限来源：账号并发用全局 limit，
// 而每把 key 的上限由 apiKeyRecord.MaxConcurrent 逐次传入（0 表示不限，
// 由全局账号并发继续兜底）。
type keyConcurrency struct {
	mu       sync.Mutex
	inflight map[string]int
	changed  chan struct{}
}

func newKeyConcurrency() *keyConcurrency {
	return &keyConcurrency{inflight: map[string]int{}, changed: make(chan struct{})}
}

// Acquire 在 keyID 的在途数低于 limit 时占用一个槽位，否则阻塞到有槽位或
// ctx 结束。limit <= 0 表示该 key 不限并发，直接放行。
func (c *keyConcurrency) Acquire(ctx context.Context, keyID string, limit int) (func(), error) {
	if c == nil || keyID == "" || limit <= 0 {
		return func() {}, nil
	}
	for {
		c.mu.Lock()
		if c.inflight[keyID] < limit {
			c.inflight[keyID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					if c.inflight[keyID] <= 1 {
						delete(c.inflight, keyID)
					} else {
						c.inflight[keyID]--
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// TryAcquire 是 Acquire 的非阻塞形式：槽位不足时立即返回 false，供 HTTP
// 入口把超限翻译成 429 而不是让客户端悬挂。
func (c *keyConcurrency) TryAcquire(keyID string, limit int) (func(), bool) {
	if c == nil || keyID == "" || limit <= 0 {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[keyID] >= limit {
		return nil, false
	}
	c.inflight[keyID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if c.inflight[keyID] <= 1 {
				delete(c.inflight, keyID)
			} else {
				c.inflight[keyID]--
			}
			close(c.changed)
			c.changed = make(chan struct{})
			c.mu.Unlock()
		})
	}, true
}

func (c *keyConcurrency) Inflight(keyID string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[keyID]
}

// keyRateLimiter 是每把 key 的滑动分钟窗口计数器，服务 rpmLimit。
type keyRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newKeyRateLimiter() *keyRateLimiter {
	return &keyRateLimiter{hits: map[string][]time.Time{}}
}

// Allow 记录一次请求并判断是否仍在 limit 之内。limit <= 0 表示不限。
func (l *keyRateLimiter) Allow(keyID string, limit int) bool {
	if l == nil || keyID == "" || limit <= 0 {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[keyID][:0]
	for _, at := range l.hits[keyID] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= limit {
		l.hits[keyID] = kept
		return false
	}
	l.hits[keyID] = append(kept, now)
	return true
}

// rawAPIKey 取出请求携带的完整明文 key。extractAPIKey 会为日志截断成前
// 8 位，因此策略判定不能复用它。
func rawAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	value := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}

// modelAllowed 判定 key 的白名单是否放行某个 model id。空白名单表示不限制。
// 比较对 model id 大小写不敏感，与 /v1/models 暴露的 id 一致。
func (r apiKeyRecord) modelAllowed(model string) bool {
	if len(r.AllowedModelIDs) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		// 未指定模型时走服务端默认模型，白名单非空即视为越权，
		// 否则客户端只要省略 model 就能绕过限制。
		return false
	}
	for _, allowed := range r.AllowedModelIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), model) {
			return true
		}
	}
	return false
}

// keyPolicy 是一次请求解析出的 key 侧策略。Record 为 zero 且 Known 为 false
// 时表示请求没有匹配到已登记的 key（例如直传 JWT），此时不施加 key 级限制。
type keyPolicy struct {
	Record apiKeyRecord
	Known  bool
}

func (s *Server) keyPolicyFor(r *http.Request) keyPolicy {
	if s == nil || s.apiKeys == nil {
		return keyPolicy{}
	}
	raw := rawAPIKey(r)
	if raw == "" {
		return keyPolicy{}
	}
	record, ok := s.apiKeys.lookup(raw)
	if !ok {
		return keyPolicy{}
	}
	return keyPolicy{Record: record, Known: true}
}

// enforceKeyPolicy 在 OpenAI 兼容入口强制执行 key 级策略：模型白名单、
// 每分钟请求数与并发上限。返回的 release 必须在请求结束时调用；ok 为 false
// 时响应已写出，调用方直接返回即可。
//
// writeErr 让不同协议（OpenAI / Responses / Anthropic）沿用各自的错误形状。
func (s *Server) enforceKeyPolicy(w http.ResponseWriter, r *http.Request, model string, writeErr func(http.ResponseWriter, int, string, string)) (func(), bool) {
	policy := s.keyPolicyFor(r)
	if !policy.Known {
		return func() {}, true
	}
	record := policy.Record
	if !record.modelAllowed(model) {
		writeErr(w, http.StatusForbidden, "model_not_allowed",
			"model "+strings.TrimSpace(model)+" is not permitted for this API key")
		return nil, false
	}
	if !s.keyRates.Allow(record.ID, record.RPMLimit) {
		writeErr(w, http.StatusTooManyRequests, "rate_limit_exceeded",
			"API key exceeded its requests-per-minute limit")
		return nil, false
	}
	release, ok := s.keyLimits.TryAcquire(record.ID, record.MaxConcurrent)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "concurrency_limit_exceeded",
			"API key exceeded its concurrent request limit")
		return nil, false
	}
	return release, true
}
