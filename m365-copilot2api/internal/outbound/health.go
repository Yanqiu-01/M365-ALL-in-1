package outbound

import (
	"strings"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

func redactProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// Probe targets. The old default target was http://www.msftconnecttest.com over
// plain HTTP: an exit that only forwards cleartext HTTP passed it and then failed
// every chat request, and on this host it answered 000 even on a direct dial. The
// four levels below are the dependencies a chat request actually uses, so a green
// probe means the exit can carry real traffic.
//
//	L2  CONNECT + TLS + HEAD  login.microsoftonline.com   (an HTTP status is mandatory)
//	L3a CONNECT + TLS         substrate.office.com
//	L3b CONNECT + TLS         m365.cloud.microsoft
//	L3c WebSocket Upgrade     wss://substrate.office.com/m365Copilot/Chathub
//
// L3a is proven by the L3c tunnel rather than probed separately: the budget is one
// request per exit per domain per round, and the Upgrade already needs CONNECT+TLS
// to substrate.office.com.
//
// The probe never sends a credential of ours. The proxy's own Proxy-Authorization
// is written by the dialer, because that is what production does.
const (
	probeAuthHost = "login.microsoftonline.com"
	probeAuthPath = "/common/discovery/instance?api-version=1.1" +
		"&authorization_endpoint=https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	probeSubstrateHost = "substrate.office.com"
	probeM365Host      = "m365.cloud.microsoft"
	// Kept in sync with chathub wsBase (internal/chathub/client.go:62):
	// wss://substrate.office.com/m365Copilot/Chathub
	probeChathubPath = "/m365Copilot/Chathub"
	probeOrigin      = "https://m365.cloud.microsoft"
	probeUserAgent   = "m365-proxy-quality-guard"

	defaultProbeTimeout = 10 * time.Second
)

// dialFunc is the tunnel hook of an exit: it returns a byte stream to
// host:port through that exit, with the proxy handshake already done.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

type probeResult struct {
	l2Code      int
	l2Latency   time.Duration
	substrateOK bool
	m365OK      bool
	wsOK        bool
	wsCode      int
	pass        bool
	// health keeps feeding the dashboard badge: reachable / upstream_error / unreachable.
	health string
	err    error
}

// tunnelDialer returns the exact dial hook production traffic uses for this exit.
// Reusing it - instead of re-implementing CONNECT and SOCKS5 inside the probe -
// means the probe exercises the real proxy authentication path (Proxy-Authorization,
// SOCKS5 user/password), so an exit with userinfo cannot be misjudged as broken.
func tunnelDialer(c *Clients) dialFunc {
	if c == nil {
		return nil
	}
	if c.WebSocket != nil && c.WebSocket.NetDialContext != nil {
		return c.WebSocket.NetDialContext
	}
	return (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
}

// tlsThroughTunnel opens host:443 through the exit and completes a TLS handshake.
//
// Verification is strict, always. The previous screening round found 264 candidate
// exits performing TLS interception; InsecureSkipVerify here would hand a Microsoft
// token straight to the man in the middle, so there is deliberately no option to
// turn it off for the probe target.
func tlsThroughTunnel(ctx context.Context, dial dialFunc, host string, timeout time.Duration) (*tls.Conn, error) {
	raw, err := dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))
	conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS 严格校验失败（证书不可信，疑似中间人劫持）：%w", err)
	}
	return conn, nil
}

