package outbound

// Quality-guard tests. They never touch the network: every exit is a local fake
// CONNECT proxy whose verdict is switchable at runtime (pass / fail / flaky), which
// is enough to drive the whole state machine and the scoring order.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExit is a minimal HTTP CONNECT proxy with a switchable verdict.
//
//	pass   answer 200, so a dial through this exit succeeds
//	fail   answer 502, so the dial fails the way a dead exit does
//	authed answer 200 only when Proxy-Authorization is present, 407 otherwise
type fakeExit struct {
	listener net.Listener
	verdict  atomic.Value // string
	connects atomic.Int64
	rejected atomic.Int64
	authed   atomic.Int64
}

const (
	verdictPass   = "pass"
	verdictFail   = "fail"
	verdictAuthed = "authed"
)

func newFakeExit(t *testing.T, verdict string) *fakeExit {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := &fakeExit{listener: listener}
	exit.verdict.Store(verdict)
	go exit.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return exit
}

func (e *fakeExit) serve() {
	for {
		conn, err := e.listener.Accept()
		if err != nil {
			return
		}
		go e.handle(conn)
	}
}

func (e *fakeExit) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	hasAuth := request.Header.Get("Proxy-Authorization") != ""
	switch e.current() {
	case verdictAuthed:
		if !hasAuth {
			e.rejected.Add(1)
			_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			return
		}
		e.authed.Add(1)
		e.connects.Add(1)
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	case verdictPass:
		e.connects.Add(1)
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	default:
		e.rejected.Add(1)
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	}
}

func (e *fakeExit) current() string {
	v, _ := e.verdict.Load().(string)
	return v
}

func (e *fakeExit) set(verdict string) { e.verdict.Store(verdict) }

func (e *fakeExit) url() string { return "http://" + e.listener.Addr().String() }

func (e *fakeExit) urlWithUserinfo() string {
	return "http://m365exit:s3cr3t-pa55word@" + e.listener.Addr().String()
}

// connectOnlyProbe is the probe used in these tests. It runs the real production
// dialer of the exit (CONNECT, Proxy-Authorization, SOCKS5 handshake included) and
// treats the tunnel verdict as the round verdict. The Microsoft TLS/WebSocket legs
// of probeExit cannot run against a local listener - certificate verification is
// strict by design and must stay that way - so they are cut off here while the exit
// itself remains a real proxy on a real socket.
func connectOnlyProbe(ctx context.Context, dial dialFunc, timeout time.Duration) probeResult {
	if dial == nil {
		return probeResult{health: "unreachable", err: fmt.Errorf("no dialer")}
	}
	started := time.Now()
	conn, err := dial(ctx, "tcp", net.JoinHostPort(probeAuthHost, "443"))
	latency := time.Since(started)
	if err != nil {
		return probeResult{health: "unreachable", l2Latency: latency, err: err}
	}
	_ = conn.Close()
	return probeResult{
		l2Code: http.StatusOK, l2Latency: latency, substrateOK: true, m365OK: true,
		wsOK: true, wsCode: http.StatusUnauthorized, pass: true, health: "reachable",
	}
}

func useConnectOnlyProbe(t *testing.T) {
	t.Helper()
	previous := probeRound
	probeRound = connectOnlyProbe
	t.Cleanup(func() { probeRound = previous })
}

func fastGuardTiming() guardTiming {
	return guardTiming{
		base:         10 * time.Millisecond,
		suspect:      5 * time.Millisecond,
		maxBackoff:   80 * time.Millisecond,
		evictAfter:   guardEvictAfter,
		restoreAfter: guardRestoreAfter,
		window:       guardWindowSize,
	}
}

// sweepNow forces every exit to be due and runs one patrol sweep, so a test does
// not have to sleep out the probe cadence.
func sweepNow(t *testing.T, p *Pool, timing guardTiming) {
	t.Helper()
	p.mu.Lock()
	for _, e := range p.entries {
		e.nextProbe = time.Time{}
	}
	p.mu.Unlock()
	guardSweep(context.Background(), p, timing)
}

func stateOf(t *testing.T, p *Pool, raw string) *poolEntry {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.raw == raw {
			return e
		}
	}
	t.Fatalf("exit %q not in pool", raw)
	return nil
}

