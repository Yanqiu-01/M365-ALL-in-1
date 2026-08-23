package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxWebSocketDialAttempts = 2

var errNoWebSocketProxyAvailable = errors.New("no untried websocket proxy available")

type poolEntry struct {
	raw              string
	clients          *Clients
	failures         int
	cooldown         time.Time
	lastCheck        time.Time
	latency          time.Duration
	lastError        string
	health           string
	activeWebSockets int
}
type Pool struct {
	mu        sync.Mutex
	entries   []*poolEntry
	next      int
	wsLimit   int
	wsChanged chan struct{}
	sticky    map[string]string
}

func NewPool(raw []string) (*Pool, error) {
	p := &Pool{wsLimit: outboundWebSocketLimit(), wsChanged: make(chan struct{}), sticky: map[string]string{}}
	seen := map[string]bool{}
	for _, v := range raw {
		if v == "" || seen[v] {
			continue
		}
		c, err := New(v)
		if err != nil {
			// Never echo the raw URL: it may carry userinfo and this error is
			// surfaced verbatim by the admin API and the settings validator.
			return nil, fmt.Errorf("proxy %q: %w", redactProxy(v), err)
		}
		seen[v] = true
		p.entries = append(p.entries, &poolEntry{raw: v, clients: c})
	}
	return p, nil
}
func (p *Pool) pick() *poolEntry {
	p.mu.Lock()
	e := p.pickLocked()
	p.mu.Unlock()
	return e
}

func (p *Pool) pickLocked() *poolEntry {
	if len(p.entries) == 0 {
		return nil
	}
	now := time.Now()
	for i := 0; i < len(p.entries); i++ {
		e := p.entries[(p.next+i)%len(p.entries)]
		if now.Before(e.cooldown) {
			continue
		}
		p.next = (p.next + i + 1) % len(p.entries)
		return e
	}
	e := p.entries[p.next%len(p.entries)]
	p.next = (p.next + 1) % len(p.entries)
	return e
}
func (p *Pool) pickSticky(accountID string) *poolEntry {
	if accountID == "" || p == nil {
		return p.pick()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky == nil {
		p.sticky = map[string]string{}
	}
	if raw, ok := p.sticky[accountID]; ok {
		now := time.Now()
		for _, entry := range p.entries {
			if entry.raw == raw && !now.Before(entry.cooldown) {
				return entry
			}
		}
		delete(p.sticky, accountID)
	}
	entry := p.pickLocked()
	if entry != nil {
		p.sticky[accountID] = entry.raw
	}
	return entry
}

func (p *Pool) unstick(accountID, raw string) {
	if accountID == "" || p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky[accountID] == raw {
		delete(p.sticky, accountID)
	}
}

func accountAffinity(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(accountAffinityKey{}).(string)
	return value
}

func (p *Pool) markFor(accountID, raw string, err error) {
	if err != nil {
		p.unstick(accountID, raw)
	}
	p.mark(raw, err)
}

func (p *Pool) mark(raw string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.raw == raw {
			if err == nil {
				e.failures = 0
				e.cooldown = time.Time{}
			} else {
				e.failures++
				d := time.Duration(e.failures) * 2 * time.Second
				if d > 2*time.Minute {
					d = 2 * time.Minute
				}
				e.cooldown = time.Now().Add(d)
			}
			return
		}
	}
}
func (p *Pool) HTTPClient() *http.Client {
	p.mu.Lock()
	if len(p.entries) == 0 {
		p.mu.Unlock()
		return directClients().HTTP
	}
	timeout := p.entries[0].clients.HTTP.Timeout
	p.mu.Unlock()
	return &http.Client{Transport: &poolRoundTripper{pool: p}, Timeout: timeout}
}
func (p *Pool) WebSocketDialer() *websocket.Dialer {
	base := directClients().WebSocket
	baseDialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	base.Proxy = nil
	base.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := ctx.Deadline(); !ok {
			timeout := base.HandshakeTimeout
			if timeout <= 0 {
				timeout = 20 * time.Second
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		// Retrying here is safe: this hook only covers the raw TCP / proxy CONNECT
		// phase. Gorilla performs the WebSocket HTTP handshake after this returns,
		// so an established connection is never replayed and no chat payload can be
		// written twice. A failed exit is excluded for the remainder of this dial.
		tried := make(map[*poolEntry]struct{})
		var lastErr error
		attempts := 0
		accountID := accountAffinity(ctx)
		for {
			e, release, err := p.acquireWebSocket(ctx, accountID, tried)
			if err != nil {
				if errors.Is(err, errNoWebSocketProxyAvailable) && lastErr != nil {
					return nil, lastErr
				}
				return nil, err
			}
			if e == nil {
				return baseDialer.DialContext(ctx, network, address)
			}
			dial := e.clients.WebSocket.NetDialContext
			if dial == nil {
				dial = baseDialer.DialContext
			}
			conn, err := dial(ctx, network, address)
			p.markFor(accountID, e.raw, err)
			if err == nil {
				return &pooledConn{Conn: conn, release: release}, nil
			}

			release()
			attempts++
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
			if attempts >= maxWebSocketDialAttempts {
				return nil, lastErr
			}
			tried[e] = struct{}{}
		}
	}
	return base
}

func (p *Pool) acquireWebSocket(ctx context.Context, accountID string, excluded map[*poolEntry]struct{}) (*poolEntry, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		if len(p.entries) == 0 {
			p.mu.Unlock()
			return nil, nil, nil
		}
		entry := p.pickStickyWebSocketLocked(accountID, excluded)
		if entry != nil {
			if accountID != "" {
				p.sticky[accountID] = entry.raw
			}
			entry.activeWebSockets++
			p.mu.Unlock()
			var once sync.Once
			return entry, func() {
				once.Do(func() {
					p.mu.Lock()
					if entry.activeWebSockets > 0 {
						entry.activeWebSockets--
					}
					p.signalWebSocketWaitersLocked()
					p.mu.Unlock()
				})
			}, nil
		}
		if !p.hasUntriedWebSocketLocked(excluded) {
			p.mu.Unlock()
			return nil, nil, errNoWebSocketProxyAvailable
		}
		changed := p.wsChanged
		if changed == nil {
			changed = make(chan struct{})
			p.wsChanged = changed
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

func (p *Pool) pickStickyWebSocketLocked(accountID string, excluded map[*poolEntry]struct{}) *poolEntry {
	if len(p.entries) == 0 {
		return nil
	}
	limit := p.wsLimit
	if limit < 1 {
		limit = defaultOutboundMaxWebSockets
	}
	now := time.Now()
	if accountID != "" {
		if p.sticky == nil {
			p.sticky = map[string]string{}
		}
		if raw, ok := p.sticky[accountID]; ok {
			for _, entry := range p.entries {
				if entry.raw != raw {
					continue
				}
				if _, skip := excluded[entry]; !skip && entry.activeWebSockets < limit && !now.Before(entry.cooldown) {
					return entry
				}
			}
			delete(p.sticky, accountID)
		}
	}
	start := p.next % len(p.entries)
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < len(p.entries); i++ {
			index := (start + i) % len(p.entries)
			entry := p.entries[index]
			if _, skip := excluded[entry]; skip || entry.activeWebSockets >= limit || (pass == 0 && now.Before(entry.cooldown)) {
				continue
			}
			p.next = (index + 1) % len(p.entries)
			return entry
		}
	}
	return nil
}

func (p *Pool) hasUntriedWebSocketLocked(excluded map[*poolEntry]struct{}) bool {
	for _, entry := range p.entries {
		if _, skip := excluded[entry]; !skip {
			return true
		}
	}
	return false
}

func (p *Pool) signalWebSocketWaitersLocked() {
	if p.wsChanged == nil {
		p.wsChanged = make(chan struct{})
		return
	}
	close(p.wsChanged)
	p.wsChanged = make(chan struct{})
}

type pooledConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *pooledConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, map[string]any{"url": e.raw, "failures": e.failures, "cooldownUntil": e.cooldown, "lastCheck": e.lastCheck, "latencyMs": e.latency.Milliseconds(), "lastError": e.lastError, "health": e.health, "activeWebSockets": e.activeWebSockets, "maxWebSockets": p.wsLimit})
	}
	return out
}
// rawURLs returns the exits verbatim, credentials included. Reserved for the
// settings persistence path and for internal add/remove bookkeeping, both of
// which need a URL that can actually be dialed again.
func (p *Pool) rawURLs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.raw)
	}
	return out
}