// probeAuthPlane is L2: CONNECT + TLS + HEAD against the auth endpoint. Only a real
// HTTP status line counts as a pass; no response at all is a failure.
func probeAuthPlane(ctx context.Context, dial dialFunc, timeout time.Duration) (int, time.Duration, error) {
	if dial == nil {
		return 0, 0, errors.New("该出口没有可用的拨号器")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	conn, err := tlsThroughTunnel(ctx, dial, probeAuthHost, timeout)
	if err != nil {
		return 0, time.Since(started), err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	// HEAD, no credentials, smallest possible footprint on the auth endpoint.
	request := "HEAD " + probeAuthPath + " HTTP/1.1\r\nHost: " + probeAuthHost +
		"\r\nUser-Agent: " + probeUserAgent + "\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		return 0, time.Since(started), err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, time.Since(started), fmt.Errorf("隧道已建立但没有拿到 HTTP 状态码：%w", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, time.Since(started), nil
}

// probeChathubUpgrade is L3a + L3c in one request: CONNECT + TLS to
// substrate.office.com, then a genuine WebSocket Upgrade to the chathub path.
//
// 101 is ideal. 401/403/404 also pass: the tunnel demonstrably carried a real
// WebSocket handshake and only our credentials were missing. Anything else -
// 400/407/502, a dead tunnel, a TLS failure - is a failure, because that answer
// comes from the proxy rather than from Microsoft.
func probeChathubUpgrade(ctx context.Context, dial dialFunc, timeout time.Duration) (substrateOK, wsOK bool, code int, err error) {
	if dial == nil {
		return false, false, 0, errors.New("该出口没有可用的拨号器")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := tlsThroughTunnel(ctx, dial, probeSubstrateHost, timeout)
	if err != nil {
		return false, false, 0, fmt.Errorf("L3 业务面 %s 隧道或 TLS 失败：%w", probeSubstrateHost, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return true, false, 0, err
	}
	request := "GET " + probeChathubPath + " HTTP/1.1\r\nHost: " + probeSubstrateHost +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " +
		base64.StdEncoding.EncodeToString(nonce[:]) +
		"\r\nSec-WebSocket-Version: 13\r\nOrigin: " + probeOrigin +
		"\r\nUser-Agent: " + probeUserAgent + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		return true, false, 0, fmt.Errorf("L3c WebSocket 握手写入失败：%w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return true, false, 0, fmt.Errorf("L3c WebSocket Upgrade 没有响应：%w", err)
	}
	_ = resp.Body.Close()
	if upgradeStatusPasses(resp.StatusCode) {
		return true, true, resp.StatusCode, nil
	}
	return true, false, resp.StatusCode, fmt.Errorf("L3c WebSocket Upgrade 被拒绝（状态 %d，来自代理自身而非 Microsoft）", resp.StatusCode)
}

// upgradeStatusPasses classifies the answer to the chathub Upgrade.
//
// 101 is the ideal outcome. 401/403/404 pass as well: those come from Microsoft,
// which means the tunnel carried a genuine WebSocket handshake and only our
// credentials were missing - exactly what an unauthenticated probe should see.
// 400/407/502 and friends are the proxy refusing on its own behalf, so they fail.
func upgradeStatusPasses(code int) bool {
	switch code {
	case http.StatusSwitchingProtocols, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

// probeTLSOnly is L3b: CONNECT + TLS reachability of a business-plane host.
func probeTLSOnly(ctx context.Context, dial dialFunc, host string, timeout time.Duration) error {
	if dial == nil {
		return errors.New("该出口没有可用的拨号器")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := tlsThroughTunnel(ctx, dial, host, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeExit runs one full round for a single exit: three sequential requests, one
// per domain. Returns a result even on failure so the caller can record the round.
func probeExit(ctx context.Context, dial dialFunc, timeout time.Duration) probeResult {
	result := probeResult{health: "unreachable"}
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	if dial == nil {
		result.err = errors.New("该出口没有可用的拨号器")
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}

	code, latency, err := probeAuthPlane(ctx, dial, timeout)
	result.l2Code, result.l2Latency = code, latency
	if err != nil {
		result.err = fmt.Errorf("L2 认证面 %s 探测失败：%w", probeAuthHost, err)
		return result
	}
	if code < 200 || code >= 500 {
		result.health = "upstream_error"
		result.err = fmt.Errorf("L2 认证面 %s 返回异常状态 %d", probeAuthHost, code)
		return result
	}

	substrateOK, wsOK, wsCode, wsErr := probeChathubUpgrade(ctx, dial, timeout)
	result.substrateOK, result.wsOK, result.wsCode = substrateOK, wsOK, wsCode
	if m365Err := probeTLSOnly(ctx, dial, probeM365Host, timeout); m365Err == nil {
		result.m365OK = true
	} else if wsErr == nil {
		wsErr = fmt.Errorf("L3 业务面 %s 探测失败：%w", probeM365Host, m365Err)
	}

	result.pass = result.substrateOK && result.m365OK && result.wsOK
	if result.pass {
		result.health = "reachable"
		return result
	}
	// L2 answered, so the tunnel itself works; what failed is the traffic the
	// gateway actually needs.
	result.health = "upstream_error"
	result.err = wsErr
	if result.err == nil {
		result.err = errors.New("L3 业务面探测未通过")
	}
	return result
}

// Check probes one pooled exit and folds the round into its quality state. It no
// longer calls mark(): a probe result drives the guard state machine, while mark()
// stays reserved for failures of real user traffic.
func (p *Pool) Check(ctx context.Context, raw string) (time.Duration, error) {
	p.mu.Lock()
	var e *poolEntry
	for _, item := range p.entries {
		if item.raw == raw {
			e = item
			break
		}
	}
	p.mu.Unlock()
	if e == nil {
		return 0, http.ErrNoLocation
	}
	result := probeExit(ctx, tunnelDialer(e.clients), defaultProbeTimeout)
	p.applyProbe(e, result, time.Now(), guardTimingFromEnv())
	logProbe(raw, result)
	return result.l2Latency, result.err
}

func logProbe(raw string, result probeResult) {
	if result.err != nil {
		log.Printf("proxy probe fail proxy=%s l2=%d ws=%d latency=%s err=%v",
			redactProxy(raw), result.l2Code, result.wsCode, result.l2Latency, result.err)
		return
	}
	log.Printf("proxy probe pass proxy=%s l2=%d ws=%d latency=%s",
		redactProxy(raw), result.l2Code, result.wsCode, result.l2Latency)
}

// checkAllBudget is the wall clock a manual "check every exit" run may take. It
// exists so the admin handler returns on a predictable schedule instead of being
// bounded only by the slowest exit in the pool.
const checkAllBudget = 30 * time.Second

// checkAllMaxConcurrency 是手工检查的并发上限。后台守护刻意保守（默认 6）以
// 免持续压微软，但手工检查是一次性的前台操作，用户在等结果，所以放宽。
const checkAllMaxConcurrency = 48

// CheckAll probes every exit. Probes run concurrently under the same cap the
// background guard uses; the previous sequential loop spent 10s per exit and
// made the admin handler hang on a large pool.
func (p *Pool) CheckAll(ctx context.Context) []map[string]any {
	return p.CheckSelected(ctx, nil)
}

// CheckSelected 只探测 ids 指定的出口；ids 为空时退化为全量探测。
//
// 为什么需要它：新增出口后前端过去调的是全量检查，于是
//   - 每次加一个 IP 都把池里所有老 IP 重新探一遍（用户可见的「触发老IP重新检测」）；
//   - 30s 总预算被老出口占满，新出口常常轮不到，列表里反而没有状态；
//   - 池子越大越慢。
// 按 id 定向探测把这三件事一起解决：只探新加的那几个。
func (p *Pool) CheckSelected(ctx context.Context, ids []string) []map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancelAll := context.WithTimeout(ctx, checkAllBudget)
	defer cancelAll()

	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}

	p.mu.Lock()
	targets := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if len(wanted) == 0 {
			targets = append(targets, e)
			continue
		}
		if _, ok := wanted[e.id()]; ok {
			targets = append(targets, e)
		}
	}
	p.mu.Unlock()

	timing := guardTimingFromEnv()
	// 手工检查是用户在等结果的前台操作，可以比后台守护更激进：探测是纯网络
	// 等待，几乎不吃 CPU。并发取「出口数」与上限的较小值，小批量时一轮打完。
	limit := guardConcurrency()
	if n := len(targets); n > limit {
		if n > checkAllMaxConcurrency {
			n = checkAllMaxConcurrency
		}
		limit = n
	}
	semaphore := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, entry := range targets {
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
			result := probeExit(probeCtx, tunnelDialer(e.clients), defaultProbeTimeout)
			p.applyProbe(e, result, time.Now(), timing)
			logProbe(e.raw, result)
		}(entry)
	}
	wg.Wait()
	if len(wanted) == 0 {
		return p.List()
	}
	// 定向检查只返回被探测的那几条。返回全池会让前端把没探过的老出口一起
	// 重新渲染，"新 IP 没状态、老 IP 状态变了" 就是这么来的。
	filtered := make([]map[string]any, 0, len(wanted))
	for _, item := range p.List() {
		if id, _ := item["id"].(string); id != "" {
			if _, ok := wanted[id]; ok {
				filtered = append(filtered, item)
			}
		}
	}
	return filtered
}

// probeRoundBudget is the wall clock a full round may take: three sequential
// requests plus a little slack.
func probeRoundBudget(timeout time.Duration) time.Duration {
	return 3*timeout + timeout/2
}