// The full path: a healthy exit degrades to suspect on the first failure, is
// evicted on the third, and only returns to live after two consecutive passes.
func TestGuardStateMachineWalksLiveSuspectEvictedAndBack(t *testing.T) {
	useConnectOnlyProbe(t)
	timing := fastGuardTiming()
	exit := newFakeExit(t, verdictPass)
	pool, err := NewPool([]string{exit.url()})
	if err != nil {
		t.Fatal(err)
	}

	sweepNow(t, pool, timing)
	if got := stateOf(t, pool, exit.url()); got.stateName() != stateLive || !got.wsOK {
		t.Fatalf("after a passing probe state=%s wsOk=%v, want live/true", got.stateName(), got.wsOK)
	}

	exit.set(verdictFail)
	for round := 1; round <= 2; round++ {
		sweepNow(t, pool, timing)
		entry := stateOf(t, pool, exit.url())
		if entry.stateName() != stateSuspect {
			t.Fatalf("after %d failures state=%s, want suspect", round, entry.stateName())
		}
		if entry.consecutiveFailures != round {
			t.Fatalf("consecutiveFailures=%d, want %d", entry.consecutiveFailures, round)
		}
		if entry.lastError == "" {
			t.Fatal("a failed probe must record lastError")
		}
	}

	sweepNow(t, pool, timing)
	evicted := stateOf(t, pool, exit.url())
	if evicted.stateName() != stateEvicted {
		t.Fatalf("after 3 failures state=%s, want evicted", evicted.stateName())
	}
	firstBackoff := evicted.backoff
	if firstBackoff != timing.base {
		t.Fatalf("first eviction backoff=%s, want %s", firstBackoff, timing.base)
	}

	// Still failing: the backoff doubles and the exit stays out of rotation.
	sweepNow(t, pool, timing)
	if got := stateOf(t, pool, exit.url()); got.backoff != 2*firstBackoff || got.stateName() != stateEvicted {
		t.Fatalf("backoff=%s state=%s, want %s/evicted", got.backoff, got.stateName(), 2*firstBackoff)
	}

	// One good probe is not enough to come back.
	exit.set(verdictPass)
	sweepNow(t, pool, timing)
	if got := stateOf(t, pool, exit.url()); got.stateName() != stateEvicted {
		t.Fatalf("after 1 success state=%s, want still evicted", got.stateName())
	}
	sweepNow(t, pool, timing)
	restored := stateOf(t, pool, exit.url())
	if restored.stateName() != stateLive {
		t.Fatalf("after %d successes state=%s, want live", guardRestoreAfter, restored.stateName())
	}
	if restored.backoff != 0 || restored.consecutiveFailures != 0 {
		t.Fatalf("restored exit kept backoff=%s failures=%d", restored.backoff, restored.consecutiveFailures)
	}
}

// The eviction backoff doubles and is capped at 15 minutes.
func TestEvictionBackoffDoublesAndIsCapped(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{entries: []*poolEntry{{raw: "exit"}}, wsChanged: make(chan struct{})}
	entry := pool.entries[0]
	failure := probeResult{health: "unreachable", err: fmt.Errorf("dead")}
	now := time.Now()
	var seen []time.Duration
	for round := 0; round < 12; round++ {
		pool.applyProbe(entry, failure, now, timing)
		if entry.stateName() == stateEvicted {
			seen = append(seen, entry.backoff)
		}
	}
	if len(seen) < 4 {
		t.Fatalf("expected several evicted rounds, got %d", len(seen))
	}
	if seen[0] != guardBaseInterval || seen[1] != 2*guardBaseInterval {
		t.Fatalf("backoff progression = %v, want %s then %s", seen[:2], guardBaseInterval, 2*guardBaseInterval)
	}
	if last := seen[len(seen)-1]; last != guardMaxBackoff {
		t.Fatalf("backoff cap = %s, want %s", last, guardMaxBackoff)
	}
	if entry.probeIntervalLocked(timing) != guardMaxBackoff {
		t.Fatalf("evicted probe interval = %s, want the backoff", entry.probeIntervalLocked(timing))
	}
}

