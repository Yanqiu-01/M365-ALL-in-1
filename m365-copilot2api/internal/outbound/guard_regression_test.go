package outbound

// Regression tests for defects in the quality guard and the pool's health
// accounting. Each test names the behaviour that was wrong and asserts the rule that
// replaced it. Nothing here touches the network: every exit is either a bare
// poolEntry driven through applyProbe, or a local dial hook.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger into a buffer for the duration of the
// test and returns an accessor for what was written.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buffer bytes.Buffer
	previousOut := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return buffer.String
}

func failingProbe() probeResult {
	return probeResult{health: "unreachable", err: errors.New("exit is dead")}
}

func passingProbe() probeResult {
	return probeResult{
		pass: true, health: "reachable", wsOK: true, m365OK: true,
		l2Code: http.StatusOK, l2Latency: 200 * time.Millisecond,
	}
}

// An evicted exit must not be able to leave eviction by FAILING.
//
// applyProbe's failure branch had two cases: at/above evictAfter it evicted, and the
// default wrote stateSuspect unconditionally. Suspect is a routable tier. So an
// already-evicted exit that had collected one restoring pass - which resets
// consecutiveFailures to 0 while leaving the exit evicted, because restoreAfter is 2 -
// was promoted back into rotation by its very next failing probe. One pass and one
// failure, in that order, laundered an evicted exit into a fallback the router will
// hand real traffic to.
func TestEvictedExitCannotEscapeEvictionByFailing(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{entries: []*poolEntry{{raw: "http://dead.example:3128"}}, wsChanged: make(chan struct{})}
	entry := pool.entries[0]
	now := time.Now()

	for round := 0; round < timing.evictAfter; round++ {
		pool.applyProbe(entry, failingProbe(), now, timing)
	}
	if entry.stateName() != stateEvicted {
		t.Fatalf("state after %d failures = %s, want evicted", timing.evictAfter, entry.stateName())
	}
	evictedBackoff := entry.backoff

	// One pass: not enough to be restored, so still evicted - but consecutiveFailures
	// is now 0, which is the state the escape depended on.
	pool.applyProbe(entry, passingProbe(), now, timing)
	if entry.stateName() != stateEvicted {
		t.Fatalf("state after 1 pass = %s, want still evicted (restoreAfter=%d)", entry.stateName(), timing.restoreAfter)
	}
	if entry.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures after a pass = %d, want 0; the escape needs this to be 0", entry.consecutiveFailures)
	}

	// The failure that used to promote it to suspect.
	pool.applyProbe(entry, failingProbe(), now, timing)
	if entry.stateName() != stateEvicted {
		t.Fatalf("a FAILING probe moved an evicted exit to %s; failing must never be a way out of eviction", entry.stateName())
	}
	if entry.tier(now) != tierEvicted {
		t.Fatalf("tier = %s, want evicted so the router does not select it", tierName(entry.tier(now)))
	}
	if entry.backoff < evictedBackoff {
		t.Fatalf("backoff went from %s to %s; the eviction ladder must not be reset by a failure", evictedBackoff, entry.backoff)
	}

	// And the only way out still works.
	for round := 0; round < timing.restoreAfter; round++ {
		pool.applyProbe(entry, passingProbe(), now, timing)
	}
	if entry.stateName() != stateLive {
		t.Fatalf("state after %d consecutive passes = %s, want live", timing.restoreAfter, entry.stateName())
	}
}

