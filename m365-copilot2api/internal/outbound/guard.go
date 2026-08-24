package outbound

// Quality guard for the egress pool.
//
// Before this file existed there was no background patrol at all: CheckAll had a
// single caller, the admin HTTP handler, so an exit was only ever checked when a
// human clicked the button. Failures merely produced a short backoff and an exit
// was never removed from rotation, which is how twenty dead proxies once pushed the
// 502 rate from 0.1% to 63.8%.
//
// The guard closes that gap with three parts:
//
//	patrol       every base interval, probe the exits whose turn has come
//	state machine  live -> suspect -> evicted -> live, in Pool.applyProbe
//	score        a sliding-window quality number the router selects on
//
// Reference implementation: tools/proxy-quality-guard. That is a standalone main
// package and is deliberately not imported; the logic is reproduced here.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Exit states.
//
//	live     participates in routing, probed every base interval
//	suspect  1..2 consecutive failures, probed twice as often, used for routing
//	         only when no live exit is available
//	evicted  3 consecutive failures, out of rotation, re-probed on a doubling
//	         backoff capped at 15 minutes, restored after 2 consecutive passes
const (
	stateLive    = "live"
	stateSuspect = "suspect"
	stateEvicted = "evicted"
)

// Guard thresholds.
//
// evictAfter=3 tolerates a single network hiccup while still removing a genuinely
// dead exit within roughly three minutes. restoreAfter=2 demands two consecutive
// passes so a half-dead exit cannot flap back into rotation on one lucky probe.
const (
	guardEvictAfter   = 3
	guardRestoreAfter = 2
	guardWindowSize   = 20

	guardBaseInterval    = 60 * time.Second
	guardSuspectInterval = 30 * time.Second
	guardMaxBackoff      = 15 * time.Minute

	// guardLatencyBudget is the median L2 latency that scores exactly 0.5 on the
	// latency term.
	guardLatencyBudget = 1500 * time.Millisecond

	// guardMaxConcurrency caps in-flight probes. Microsoft is the target, so this
	// is a politeness limit as much as a resource limit.
	guardMaxConcurrency     = 20
	// guardDefaultConcurrency was 20, equal to the ceiling, so a 15-exit pool probed
	// every exit in the same instant. Short-lived exits (21-minute 51daili leases)
	// flap, and a fully synchronous round made them all fail in the same second,
	// which drained the ws-dial retry budget and surfaced as a 502 upstream
	// handshake failure. A lower fan-out keeps exit failures independent.
	guardDefaultConcurrency = 6

	EnvProxyGuardInterval    = "M365_PROXY_GUARD_INTERVAL"
	EnvProxyGuardConcurrency = "M365_PROXY_GUARD_CONCURRENCY"
	EnvProxyGuardDisabled    = "M365_PROXY_GUARD_DISABLED"
)

type guardTiming struct {
	base         time.Duration
	suspect      time.Duration
	maxBackoff   time.Duration
	evictAfter   int
	restoreAfter int
	window       int
}

// tick is how often the patrol wakes up. It has to be finer than the shortest
// per-state interval, otherwise a suspect exit would be probed on the live cadence.
func (t guardTiming) tick() time.Duration {
	shortest := t.base
	if t.suspect > 0 && t.suspect < shortest {
		shortest = t.suspect
	}
	tick := shortest / 2
	if tick < time.Second {
		tick = time.Second
	}
	return tick
}

func defaultGuardTiming() guardTiming {
	return guardTiming{
		base:         guardBaseInterval,
		suspect:      guardSuspectInterval,
		maxBackoff:   guardMaxBackoff,
		evictAfter:   guardEvictAfter,
		restoreAfter: guardRestoreAfter,
		window:       guardWindowSize,
	}
}

// guardTimingFromEnv reads the configurable patrol period. It accepts a Go
// duration ("45s", "2m") or a bare number of seconds.
func guardTimingFromEnv() guardTiming {
	timing := defaultGuardTiming()
	raw := strings.TrimSpace(os.Getenv(EnvProxyGuardInterval))
	if raw == "" {
		return timing
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		if seconds, convErr := strconv.Atoi(raw); convErr == nil {
			interval = time.Duration(seconds) * time.Second
		} else {
			return timing
		}
	}
	if interval < 5*time.Second || interval > time.Hour {
		return timing
	}
	timing.base = interval
	timing.suspect = interval / 2
	if timing.suspect < time.Second {
		timing.suspect = time.Second
	}
	return timing
}

func guardConcurrency() int {
	limit := outboundIntEnv(EnvProxyGuardConcurrency, guardDefaultConcurrency, 1, guardMaxConcurrency)
	if limit > guardMaxConcurrency {
		limit = guardMaxConcurrency
	}
	return limit
}

func guardDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProxyGuardDisabled)))
	return v == "1" || v == "true" || v == "yes"
}