// The sliding window is 20 rounds: an old failure eventually falls out and stops
// dragging the success rate down.
func TestSlidingWindowKeepsTwentyRounds(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{entries: []*poolEntry{{raw: "exit"}}, wsChanged: make(chan struct{})}
	entry := pool.entries[0]
	now := time.Now()
	pass := probeResult{pass: true, health: "reachable", wsOK: true, l2Code: 200, l2Latency: 300 * time.Millisecond}
	pool.applyProbe(entry, probeResult{health: "unreachable", err: fmt.Errorf("dead")}, now, timing)
	for round := 0; round < guardWindowSize+5; round++ {
		pool.applyProbe(entry, pass, now, timing)
	}
	if len(entry.window) != guardWindowSize {
		t.Fatalf("window length = %d, want %d", len(entry.window), guardWindowSize)
	}
	passes, attempts, median := windowStats(entry.window)
	if passes != guardWindowSize || attempts != guardWindowSize {
		t.Fatalf("window stats = %d/%d, want %d/%d", passes, attempts, guardWindowSize, guardWindowSize)
	}
	if median != 300*time.Millisecond {
		t.Fatalf("median latency = %s, want 300ms", median)
	}
}

// The score is Laplace smoothed, latency aware and gated on the WebSocket term.
func TestGuardScoreShape(t *testing.T) {
	if got := guardScore(1, 1, 0, false); got >= 0.60*1.0 {
		t.Fatalf("a single lucky probe scored %.3f; Laplace smoothing must keep it below the success ceiling", got)
	}
	atBudget := guardScore(10, 10, guardLatencyBudget, true)
	half := 0.60*(11.0/12.0) + 0.25*0.5 + 0.15
	if diff := atBudget - half; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score at the latency budget = %.6f, want %.6f", atBudget, half)
	}
	withWS := guardScore(10, 10, 500*time.Millisecond, true)
	withoutWS := guardScore(10, 10, 500*time.Millisecond, false)
	if withWS-withoutWS < 0.149 {
		t.Fatalf("wsTerm contributed %.3f, want 0.15", withWS-withoutWS)
	}
	if guardScore(0, 0, 0, true) != 0 {
		t.Fatal("an exit with no samples must score 0")
	}
}

// Routing picks the highest score and stays there; it does not rotate.
func TestSelectionAlwaysPrefersTheHighestScore(t *testing.T) {
	timing := defaultGuardTiming()
	slow, fast, noWS := "http://slow.example:8080", "http://fast.example:8080", "http://no-ws.example:8080"
	pool := &Pool{
		entries: []*poolEntry{
			{raw: slow},
			{raw: fast},
			{raw: noWS},
		},
		wsChanged: make(chan struct{}),
		wsLimit:   4,
	}
	now := time.Now()
	for round := 0; round < 5; round++ {
		pool.applyProbe(pool.entries[0], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 3 * time.Second}, now, timing)
		pool.applyProbe(pool.entries[1], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 200 * time.Millisecond}, now, timing)
		// Tunnel works, chathub Upgrade does not: exactly the profile behind the 502s.
		pool.applyProbe(pool.entries[2], probeResult{pass: false, wsOK: false, health: "upstream_error", l2Latency: 150 * time.Millisecond}, now, timing)
	}

	for attempt := 0; attempt < 5; attempt++ {
		if got := pool.pick(); got == nil || got.raw != fast {
			t.Fatalf("attempt %d picked %#v, want the highest-scored exit", attempt, got)
		}
	}
	// Runner-up by score, not the next slot in a rotation.
	if got := pool.pickDifferent(pool.entries[1]); got == nil || got.raw != slow {
		t.Fatalf("runner-up = %#v, want slow (still live) over no-ws (evicted)", got)
	}
	scores := map[string]float64{}
	for _, item := range pool.List() {
		scores[item["url"].(string)] = item["score"].(float64)
	}
	if scores[fast] <= scores[slow] || scores[slow] <= scores[noWS] {
		t.Fatalf("score order = %v, want fast > slow > no-ws", scores)
	}
}