// e.failures was a one-way ratchet: mark() only ever incremented it, and only a
// successful real request cleared it. But the cooldown it sizes ranks the exit as
// evicted, so an exit that failed enough times stopped being selected and could never
// produce that success. The count is also what sizes the next cooldown, so an exit
// stuck at a high count got the 2-minute ceiling from a single fresh failure forever.
func TestTrafficFailureRatchetDecaysAndIsClearedByAPassingProbe(t *testing.T) {
	pool := &Pool{entries: []*poolEntry{{raw: "http://ratchet.example:3128"}}, wsChanged: make(chan struct{})}
	entry := pool.entries[0]

	for round := 0; round < 40; round++ {
		pool.mark(entry.raw, errors.New("request failed"))
	}
	pool.mu.Lock()
	peak := entry.failures
	cooldown := entry.cooldown
	pool.mu.Unlock()
	if peak != 40 {
		t.Fatalf("failures = %d, want 40", peak)
	}

	// Quiet time retires the count. dueForProbe is the patrol's own clock and runs
	// whether or not the exit carries traffic, which is what makes the decay
	// reachable for an exit the router has stopped selecting.
	pool.dueForProbe(cooldown.Add(3*failureDecayInterval), defaultGuardTiming())
	pool.mu.Lock()
	afterThreeSteps := entry.failures
	pool.mu.Unlock()
	if afterThreeSteps != peak-3 {
		t.Fatalf("failures after 3 decay intervals = %d, want %d", afterThreeSteps, peak-3)
	}

	pool.dueForProbe(cooldown.Add(500*failureDecayInterval), defaultGuardTiming())
	pool.mu.Lock()
	drained := entry.failures
	drainedCooldown := entry.cooldown
	pool.mu.Unlock()
	if drained != 0 {
		t.Fatalf("failures after a long quiet period = %d, want 0", drained)
	}
	if !drainedCooldown.IsZero() {
		t.Fatalf("cooldown = %v, want cleared once the ratchet is drained", drainedCooldown)
	}

	// The observable consequence: a fresh single failure now costs a short cooldown
	// again instead of the 2-minute ceiling the stuck count produced.
	before := time.Now()
	pool.mark(entry.raw, errors.New("request failed"))
	pool.mu.Lock()
	freshCooldown := entry.cooldown.Sub(before)
	pool.mu.Unlock()
	if freshCooldown > 10*time.Second {
		t.Fatalf("cooldown after decay = %s, want the short first-failure cooldown, not the ceiling", freshCooldown)
	}

	// A passing probe is the other recovery path: the guard is the authority on
	// health, so it clears the traffic ratchet outright.
	for round := 0; round < 30; round++ {
		pool.mark(entry.raw, errors.New("request failed"))
	}
	pool.applyProbe(entry, passingProbe(), time.Now(), defaultGuardTiming())
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if entry.failures != 0 || !entry.cooldown.IsZero() {
		t.Fatalf("after a passing probe failures=%d cooldown=%v, want 0 and cleared", entry.failures, entry.cooldown)
	}
}

