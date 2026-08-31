package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxWebSocketDialAttempts = 2

var errNoWebSocketProxyAvailable = errors.New("no untried websocket proxy available")

// Selection tiers. An exit is ranked by tier first and by score second, so a
// healthy slow exit always beats a fast one that is failing.
const (
	tierLive = iota
	tierSuspect
	tierEvicted
)

// probeSample is one round of the quality guard for one exit.
type probeSample struct {
	pass    bool
	latency time.Duration
}

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

	// Quality-guard state. The zero value is a live exit with no samples, which
	// keeps a freshly configured pool routable before the first probe lands.
	state                string
	consecutiveFailures  int
	consecutiveSuccesses int
	window               []probeSample
	score                float64
	medianLatency        time.Duration
	wsOK                 bool
	m365OK               bool
	backoff              time.Duration
	nextProbe            time.Time
}

// stateName normalises the empty zero value to live.
func (e *poolEntry) stateName() string {
	if e.state == "" {
		return stateLive
	}
	return e.state
}

// tier classifies an exit for selection. A cooldown set by mark() - that is, by a
// real user request failing - counts as evicted for ranking purposes: it is the
// signal that live traffic just broke on this exit.
func (e *poolEntry) tier(now time.Time) int {
	switch {
	case e.stateName() == stateEvicted:
		return tierEvicted
	case now.Before(e.cooldown):
		return tierEvicted
	case e.stateName() == stateSuspect:
		return tierSuspect
	default:
		return tierLive
	}
}

// id is a stable identifier derived from the host:port of the exit. It never
// contains credentials, so it is safe to hand to a browser and to accept back on
// a delete request.
func (e *poolEntry) id() string {
	return proxyEntryID(e.raw)
}