// A suspect exit is a fallback only: it is never chosen while a live exit exists,
// and it is chosen as soon as none does.
func TestSuspectExitIsOnlyUsedWhenNothingIsLive(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{
		entries:   []*poolEntry{{raw: "healthy"}, {raw: "shaky"}},
		wsChanged: make(chan struct{}),
		wsLimit:   4,
	}
	now := time.Now()
	healthy, shaky := pool.entries[0], pool.entries[1]
	pass := func(latency time.Duration) probeResult {
		return probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: latency}
	}
	fail := probeResult{health: "unreachable", err: fmt.Errorf("flap")}
	// The healthy exit is slow and has a patchy history, yet it never fails three
	// times in a row, so it is live. The shaky exit is fast and mostly passing, so
	// on score alone it would win - which is the point of the test.
	for round := 0; round < 10; round++ {
		pool.applyProbe(healthy, fail, now, timing)
		pool.applyProbe(healthy, pass(4*time.Second), now, timing)
		pool.applyProbe(shaky, pass(50*time.Millisecond), now, timing)
	}
	pool.applyProbe(shaky, fail, now, timing)
	if healthy.stateName() != stateLive {
		t.Fatalf("healthy state = %s, want live", healthy.stateName())
	}
	if shaky.stateName() != stateSuspect {
		t.Fatalf("shaky state = %s, want suspect", shaky.stateName())
	}
	if shaky.score <= healthy.score {
		t.Fatalf("test is not meaningful unless the suspect exit scores higher: %.3f vs %.3f", shaky.score, healthy.score)
	}
	if got := pool.pick(); got == nil || got.raw != "healthy" {
		t.Fatalf("picked %#v while a live exit existed, want healthy", got)
	}
	if got := pool.pickStickyWebSocketLocked("", map[*poolEntry]struct{}{}); got == nil || got.raw != "healthy" {
		t.Fatalf("websocket picked %#v while a live exit existed, want healthy", got)
	}

	// Now nothing is live: the suspect exit takes over rather than the pool going dark.
	for round := 0; round < guardEvictAfter; round++ {
		pool.applyProbe(healthy, fail, now, timing)
	}
	if healthy.stateName() != stateEvicted {
		t.Fatalf("healthy state = %s, want evicted", healthy.stateName())
	}
	if got := pool.pick(); got == nil || got.raw != "shaky" {
		t.Fatalf("picked %#v with no live exit, want the suspect fallback", got)
	}

	// And with everything evicted an exit is still returned: entries exist, so
	// falling through to nil would dial Microsoft directly from the operator's own
	// address. Only an empty pool means direct.
	for round := 0; round < guardEvictAfter; round++ {
		pool.applyProbe(shaky, fail, now, timing)
	}
	if got := pool.pick(); got == nil {
		t.Fatal("an all-evicted non-empty pool must still return an exit, not fall back to a direct dial")
	}
}

// The per-exit WebSocket budget is the only reason the best exit is skipped.
func TestWebSocketBudgetFallsToTheNextScore(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{
		entries:   []*poolEntry{{raw: "best"}, {raw: "second"}},
		wsChanged: make(chan struct{}),
		wsLimit:   1,
	}
	now := time.Now()
	for round := 0; round < 4; round++ {
		pool.applyProbe(pool.entries[0], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 100 * time.Millisecond}, now, timing)
		pool.applyProbe(pool.entries[1], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 900 * time.Millisecond}, now, timing)
	}
	if got := pool.pickStickyWebSocketLocked("", map[*poolEntry]struct{}{}); got.raw != "best" {
		t.Fatalf("first pick = %s, want best", got.raw)
	}
	pool.entries[0].activeWebSockets = 1
	if got := pool.pickStickyWebSocketLocked("", map[*poolEntry]struct{}{}); got == nil || got.raw != "second" {
		t.Fatalf("pick with the best exit at capacity = %#v, want second", got)
	}
}

// Account affinity survives: a bound live exit is reused even when another exit
// scores higher, so a conversation does not migrate between egresses.
func TestStickyAffinityOutranksScore(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{
		entries:   []*poolEntry{{raw: "bound"}, {raw: "better"}},
		wsChanged: make(chan struct{}),
		wsLimit:   4,
		sticky:    map[string]string{"account-1": "bound"},
	}
	now := time.Now()
	for round := 0; round < 4; round++ {
		pool.applyProbe(pool.entries[0], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 900 * time.Millisecond}, now, timing)
		pool.applyProbe(pool.entries[1], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 80 * time.Millisecond}, now, timing)
	}
	if got := pool.pickSticky("account-1"); got == nil || got.raw != "bound" {
		t.Fatalf("sticky pick = %#v, want the bound exit", got)
	}
	if got := pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{}); got == nil || got.raw != "bound" {
		t.Fatalf("sticky websocket pick = %#v, want the bound exit", got)
	}
	// Once the bound exit is evicted the account fails over to the best exit.
	for round := 0; round < guardEvictAfter; round++ {
		pool.applyProbe(pool.entries[0], probeResult{health: "unreachable", err: fmt.Errorf("dead")}, now, timing)
	}
	if got := pool.pickSticky("account-1"); got == nil || got.raw != "better" {
		t.Fatalf("sticky pick after eviction = %#v, want better", got)
	}
}