// guardScore is the routing weight of an exit.
//
//	score = 0.60*successRate + 0.25*latencyTerm + 0.15*wsTerm
//	successRate = (passes+1)/(attempts+2)      Laplace smoothed, so one lucky
//	                                           probe cannot reach 1.0
//	latencyTerm = B/(B+medianL2), B = 1500ms   exactly 0.5 at the budget
//	wsTerm      = 1 when the last round carried a WebSocket Upgrade, else 0
//
// Weights: whether an exit works at all outranks how fast it is, hence 0.60.
// Latency shapes the experience but not availability, 0.25. wsTerm is a
// pass/fail gate worth 0.15 so an exit where HTTP works but the chathub Upgrade
// does not - the main source of the 502s - cannot reach the top of the ranking.
func guardScore(passes, attempts int, medianLatency time.Duration, wsOK bool) float64 {
	if attempts <= 0 {
		return 0
	}
	success := float64(passes+1) / float64(attempts+2)
	latency := 0.0
	if medianLatency > 0 {
		latency = float64(guardLatencyBudget) / float64(guardLatencyBudget+medianLatency)
	}
	ws := 0.0
	if wsOK {
		ws = 1.0
	}
	return 0.60*success + 0.25*latency + 0.15*ws
}

// probeRound is the probe the guard runs. It is a package variable so a test can
// drive the state machine through a local fake proxy instead of reaching out to
// Microsoft; production always uses probeExit.
var probeRound = probeExit

// StartProxyGuard launches the background patrol and returns a function that
// blocks until the patrol goroutine has exited.
//
// Ownership is explicit on purpose: the caller passes the process context and
// calls the returned function during shutdown, so no goroutine can outlive main.
func StartProxyGuard(ctx context.Context) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	if guardDisabled() {
		log.Printf("proxy guard disabled via %s", EnvProxyGuardDisabled)
		return func() {}
	}
	timing := guardTimingFromEnv()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyGuard(ctx, timing)
	}()
	log.Printf("proxy guard started interval=%s suspect=%s concurrency=%d",
		timing.base, timing.suspect, guardConcurrency())
	return func() { <-done }
}

func runProxyGuard(ctx context.Context, timing guardTiming) {
	ticker := time.NewTicker(timing.tick())
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		guardSweep(ctx, CurrentPool(), timing)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// guardSweep probes every exit of p whose next probe is due. The caller resolves
// the pool on every sweep rather than capturing it once: ConfigurePool edits the
// live pool, and the pool can also be detached entirely.
func guardSweep(ctx context.Context, p *Pool, timing guardTiming) {
	if p == nil {
		return
	}
	due := p.dueForProbe(time.Now(), timing)
	if len(due) == 0 {
		return
	}
	semaphore := make(chan struct{}, guardConcurrency())
	var wg sync.WaitGroup
	for _, entry := range due {
		wg.Add(1)
		go func(e *poolEntry) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			probeCtx, cancel := context.WithTimeout(ctx, probeRoundBudget(defaultProbeTimeout))
			defer cancel()
			result := probeRound(probeCtx, tunnelDialer(e.clients), defaultProbeTimeout)
			p.applyProbe(e, result, time.Now(), timing)
			if result.err != nil || !result.pass {
				logProbe(e.raw, result)
			}
		}(entry)
	}
	wg.Wait()
}

// admissionTimeout bounds the pre-admission probe. The admin handler runs it
// inline, so it must not be able to hang the request.
const admissionTimeout = 10 * time.Second

// admissionProbe is the L2 check run before an exit enters the pool. Same seam as
// probeRound: a test can substitute a local target.
var admissionProbe = probeAuthPlane

// ValidateProxyCandidate runs one L2 probe (CONNECT + TLS + HEAD against
// login.microsoftonline.com) before an exit is allowed into the pool. A failure
// returns a message that names the layer that broke, in Chinese, for the admin UI.
//
// It dials through the very same client stack production uses, so an exit whose URL
// carries userinfo is authenticated exactly as it would be in service - a proxy
// with a password is not misjudged as broken. No credential of ours is ever sent to
// Microsoft, and the target certificate is verified strictly.
func ValidateProxyCandidate(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("入池校验失败：代理地址为空")
	}
	clients, err := New(raw)
	if err != nil {
		// New never echoes the URL, so this cannot leak a password.
		return fmt.Errorf("入池校验失败：代理地址无法使用：%w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()
	code, latency, err := admissionProbe(ctx, tunnelDialer(clients), admissionTimeout)
	if err != nil {
		return fmt.Errorf("入池校验失败：L2 认证面（CONNECT+TLS+HEAD %s）不通，耗时 %s：%w",
			probeAuthHost, latency.Round(time.Millisecond), err)
	}
	if code < 200 || code >= 500 {
		return fmt.Errorf("入池校验失败：L2 认证面（%s）返回状态 %d —— 代理隧道可用但上游异常，拒绝入池",
			probeAuthHost, code)
	}
	return nil
}