func proxyEntryID(raw string) string {
	key := strings.TrimSpace(raw)
	if u, err := url.Parse(key); err == nil && u.Host != "" {
		key = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// Pool holds the configured exits. There is deliberately no rotation cursor: an
// unused `next int` survived the removal of the round-robin selector and was still
// being maintained by adoptEntries, so it read as live state that some selector
// consulted. Nothing did. Selection is bestLocked, by tier then score.
type Pool struct {
	mu        sync.Mutex
	entries   []*poolEntry
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
	return p.bestLocked(time.Now(), func(*poolEntry) bool { return true })
}

// bestLocked implements the selection policy: always the best exit available.
//
// Entries are ranked by tier (live, then suspect, then evicted/cooling) and by
// score inside a tier, descending; ties keep configuration order so the choice is
// deterministic. There is deliberately no rotation. Round-robin used to hand an
// equal share of traffic to slow and half-dead exits, and it made a session bounce
// between a fast and a slow egress; the guard's score is what decides now.
//
// A suspect exit is therefore only reached when no live exit is eligible, which is
// the "fall back only when there is nothing healthy" rule. An evicted exit is last
// resort: with entries in the pool, returning nil would either hang the WebSocket
// waiter or silently leak the operator's own IP by dialing Microsoft directly, so
// an evicted exit is still preferable while the guard keeps re-probing it.
func (p *Pool) bestLocked(now time.Time, eligible func(*poolEntry) bool) *poolEntry {
	var best *poolEntry
	bestTier := tierEvicted
	for _, e := range p.entries {
		if eligible != nil && !eligible(e) {
			continue
		}
		tier := e.tier(now)
		if best == nil || tier < bestTier || (tier == bestTier && e.score > best.score) {
			best, bestTier = e, tier
		}
	}
	return best
}

// hasLiveLocked reports whether any exit is currently in the live tier. It is what
// makes "suspect exits are a fallback only" observable to the guard and to tests.
func (p *Pool) hasLiveLocked(now time.Time) bool {
	for _, e := range p.entries {
		if e.tier(now) == tierLive {
			return true
		}
	}
	return false
}

func (p *Pool) pickSticky(accountID string) *poolEntry {
	if p == nil {
		return nil
	}
	if accountID == "" {
		return p.pick()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky == nil {
		p.sticky = map[string]string{}
	}
	now := time.Now()
	if raw, ok := p.sticky[accountID]; ok {
		for _, entry := range p.entries {
			// Affinity is honoured only while the bound exit is healthy: an account
			// pinned to a failing exit would otherwise never fail over.
			if entry.raw == raw && entry.tier(now) == tierLive {
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

// ReleaseStickyExits drops the account -> exit bindings so the next request
// re-picks by score instead of staying pinned to whatever exit it landed on.
// Passing no account clears every binding. Callers use this to force a
// reassignment after the pool changed or after an exit started flapping;
// it only forgets the affinity, it never touches an in-flight connection.
func ReleaseStickyExits(accountIDs ...string) int {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky == nil {
		return 0
	}
	if len(accountIDs) == 0 {
		released := len(p.sticky)
		p.sticky = map[string]string{}
		return released
	}
	released := 0
	for _, id := range accountIDs {
		if id == "" {
			continue
		}
		if _, ok := p.sticky[id]; ok {
			delete(p.sticky, id)
			released++
		}
	}
	return released
}
func accountAffinity(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(accountAffinityKey{}).(string)
	return value
}

// gatewayCancelled reports whether a failed operation was cut short by the context
// the gateway handed us rather than by the exit itself.
//
// A dial the gateway abandoned - the client hung up, the request budget expired, the
// process is shutting down - says nothing about the exit's health. It was recorded as
// a real exit failure anyway: e.failures went up, the exit went into a cooldown that
// ranks it as evicted, and markFor dropped the account's sticky binding on top. So a
// user pressing stop mid-stream demoted a perfectly healthy egress. This is the same
// discrimination probeExit's two callers already make (guardSweep in guard.go and
// CheckSelected in health.go).
//
// A per-attempt deadline we derived ourselves is deliberately not covered: in that
// case the caller's context stays clean and a dial that overran the budget really was
// the exit being too slow.
func gatewayCancelled(ctx context.Context, err error) bool {
	return err != nil && ctx != nil && ctx.Err() != nil
}

// markFor folds the outcome of one real request into the exit's health. ctx is the
// caller's context, not a per-attempt one: it is what distinguishes an exit that
// failed from an operation the gateway gave up on.
func (p *Pool) markFor(ctx context.Context, accountID, raw string, err error) {
	if gatewayCancelled(ctx, err) {
		log.Printf("proxy request abandoned proxy=%s reason=%v (gateway or client cancelled, not counted as an exit failure)",
			redactProxy(raw), ctx.Err())
		return
	}
	if err != nil {
		p.unstick(accountID, raw)
	}
	p.mark(raw, err)
}

// failureDecayInterval is how much quiet time retires one accumulated traffic
// failure. It is deliberately longer than the longest cooldown mark() can compute
// (2 minutes), so an exit has to be genuinely quiet - not merely out of cooldown -
// before its ratchet steps down.
const failureDecayInterval = 5 * time.Minute

// decayFailuresLocked steps e.failures back down for elapsed quiet time.
//
// e.failures was a one-way ratchet. It only ever grew in mark(err != nil) and was
// only ever cleared by mark(err == nil) - a *successful real request*. But the
// cooldown it computes ranks the exit as evicted (see tier), which is last in
// selection order, so an exit that failed enough times stopped being picked and
// could never produce the success that was its only way back. Worse, the count is
// what sizes the next cooldown: an exit sitting at failures=60 got the 2-minute
// ceiling from a single fresh failure forever after.
//
// The cooldown deadline doubles as the decay anchor, so no extra field is needed:
// every failure rewrites it, and a success clears both together.
func (e *poolEntry) decayFailuresLocked(now time.Time) {
	if e.failures <= 0 || e.cooldown.IsZero() || now.Before(e.cooldown) {
		return
	}
	steps := int(now.Sub(e.cooldown) / failureDecayInterval)
	if steps <= 0 {
		return
	}
	if steps >= e.failures {
		e.failures = 0
		e.cooldown = time.Time{}
		return
	}
	e.failures -= steps
	// Advance the anchor by exactly what was consumed, so the same quiet time is
	// not spent twice on the next call.
	e.cooldown = e.cooldown.Add(time.Duration(steps) * failureDecayInterval)
}

// mark records the outcome of real user traffic. It keeps the backoff behaviour it
// always had; eviction is the quality guard's job (see guard.go), because a probe
// result is a controlled measurement while a request failure can also mean the
// upstream, not the exit, is broken.
func (p *Pool) mark(raw string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, e := range p.entries {
		if e.raw == raw {
			if err == nil {
				e.failures = 0
				e.cooldown = time.Time{}
			} else {
				// Retire whatever the exit has already sat out before adding to the
				// count, so the cooldown reflects how it is behaving now rather than
				// every failure it ever had.
				e.decayFailuresLocked(now)
				e.failures++
				d := time.Duration(e.failures) * 2 * time.Second
				if d > 2*time.Minute {
					d = 2 * time.Minute
				}
				e.cooldown = now.Add(d)
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
		// Hold on to the caller's context before deriving a per-attempt one below.
		// Only the caller's context can say that a failed dial was the gateway or the
		// client giving up rather than the exit breaking; the derived deadline is our
		// own budget for this exit and overrunning it is a genuine failure.
		caller := ctx
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
			p.markFor(caller, accountID, e.raw, err)
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
		// pickStickyWebSocketLocked owns the binding. Rebinding here would undo its
		// distinction between a failover and a temporary capacity fallback.
		entry := p.pickStickyWebSocketLocked(accountID, excluded)
		if entry != nil {
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

// pickStickyWebSocketLocked selects the WebSocket exit and, for an account, records
// the affinity it settled on. The per-exit concurrency budget (maxWebSockets) is the
// only reason the best exit is skipped: it is a hard capacity limit, so the next best
// score takes over until a slot frees up.
//
// The binding is written here rather than by the caller, because the reasons the bound
// exit gets skipped need opposite treatment and only this function can tell them
// apart:
//
//	gone from the pool, or no longer live   the account must fail over: forget it
//	live but full, or already tried on this dial   transient: keep it
//
// Both used to fall into the same delete, and the caller then bound the account to
// whatever came back. So a burst that filled the bound exit's slots for a moment cost
// the account its exit affinity permanently - nothing ever re-picks the original exit
// for that account, so the "sticky" exit was whichever one happened to have a free
// slot during the spike. A single failed attempt earlier in the same dial did it too,
// because an excluded entry looked identical to an unhealthy one.
func (p *Pool) pickStickyWebSocketLocked(accountID string, excluded map[*poolEntry]struct{}) *poolEntry {
	if len(p.entries) == 0 {
		return nil
	}
	limit := p.wsLimit
	if limit < 1 {
		limit = defaultOutboundMaxWebSockets
	}
	now := time.Now()
	eligible := func(entry *poolEntry) bool {
		if _, skip := excluded[entry]; skip {
			return false
		}
		return entry.activeWebSockets < limit
	}
	if accountID == "" {
		return p.bestLocked(now, eligible)
	}
	if p.sticky == nil {
		p.sticky = map[string]string{}
	}
	if raw, ok := p.sticky[accountID]; ok {
		switch bound := p.entryLocked(raw); {
		case bound == nil || bound.tier(now) != tierLive:
			// The only failover the affinity is allowed to break for.
			delete(p.sticky, accountID)
		case eligible(bound):
			return bound
		default:
			// Live, but at capacity or excluded from this dial. Serve this one dial
			// from the best available exit and leave the binding alone, so the account
			// returns to its own exit as soon as a slot frees up.
			return p.bestLocked(now, eligible)
		}
	}
	entry := p.bestLocked(now, eligible)
	if entry != nil {
		p.sticky[accountID] = entry.raw
	}
	return entry
}

// entryLocked resolves a raw exit URL to its entry, or nil when the exit is no
// longer configured.
func (p *Pool) entryLocked(raw string) *poolEntry {
	for _, entry := range p.entries {
		if entry.raw == raw {
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

// List reports pool status for the admin API and the dashboard.
//
// The url field is redacted here, at the source: it used to carry the proxy
// password verbatim into the browser (visible in the devtools network tab and in
// the settings response). Deleting by url keeps working because RemoveProxy accepts
// the redacted form as well as the raw one (see sameProxyURL); the id field is a
// credential-free alternative for the same purpose.
//
// Every pre-existing key is preserved with its original name and type. The guard
// fields are additions only.
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		lastCheckedAt := ""
		if !e.lastCheck.IsZero() {
			lastCheckedAt = e.lastCheck.Format(time.RFC3339)
		}
		out = append(out, map[string]any{
			"url": redactProxyDisplay(e.raw), "failures": e.failures, "cooldownUntil": e.cooldown,
			"lastCheck": e.lastCheck, "latencyMs": e.latency.Milliseconds(), "lastError": e.lastError,
			"health": e.health, "activeWebSockets": e.activeWebSockets, "maxWebSockets": p.wsLimit,
			"id": e.id(), "state": e.stateName(), "score": roundScore(e.score),
			"medianLatencyMs": e.medianLatency.Milliseconds(), "wsOk": e.wsOK,
			// m365Ok 不再参与 pass 判定（见 health.go 的探测目标表），但它仍然是一个
			// 有价值的信号。不导出的话，这个字段就会变成「探了、存了、没人读」，
			// 运维也看不到某个出口够不着 m365.cloud.microsoft。
			"m365Ok":        e.m365OK,
			"lastCheckedAt": lastCheckedAt, "consecutiveFailures": e.consecutiveFailures,
			// tier/tierName 是选路时真正用的分档，之前只存在于进程内部，
			// 面板拿不到，只能自己按 state 猜 —— 于是 UI 的分组和实际选路口径
			// 不一致。冷却中的出口 state 仍是 live，但 tier 已是 evicted。
			"tier": e.tier(now), "tierName": tierName(e.tier(now)),
			"cooldownRemainingMs": cooldownRemainingMs(e.cooldown, now),
			"refused":             isRefusalError(e.lastError),
		})
	}
	return out
}

// tierName gives the selection tier a stable name for the admin API. The
// dashboard groups exits by this instead of re-deriving it from state: an exit in
// cooldown still reports state=live, so a UI that groups by state shows a broken
// exit at the top of the healthy list.
func tierName(tier int) string {
	switch tier {
	case tierLive:
		return "live"
	case tierSuspect:
		return "suspect"
	default:
		return "evicted"
	}
}

// cooldownRemainingMs reports how long an exit stays isolated. 0 means it is not
// in cooldown.
func cooldownRemainingMs(cooldown, now time.Time) int64 {
	if cooldown.IsZero() || !now.Before(cooldown) {
		return 0
	}
	return now.Sub(cooldown).Milliseconds() * -1
}

// isRefusalError separates "refused outright" from "timed out". They look the
// same in a flat list yet mean opposite things: a refusal answers in a few
// milliseconds and will keep refusing, while a timeout may just be a slow path.
func isRefusalError(lastError string) bool {
	if lastError == "" {
		return false
	}
	e := strings.ToLower(lastError)
	for _, marker := range []string{"refused", "reset", "forbidden", "407", "403", "denied", "unauthorized"} {
		if strings.Contains(e, marker) {
			return true
		}
	}
	return false
}

func roundScore(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
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
// List already redacts; this stays as the explicit spelling for callers that want
// the guarantee at the call site.
func (p *Pool) ListRedacted() []map[string]any {
	items := p.List()
	for _, item := range items {
		if raw, ok := item["url"].(string); ok {
			item["url"] = redactProxyDisplay(raw)
		}
	}
	return items
}

// RawURLForID resolves the credential-free id from List back to a dialable URL.
// It lets the admin UI delete an exit without ever holding its password.
func (p *Pool) RawURLForID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.id() == id {
			return e.raw
		}
	}
	return ""
}

// ProxyRawURLForID resolves the credential-free id from the status view back to a
// dialable URL on the active pool. It exists so the admin API can delete an exit by
// id, without the browser ever having to hold the proxy password.
func ProxyRawURLForID(id string) string {
	p := CurrentPool()
	if p == nil {
		return ""
	}
	return p.RawURLForID(id)
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
	t.pool.markFor(r.Context(), accountID, entry.raw, err)
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
	t.pool.markFor(r.Context(), accountID, next.raw, err2)
	if err2 == nil {
		t.pool.bindSticky(accountID, next.raw)
		return resp2, nil
	}
	return resp, err
}

// pickDifferent returns the best exit other than previous, so a retry moves to the
// runner-up by score instead of to the next slot in a rotation.
func (p *Pool) pickDifferent(previous *poolEntry) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) < 2 {
		return nil
	}
	return p.bestLocked(time.Now(), func(entry *poolEntry) bool { return entry != previous })
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

// applyProbe folds one probe round into the quality state of an exit and advances
// the state machine. It is the only place state, score and the sliding window are
// written, so the transitions live in exactly one function.
func (p *Pool) applyProbe(e *poolEntry, result probeResult, now time.Time, timing guardTiming) {
	if e == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	e.lastCheck = now
	e.latency = result.l2Latency
	e.health = result.health
	e.wsOK = result.wsOK
	e.m365OK = result.m365OK
	e.lastError = ""
	if result.err != nil {
		e.lastError = result.err.Error()
	}

	e.window = append(e.window, probeSample{pass: result.pass, latency: result.l2Latency})
	if len(e.window) > timing.window {
		e.window = e.window[len(e.window)-timing.window:]
	}

	was := e.stateName()
	reason := ""
	if result.pass {
		e.consecutiveFailures = 0
		e.consecutiveSuccesses++
		if e.stateName() != stateEvicted || e.consecutiveSuccesses >= timing.restoreAfter {
			e.state = stateLive
			e.backoff = 0
			// A probe that passes is the authority on this exit's health, so it also
			// clears the traffic-side ratchet mark() maintains. Without this,
			// e.failures and the cooldown it computes were only ever reset by a
			// successful real request - which an exit ranked last by tier stops
			// receiving, so the recovery condition could not be met (see mark).
			e.failures = 0
			e.cooldown = time.Time{}
			reason = "probe passed"
		} else {
			reason = "probe passed but restore threshold not met"
		}
	} else {
		e.consecutiveSuccesses = 0
		e.consecutiveFailures++
		switch {
		case e.consecutiveFailures >= timing.evictAfter:
			e.state = stateEvicted
			if e.backoff <= 0 {
				e.backoff = timing.base
			} else {
				e.backoff *= 2
			}
			if e.backoff > timing.maxBackoff {
				e.backoff = timing.maxBackoff
			}
			reason = "probe failed, eviction threshold reached"
		case e.stateName() == stateEvicted:
			// A FAILURE must never be a way out of eviction. This branch used to fall
			// into the default below and write stateSuspect unconditionally, so an
			// evicted exit that had collected one restoring pass (consecutiveFailures
			// reset to 0, still evicted because restoreAfter is 2) was *promoted* to
			// suspect by its very next failing probe - and suspect is a routable tier.
			// Failing your way back into rotation is the exact opposite of what the
			// state machine is for. Eviction is left only by consecutive passes, so
			// the entry stays evicted and keeps its backoff ladder.
			reason = "probe failed while evicted, staying evicted"
		default:
			e.state = stateSuspect
			e.backoff = 0
			reason = "probe failed below eviction threshold"
		}
	}

	passes, attempts, median := windowStats(e.window)
	e.medianLatency = median
	e.score = guardScore(passes, attempts, median, e.wsOK)
	e.nextProbe = now.Add(e.probeIntervalLocked(timing))

	// Log every transition. The whole live->suspect->evicted->live cycle used to
	// happen silently inside this function: the only per-exit line was logProbe's
	// pass/fail, so an exit leaving rotation - or coming back - left no trace, and
	// reconstructing why a pool went dark meant guessing from probe results.
	if now := e.stateName(); now != was {
		log.Printf("proxy exit state %s -> %s proxy=%s reason=%q failures=%d successes=%d evictAfter=%d restoreAfter=%d backoff=%s score=%.3f",
			was, now, redactProxy(e.raw), reason, e.consecutiveFailures, e.consecutiveSuccesses,
			timing.evictAfter, timing.restoreAfter, e.backoff, e.score)
	}
}

// probeIntervalLocked is the per-state probe cadence: live exits every base
// interval, suspect exits twice as often to resolve the ambiguity quickly, evicted
// exits on an exponential backoff.
func (e *poolEntry) probeIntervalLocked(timing guardTiming) time.Duration {
	switch e.stateName() {
	case stateEvicted:
		if e.backoff > 0 {
			return e.backoff
		}
		return timing.base
	case stateSuspect:
		return timing.suspect
	default:
		return timing.base
	}
}

// windowStats reduces the sliding window to the numbers the score needs. Only a
// passing round contributes a latency sample: a failing exit often rejects in a
// millisecond, and counting that would make a dead exit look fast.
func windowStats(window []probeSample) (passes, attempts int, median time.Duration) {
	latencies := make([]time.Duration, 0, len(window))
	for _, sample := range window {
		attempts++
		if !sample.pass {
			continue
		}
		passes++
		if sample.latency > 0 {
			latencies = append(latencies, sample.latency)
		}
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		median = latencies[len(latencies)/2]
	}
	return passes, attempts, median
}

// dueForProbe returns the exits whose next probe is due. A never-probed exit is
// always due, so the first sweep after startup covers the whole pool.
//
// Being a routing fallback and being probed are separate concerns: a suspect exit
// only carries traffic when nothing is live, yet it is probed on the faster suspect
// cadence precisely so it gets re-qualified or evicted quickly.
func (p *Pool) dueForProbe(now time.Time, timing guardTiming) []*poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	due := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		// The patrol tick is the one clock that runs regardless of traffic, so it is
		// where the traffic-failure ratchet is retired. Doing it only in mark() would
		// leave an exit that stopped being selected stuck at its peak count forever,
		// which is the whole defect decayFailuresLocked exists to fix.
		e.decayFailuresLocked(now)
		if !e.nextProbe.IsZero() && e.nextProbe.After(now) {
			continue
		}
		due = append(due, e)
	}
	return due
}