// Admission gate: a failing candidate is refused with a message naming the layer,
// and an exit whose URL carries userinfo authenticates correctly instead of being
// misjudged as broken.
func TestValidateProxyCandidate(t *testing.T) {
	previous := admissionProbe
	admissionProbe = func(ctx context.Context, dial dialFunc, timeout time.Duration) (int, time.Duration, error) {
		result := connectOnlyProbe(ctx, dial, timeout)
		return result.l2Code, result.l2Latency, result.err
	}
	t.Cleanup(func() { admissionProbe = previous })

	dead := newFakeExit(t, verdictFail)
	err := ValidateProxyCandidate(context.Background(), dead.url())
	if err == nil {
		t.Fatal("a dead exit must be refused")
	}
	if !strings.Contains(err.Error(), "L2") || !strings.Contains(err.Error(), "入池校验失败") {
		t.Fatalf("error = %v, want a readable Chinese message naming the failing layer", err)
	}

	authenticated := newFakeExit(t, verdictAuthed)
	if err := ValidateProxyCandidate(context.Background(), authenticated.urlWithUserinfo()); err != nil {
		t.Fatalf("an exit with userinfo must pass admission: %v (rejected=%d)", err, authenticated.rejected.Load())
	}
	if authenticated.authed.Load() == 0 {
		t.Fatal("the admission probe did not send Proxy-Authorization")
	}
	if authenticated.rejected.Load() != 0 {
		t.Fatalf("the admission probe was rejected %d times for missing credentials", authenticated.rejected.Load())
	}
	if err := ValidateProxyCandidate(context.Background(), authenticated.url()); err == nil {
		t.Fatal("the same exit without credentials must be refused, otherwise the probe is not really authenticating")
	}
	if err := ValidateProxyCandidate(context.Background(), "ftp://user:pw@127.0.0.1:1"); err == nil {
		t.Fatal("an unusable scheme must be refused")
	} else if strings.Contains(err.Error(), "pw") {
		t.Fatalf("admission error leaked the credential: %v", err)
	}
}