// A WebSocket dial the gateway or the client cancelled is not an exit failure.
// markFor recorded it as one: e.failures went up, the exit went into a cooldown that
// ranks it as evicted, and the account lost its sticky binding - all because a user
// pressed stop.
func TestWebSocketDialCancelledByCallerIsNotAnExitFailure(t *testing.T) {
	logged := captureLog(t)
	pool := &Pool{
		entries: []*poolEntry{{raw: "healthy", clients: testClients(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, http.DefaultTransport)}},
		wsLimit:   4,
		wsChanged: make(chan struct{}),
		sticky:    map[string]string{"account-1": "healthy"},
	}
	ctx, cancel := context.WithTimeout(WithAccountAffinity(context.Background(), "account-1"), 30*time.Millisecond)
	defer cancel()

	if _, err := pool.WebSocketDialer().NetDialContext(ctx, "tcp", "substrate.office.test:443"); err == nil {
		t.Fatal("expected the dial to fail once the caller context expired")
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	entry := pool.entries[0]
	if entry.failures != 0 {
		t.Fatalf("failures = %d after a dial the caller abandoned, want 0", entry.failures)
	}
	if !entry.cooldown.IsZero() {
		t.Fatalf("cooldown = %v, want none: the exit did nothing wrong", entry.cooldown)
	}
	if entry.tier(time.Now()) != tierLive {
		t.Fatalf("tier = %s, want live", tierName(entry.tier(time.Now())))
	}
	if pool.sticky["account-1"] != "healthy" {
		t.Fatalf("sticky binding = %q, want it kept: a cancelled dial must not break affinity", pool.sticky["account-1"])
	}
	if !strings.Contains(logged(), "abandoned") {
		t.Fatalf("nothing recorded the abandoned dial; log was %q", logged())
	}
}

// The same discrimination on the HTTP path: a request whose context the client
// cancelled must not demote the exit that was carrying it.
func TestRoundTripCancelledByClientIsNotAnExitFailure(t *testing.T) {
	captureLog(t)
	failure := errors.New("read: connection reset")
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "first", clients: testClients((&net.Dialer{}).DialContext, testRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, failure
			}))},
		},
		wsLimit:   4,
		wsChanged: make(chan struct{}),
		sticky:    map[string]string{"account-1": "first"},
	}
	ctx, cancel := context.WithCancel(WithAccountAffinity(context.Background(), "account-1"))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The client hangs up while the request is in flight; the transport then reports
	// a broken connection rather than a context error, which is exactly the case that
	// looked like a genuine exit failure.
	cancel()
	if _, err := (&poolRoundTripper{pool: pool}).RoundTrip(request); err == nil {
		t.Fatal("expected the round trip to fail")
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if got := pool.entries[0].failures; got != 0 {
		t.Fatalf("failures = %d after the client hung up, want 0", got)
	}
	if pool.sticky["account-1"] != "first" {
		t.Fatalf("sticky binding = %q, want it kept", pool.sticky["account-1"])
	}
}