// ListRedacted is List with the password of every exit masked. Use it for
// anything that leaves the process: an API response, a log line, a template.
func (p *Pool) ListRedacted() []map[string]any {
	items := p.List()
	for _, item := range items {
		if raw, ok := item["url"].(string); ok {
			item["url"] = redactProxyDisplay(raw)
		}
	}
	return items
}

func (p *Pool) Remove(raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.raw == raw {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			for account, bound := range p.sticky {
				if bound == raw {
					delete(p.sticky, account)
				}
			}
			return
		}
	}
}

type poolRoundTripper struct {
	pool *Pool
}

func (t *poolRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	accountID := accountAffinity(r.Context())
	entry := t.pool.pickSticky(accountID)
	if entry == nil {
		return directClients().HTTP.Transport.RoundTrip(r)
	}
	resp, err := entry.clients.HTTP.Transport.RoundTrip(r)
	t.pool.markFor(accountID, entry.raw, err)
	if err == nil || r.Context().Err() != nil || !safeRetryMethod(r) {
		return resp, err
	}
	// Retry exactly once, and only for an idempotent request whose body can be
	// recreated. This avoids duplicating upstream mutations or token exchanges.
	next := t.pool.pickDifferent(entry)
	if next == nil {
		return resp, err
	}
	retry := r.Clone(r.Context())
	if r.Body != nil {
		if r.GetBody == nil {
			return resp, err
		}
		body, bodyErr := r.GetBody()
		if bodyErr != nil {
			return resp, err
		}
		retry.Body = body
	}
	resp2, err2 := next.clients.HTTP.Transport.RoundTrip(retry)
	t.pool.markFor(accountID, next.raw, err2)
	if err2 == nil {
		t.pool.bindSticky(accountID, next.raw)
		return resp2, nil
	}
	return resp, err
}

func (p *Pool) pickDifferent(previous *poolEntry) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) < 2 {
		return nil
	}
	now := time.Now()
	for i := 0; i < len(p.entries); i++ {
		index := (p.next + i) % len(p.entries)
		entry := p.entries[index]
		if entry == previous || now.Before(entry.cooldown) {
			continue
		}
		p.next = (index + 1) % len(p.entries)
		return entry
	}
	return nil
}

func (p *Pool) bindSticky(accountID, raw string) {
	if accountID == "" || p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky == nil {
		p.sticky = map[string]string{}
	}
	p.sticky[accountID] = raw
}

func safeRetryMethod(r *http.Request) bool {
	if r.Header.Get("Idempotency-Key") != "" {
		return r.Body == nil || r.GetBody != nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.GetBody != nil
	default:
		return false
	}
}