// The candidate probe must not outlive its timeout, so an admin request cannot hang.
func TestValidateProxyCandidateHonorsContext(t *testing.T) {
	previous := admissionProbe
	admissionProbe = func(ctx context.Context, dial dialFunc, timeout time.Duration) (int, time.Duration, error) {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	t.Cleanup(func() { admissionProbe = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := ValidateProxyCandidate(ctx, "http://127.0.0.1:1"); err == nil {
		t.Fatal("expected the probe to fail once the context expired")
	}
	if elapsed := time.Since(started); elapsed > admissionTimeout {
		t.Fatalf("admission took %s, want it bounded by the caller context", elapsed)
	}
}

// Status output: every pre-existing field is still present, the guard fields are
// there, and the url no longer carries the password.
func TestListRedactsCredentialsAndKeepsDeleteWorking(t *testing.T) {
	t.Cleanup(func() { _ = ConfigurePool(nil) })
	raw := "http://m365exit:s3cr3t-pa55word@203.0.113.7:3128"
	if err := ConfigurePool([]string{raw}); err != nil {
		t.Fatal(err)
	}
	items := ProxyPoolStatus()
	if len(items) != 1 {
		t.Fatalf("status = %#v, want one exit", items)
	}
	item := items[0]
	for _, key := range []string{"url", "failures", "cooldownUntil", "lastCheck", "latencyMs", "lastError", "health", "activeWebSockets", "maxWebSockets"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("pre-existing field %q disappeared from the status payload", key)
		}
	}
	for _, key := range []string{"state", "score", "medianLatencyMs", "wsOk", "lastCheckedAt", "consecutiveFailures", "id"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("guard field %q missing from the status payload", key)
		}
	}
	shown, _ := item["url"].(string)
	if strings.Contains(shown, "s3cr3t-pa55word") {
		t.Fatalf("status url leaked the proxy password: %q", shown)
	}
	if shown != "http://m365exit:"+proxyPasswordPlaceholder+"@203.0.113.7:3128" {
		t.Fatalf("status url = %q, want the masked form", shown)
	}
	if state, _ := item["state"].(string); state != stateLive {
		t.Fatalf("state = %q, want live for a freshly configured exit", state)
	}

	// Deleting by the id from the status payload resolves back to a dialable URL.
	id, _ := item["id"].(string)
	if got := ProxyRawURLForID(id); got != raw {
		t.Fatalf("id %q resolved to %q, want the raw URL", id, got)
	}
	// And deleting by the masked url still works.
	if err := RemoveProxy(shown); err != nil {
		t.Fatalf("delete by the masked url failed: %v", err)
	}
	if len(ProxyPoolStatus()) != 0 {
		t.Fatalf("pool not empty after delete: %#v", ProxyPoolStatus())
	}
}

// The patrol stops with its context and leaves no goroutine behind.
func TestProxyGuardStopsWithContextAndLeaksNothing(t *testing.T) {
	useConnectOnlyProbe(t)
	t.Setenv(EnvProxyGuardInterval, "5s")
	t.Cleanup(func() { _ = ConfigurePool(nil) })
	exit := newFakeExit(t, verdictPass)
	if err := ConfigurePool([]string{exit.url()}); err != nil {
		t.Fatal(err)
	}
	settleGoroutines()
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	wait := StartProxyGuard(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stateOf(t, CurrentPool(), exit.url()).lastCheck.IsZero() {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	if stateOf(t, CurrentPool(), exit.url()).lastCheck.IsZero() {
		t.Fatal("the patrol never probed the pool")
	}

	cancel()
	stopped := make(chan struct{})
	go func() { wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the patrol did not stop when its context was cancelled")
	}

	settleGoroutines()
	if grown := runtime.NumGoroutine() - baseline; grown > 0 {
		t.Fatalf("goroutine count grew by %d after the patrol stopped", grown)
	}
}

func TestProxyGuardCanBeDisabled(t *testing.T) {
	t.Setenv(EnvProxyGuardDisabled, "1")
	wait := StartProxyGuard(context.Background())
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a disabled guard must return immediately")
	}
}

func TestGuardTimingFromEnv(t *testing.T) {
	t.Setenv(EnvProxyGuardInterval, "")
	if got := guardTimingFromEnv(); got.base != guardBaseInterval || got.suspect != guardSuspectInterval {
		t.Fatalf("default timing = %s/%s, want %s/%s", got.base, got.suspect, guardBaseInterval, guardSuspectInterval)
	}
	t.Setenv(EnvProxyGuardInterval, "90s")
	if got := guardTimingFromEnv(); got.base != 90*time.Second || got.suspect != 45*time.Second {
		t.Fatalf("configured timing = %s/%s, want 90s/45s", got.base, got.suspect)
	}
	t.Setenv(EnvProxyGuardInterval, "120")
	if got := guardTimingFromEnv(); got.base != 2*time.Minute {
		t.Fatalf("bare-seconds timing = %s, want 2m", got.base)
	}
	t.Setenv(EnvProxyGuardInterval, "nonsense")
	if got := guardTimingFromEnv(); got.base != guardBaseInterval {
		t.Fatalf("invalid timing = %s, want the default %s", got.base, guardBaseInterval)
	}
	if tick := defaultGuardTiming().tick(); tick > guardSuspectInterval {
		t.Fatalf("tick = %s, want no coarser than the suspect interval %s", tick, guardSuspectInterval)
	}
}

func TestGuardConcurrencyIsCapped(t *testing.T) {
	t.Setenv(EnvProxyGuardConcurrency, "200")
	if got := guardConcurrency(); got > guardMaxConcurrency {
		t.Fatalf("concurrency = %d, want at most %d", got, guardMaxConcurrency)
	}
	t.Setenv(EnvProxyGuardConcurrency, "4")
	if got := guardConcurrency(); got != 4 {
		t.Fatalf("concurrency = %d, want 4", got)
	}
}

// The WebSocket Upgrade verdict: Microsoft refusing an uncredentialed handshake is
// a pass, the proxy refusing on its own behalf is not.
func TestUpgradeStatusClassification(t *testing.T) {
	for _, code := range []int{101, 401, 403, 404} {
		if !upgradeStatusPasses(code) {
			t.Fatalf("status %d must pass: the tunnel carried a real handshake", code)
		}
	}
	for _, code := range []int{400, 407, 500, 502, 503} {
		if upgradeStatusPasses(code) {
			t.Fatalf("status %d must fail: it is the proxy refusing, not Microsoft", code)
		}
	}
}

func settleGoroutines() {
	for attempt := 0; attempt < 50; attempt++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}