// Admission: a check the gateway cancelled must not be reported to the operator as a
// bad proxy. ValidateProxyCandidate was the third probe entry point and the only one
// without this discrimination.
func TestValidateProxyCandidateSeparatesCancellationFromABadProxy(t *testing.T) {
	previous := admissionProbe
	t.Cleanup(func() { admissionProbe = previous })

	admissionProbe = func(ctx context.Context, _ dialFunc, _ time.Duration) (int, time.Duration, error) {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := ValidateProxyCandidate(cancelled, "http://198.51.100.9:3128")
	if err == nil {
		t.Fatal("an unvalidated candidate must still be refused")
	}
	cancelledMessage := err.Error()
	if !strings.Contains(cancelledMessage, "取消") {
		t.Fatalf("cancellation message = %q, want it to say the check was cancelled", cancelledMessage)
	}
	if strings.Contains(cancelledMessage, "不通") {
		t.Fatalf("a cancelled check reported the proxy as unreachable: %q", cancelledMessage)
	}

	admissionProbe = func(context.Context, dialFunc, time.Duration) (int, time.Duration, error) {
		return 0, 5 * time.Millisecond, errors.New("proxy CONNECT rejected: 502")
	}
	err = ValidateProxyCandidate(context.Background(), "http://198.51.100.9:3128")
	if err == nil {
		t.Fatal("a genuinely broken proxy must be refused")
	}
	if !strings.Contains(err.Error(), "不通") || !strings.Contains(err.Error(), "L2") {
		t.Fatalf("bad-proxy message = %q, want it to name the failing layer", err)
	}
	if err.Error() == cancelledMessage {
		t.Fatal("a cancelled check and a bad proxy produce the same message; the operator cannot tell them apart")
	}
}

// m365.cloud.microsoft is a signal, not a pass requirement. No pooled traffic reaches
// that host, yet ANDing it into the verdict evicted exits whose tunnel and chathub
// Upgrade both worked.
func TestM365PlaneIsASignalNotAPassRequirement(t *testing.T) {
	// The measured shape: L2 answered 200, the Upgrade came back 401 from Microsoft,
	// only the third plane failed.
	carriesTraffic := probeResult{
		l2Code: http.StatusOK, l2Latency: 300 * time.Millisecond,
		substrateOK: true, wsOK: true, wsCode: http.StatusUnauthorized, m365OK: false,
	}
	if !probePasses(carriesTraffic) {
		t.Fatal("an exit whose substrate tunnel and chathub Upgrade both work must pass; m365 must not gate the verdict")
	}
	if probePasses(probeResult{substrateOK: true, wsOK: false, m365OK: true}) {
		t.Fatal("a failed chathub Upgrade must still fail the round")
	}
	if probePasses(probeResult{substrateOK: false, wsOK: true, m365OK: true}) {
		t.Fatal("a failed substrate tunnel must still fail the round")
	}

	// Demoting it must not make it invisible: a field that is measured and never
	// reported is the defect one step removed.
	logged := captureLog(t)
	logProbe("http://198.51.100.4:3128", carriesTraffic)
	if !strings.Contains(logged(), "m365=false") {
		t.Fatalf("probe log does not report the m365 signal: %q", logged())
	}

	pool := &Pool{entries: []*poolEntry{{raw: "http://198.51.100.4:3128"}}, wsChanged: make(chan struct{})}
	carriesTraffic.pass = probePasses(carriesTraffic)
	carriesTraffic.health = "reachable"
	pool.applyProbe(pool.entries[0], carriesTraffic, time.Now(), defaultGuardTiming())
	if pool.entries[0].stateName() != stateLive {
		t.Fatalf("state = %s, want live: this exit can carry real traffic", pool.entries[0].stateName())
	}
	status := pool.List()[0]
	if got, ok := status["m365Ok"].(bool); !ok || got {
		t.Fatalf("status m365Ok = %v (present=%v), want a reported false", got, ok)
	}
}

// Every state transition is logged. The whole live->suspect->evicted->live cycle used
// to happen silently inside applyProbe, so an exit leaving rotation left no trace and
// there was no way to reconstruct why a pool went dark.
func TestStateTransitionsAreLogged(t *testing.T) {
	logged := captureLog(t)
	timing := defaultGuardTiming()
	pool := &Pool{entries: []*poolEntry{{raw: "http://203.0.113.9:3128"}}, wsChanged: make(chan struct{})}
	entry := pool.entries[0]
	now := time.Now()

	pool.applyProbe(entry, failingProbe(), now, timing)
	if out := logged(); !strings.Contains(out, "live -> suspect") {
		t.Fatalf("no live->suspect transition logged: %q", out)
	}
	for round := 1; round < timing.evictAfter; round++ {
		pool.applyProbe(entry, failingProbe(), now, timing)
	}
	if out := logged(); !strings.Contains(out, "suspect -> evicted") {
		t.Fatalf("no suspect->evicted transition logged: %q", out)
	}
	for round := 0; round < timing.restoreAfter; round++ {
		pool.applyProbe(entry, passingProbe(), now, timing)
	}
	out := logged()
	if !strings.Contains(out, "evicted -> live") {
		t.Fatalf("no evicted->live transition logged: %q", out)
	}
	// The reason and the counters that caused it have to be in the line, otherwise
	// the log says a transition happened but not why.
	for _, want := range []string{"reason=", "failures=", "successes=", "evictAfter=", "backoff="} {
		if !strings.Contains(out, want) {
			t.Fatalf("transition log is missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "203.0.113.9") && strings.Contains(out, "@") {
		t.Fatalf("transition log may not carry credentials: %q", out)
	}
	// A round that changes nothing must stay quiet, or the 15-minute patrol would
	// log a line per exit per sweep.
	quietBefore := len(logged())
	pool.applyProbe(entry, passingProbe(), now, timing)
	if len(logged()) != quietBefore {
		t.Fatalf("a non-transition was logged: %q", logged()[quietBefore:])
	}
}

// A transient capacity spike must not cost an account its exit affinity.
// pickStickyWebSocketLocked deleted the binding when the bound exit was at capacity,
// exactly as it did when the exit was unhealthy, and the caller then bound the
// account to whatever the fallback happened to be. Nothing ever re-picks the original
// exit for that account, so the loss was permanent.
func TestWebSocketCapacityPressureKeepsAccountAffinity(t *testing.T) {
	timing := defaultGuardTiming()
	pool := &Pool{
		entries:   []*poolEntry{{raw: "bound"}, {raw: "other"}},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
		sticky:    map[string]string{"account-1": "bound"},
	}
	now := time.Now()
	// Make "other" score higher, so a rebind is observable rather than a coin flip.
	for round := 0; round < 4; round++ {
		pool.applyProbe(pool.entries[0], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 900 * time.Millisecond}, now, timing)
		pool.applyProbe(pool.entries[1], probeResult{pass: true, wsOK: true, health: "reachable", l2Latency: 50 * time.Millisecond}, now, timing)
	}

	pool.mu.Lock()
	pool.entries[0].activeWebSockets = pool.wsLimit // the burst
	got := pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{})
	binding := pool.sticky["account-1"]
	pool.mu.Unlock()
	if got == nil || got.raw != "other" {
		t.Fatalf("pick at capacity = %#v, want the fallback exit to serve this dial", got)
	}
	if binding != "bound" {
		t.Fatalf("sticky binding = %q after a capacity spike, want it kept as \"bound\"", binding)
	}

	// The burst passes. The account must return to its own exit.
	pool.mu.Lock()
	pool.entries[0].activeWebSockets = 0
	got = pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{})
	pool.mu.Unlock()
	if got == nil || got.raw != "bound" {
		t.Fatalf("pick after the burst = %#v, want the account back on its own exit", got)
	}

	// An exit merely excluded from this one dial is the same transient case.
	pool.mu.Lock()
	got = pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{pool.entries[0]: {}})
	binding = pool.sticky["account-1"]
	pool.mu.Unlock()
	if got == nil || got.raw != "other" {
		t.Fatalf("pick with the bound exit excluded = %#v, want the fallback", got)
	}
	if binding != "bound" {
		t.Fatalf("sticky binding = %q after one excluded attempt, want it kept", binding)
	}

	// And the one case that must still break the affinity: the bound exit is no
	// longer healthy.
	pool.mu.Lock()
	pool.entries[0].cooldown = time.Now().Add(time.Minute)
	got = pool.pickStickyWebSocketLocked("account-1", map[*poolEntry]struct{}{})
	binding = pool.sticky["account-1"]
	pool.mu.Unlock()
	if got == nil || got.raw != "other" {
		t.Fatalf("pick with the bound exit unhealthy = %#v, want failover", got)
	}
	if binding != "other" {
		t.Fatalf("sticky binding = %q, want the failover to rebind", binding)
	}
}

// A full dial through the pool must not rebind the account either: acquireWebSocket
// used to overwrite the binding with whatever pick returned.
func TestWebSocketAcquireDoesNotRebindOnCapacityFallback(t *testing.T) {
	fallback := &testPipeDialer{}
	t.Cleanup(fallback.Close)
	pool := &Pool{
		entries: []*poolEntry{
			{raw: "bound", clients: testClients((&testPipeDialer{}).DialContext, http.DefaultTransport)},
			{raw: "other", clients: testClients(fallback.DialContext, http.DefaultTransport)},
		},
		wsLimit:   1,
		wsChanged: make(chan struct{}),
		sticky:    map[string]string{"account-1": "bound"},
	}
	pool.mu.Lock()
	pool.entries[0].activeWebSockets = 1
	pool.mu.Unlock()

	ctx := WithAccountAffinity(context.Background(), "account-1")
	conn, err := pool.WebSocketDialer().NetDialContext(ctx, "tcp", "substrate.office.test:443")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if fallback.Calls() != 1 {
		t.Fatalf("fallback dials = %d, want 1", fallback.Calls())
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.sticky["account-1"] != "bound" {
		t.Fatalf("sticky binding = %q after a capacity fallback dial, want it kept", pool.sticky["account-1"])
	}
}

// The evicted backoff ceiling has to stay above the base cadence for every base the
// env parser accepts. It was a hard 60m while the base is tunable to 1h, so a tuned
// base could equal the ceiling and a dead exit would be re-probed as often as a
// healthy one.
func TestBackoffCeilingStaysAboveATunedBaseCadence(t *testing.T) {
	for _, interval := range []string{"", "10s", "5m", "15m", "45m", "1h", "3600"} {
		t.Setenv(EnvProxyGuardInterval, interval)
		timing := guardTimingFromEnv()
		if timing.maxBackoff <= timing.base {
			t.Fatalf("interval=%q gives maxBackoff=%s base=%s; the evicted backoff is meaningless unless the ceiling exceeds the base",
				interval, timing.maxBackoff, timing.base)
		}
		if timing.suspect >= timing.base {
			t.Fatalf("interval=%q gives suspect=%s base=%s, want the suspect cadence to be faster", interval, timing.suspect, timing.base)
		}
	}
	// The defaults are untouched by the clamp.
	t.Setenv(EnvProxyGuardInterval, "")
	if got := guardTimingFromEnv(); got.maxBackoff != guardMaxBackoff {
		t.Fatalf("default maxBackoff = %s, want %s", got.maxBackoff, guardMaxBackoff)
	}
}

// The round-robin cursor is gone. Pool.next was written by adoptEntries and read by
// nothing, which made it look like live selection state.
func TestPoolCarriesNoDeadRotationCursor(t *testing.T) {
	poolType := reflect.TypeOf(Pool{})
	for index := 0; index < poolType.NumField(); index++ {
		if name := poolType.Field(index).Name; name == "next" {
			t.Fatal("Pool.next is back: it is a leftover of the removed round-robin selector and no selector reads it")
		}
	}
	// Live reconfiguration still works without it.
	pool := &Pool{entries: []*poolEntry{{raw: "a"}, {raw: "b"}, {raw: "c"}}, wsLimit: 4, wsChanged: make(chan struct{}), sticky: map[string]string{"account-1": "c"}}
	pool.adoptEntries(&Pool{entries: []*poolEntry{{raw: "a"}}, wsLimit: 2})
	if len(pool.entries) != 1 || pool.entries[0].raw != "a" {
		t.Fatalf("entries after adopt = %#v, want just a", pool.entries)
	}
	if _, ok := pool.sticky["account-1"]; ok {
		t.Fatal("a binding to a removed exit must be dropped")
	}
	if pool.pick() == nil {
		t.Fatal("selection broke after adoptEntries")
	}
}

// Guard against a silent regression of the whole point of the ctx discrimination: the
// exit's own per-attempt budget must still count as a failure. It is the exit being
// too slow, not the gateway giving up.
func TestExitOwnTimeoutStillCountsAsAFailure(t *testing.T) {
	captureLog(t)
	pool := &Pool{
		entries: []*poolEntry{{raw: "slow", clients: &Clients{
			HTTP: &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("i/o timeout")
			}), Timeout: time.Second},
			WebSocket: nil,
		}}},
		wsLimit:   4,
		wsChanged: make(chan struct{}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&poolRoundTripper{pool: pool}).RoundTrip(request)
	if err == nil {
		if response != nil && response.Body != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		t.Fatal("expected the request to fail")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.entries[0].failures != 1 {
		t.Fatalf("failures = %d, want 1: a failure with a healthy caller context is the exit's own", pool.entries[0].failures)
	}
}

// gatewayCancelled is the single expression the three call sites share.
func TestGatewayCancelledDiscrimination(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	dead, killDead := context.WithCancel(context.Background())
	killDead()

	failure := fmt.Errorf("dial tcp: connection refused")
	if gatewayCancelled(live, failure) {
		t.Fatal("a failure under a healthy context is the exit's")
	}
	if gatewayCancelled(dead, nil) {
		t.Fatal("a success is never a cancellation, whatever the context says")
	}
	if !gatewayCancelled(dead, failure) {
		t.Fatal("a failure under a cancelled context is not the exit's fault")
	}
	if gatewayCancelled(nil, failure) {
		t.Fatal("a nil context cannot prove cancellation")
	}
}
