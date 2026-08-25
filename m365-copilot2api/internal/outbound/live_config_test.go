package outbound_test

// 这些测试守住一条不变量：出口（代理池）配置必须在每次拨号时实时求值。
// 历史 bug 是 chathub.NewClient() 在构造时把 outbound.WebSocketDialer() /
// outbound.HTTPClient() 的结果快照进字段，而 outbound.ConfigurePool() 整体
// 替换 proxyPool 指针，于是被缓存的 dialer 永远绑在启动时那个 Pool 对象上：
// 代理池增删改（包括清空）对 chat 路径完全失效，直到进程重启。

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"

	"github.com/gorilla/websocket"
)

// fakeProxy 是一个只会用 400 拒绝 CONNECT 的监听器，并统计收到的 TCP 连接数。
// 拨号打到它身上，就证明这个出口被真正使用了。
type fakeProxy struct {
	listener net.Listener
	dials    atomic.Int64
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			p.dials.Add(1)
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || strings.TrimSpace(line) == "" {
						break
					}
				}
				_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return p
}

func (p *fakeProxy) url() string  { return "http://" + p.listener.Addr().String() }
func (p *fakeProxy) count() int64 { return p.dials.Load() }

// directTarget 代表"直连"目标：只有绕过代理池的拨号才会到达它。
type directTarget struct {
	listener net.Listener
	dials    atomic.Int64
}

func newDirectTarget(t *testing.T) *directTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := &directTarget{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			target.dials.Add(1)
			go func() {
				<-time.After(2 * time.Second)
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return target
}

func (t *directTarget) addr() string { return t.listener.Addr().String() }
func (t *directTarget) count() int64 { return t.dials.Load() }

// awaitCount waits for the accept loop to catch up: a dial returns as soon as
// the TCP handshake completes, which can be before Accept runs.
func (target *directTarget) awaitCount(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if target.count() >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := target.count(); got != want {
		t.Fatalf("direct connections = %d, want %d", got, want)
	}
}

// settle gives a stray dial a chance to show up before asserting it never
// happened, so a "no proxy was used" assertion cannot pass by winning a race.
func settle() { time.Sleep(50 * time.Millisecond) }

func restorePool(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = outbound.ConfigurePool(nil) })
}

// dialWith 走 chathub.Client 实际使用的那条出口解析路径。
func dialWith(client *chathub.Client, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer := client.WebSocketDialer()
	if dialer == nil || dialer.NetDialContext == nil {
		return nil, errors.New("websocket dialer has no NetDialContext")
	}
	return dialer.NetDialContext(ctx, "tcp", address)
}

// 本 bug 的回归测试：池被清空后，同一个 Client 实例必须改走直连，
// 不能再碰已删除的代理。
func TestClearedProxyPoolStopsBeingUsedByExistingClient(t *testing.T) {
	restorePool(t)
	proxyA, proxyB := newFakeProxy(t), newFakeProxy(t)
	target := newDirectTarget(t)

	if err := outbound.ConfigurePool([]string{proxyA.url(), proxyB.url()}); err != nil {
		t.Fatal(err)
	}
	client := chathub.NewClient()

	if conn, err := dialWith(client, target.addr()); err == nil {
		_ = conn.Close()
		t.Fatal("dial through the rejecting proxy pool unexpectedly succeeded")
	}
	used := proxyA.count() + proxyB.count()
	if used == 0 {
		t.Fatal("first dial did not leave through the configured proxy pool")
	}
	if target.count() != 0 {
		t.Fatalf("first dial bypassed the pool: direct connections = %d", target.count())
	}

	if err := outbound.ConfigurePool(nil); err != nil {
		t.Fatal(err)
	}
	conn, err := dialWith(client, target.addr())
	if err != nil {
		t.Fatalf("dial after clearing the pool did not fall back to direct: %v", err)
	}
	_ = conn.Close()
	settle()
	if got := proxyA.count() + proxyB.count(); got != used {
		t.Fatalf("dial after clearing the pool still reached a deleted proxy: %d -> %d", used, got)
	}
	target.awaitCount(t, 1)
}

// 池从 A 换到 B 后，后续拨号必须使用 B，且不再触碰 A。
func TestReplacedProxyPoolUsesTheNewExit(t *testing.T) {
	restorePool(t)
	proxyA, proxyB := newFakeProxy(t), newFakeProxy(t)
	target := newDirectTarget(t)

	if err := outbound.ConfigurePool([]string{proxyA.url()}); err != nil {
		t.Fatal(err)
	}
	client := chathub.NewClient()
	if conn, err := dialWith(client, target.addr()); err == nil {
		_ = conn.Close()
		t.Fatal("dial through pool A unexpectedly succeeded")
	}
	if proxyA.count() != 1 || proxyB.count() != 0 {
		t.Fatalf("pool A dial counts = A:%d B:%d, want 1/0", proxyA.count(), proxyB.count())
	}

	if err := outbound.ConfigurePool([]string{proxyB.url()}); err != nil {
		t.Fatal(err)
	}
	if conn, err := dialWith(client, target.addr()); err == nil {
		_ = conn.Close()
		t.Fatal("dial through pool B unexpectedly succeeded")
	}
	if proxyA.count() != 1 || proxyB.count() != 1 {
		t.Fatalf("pool B dial counts = A:%d B:%d, want 1/1", proxyA.count(), proxyB.count())
	}
}

// 空池在 ws 与 http 两侧都必须回落直连。
func TestEmptyPoolFallsBackToDirectOnBothTransports(t *testing.T) {
	restorePool(t)
	proxy := newFakeProxy(t)
	target := newDirectTarget(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	if err := outbound.ConfigurePool([]string{proxy.url()}); err != nil {
		t.Fatal(err)
	}
	client := chathub.NewClient()
	if err := outbound.ConfigurePool([]string{}); err != nil {
		t.Fatal(err)
	}

	conn, err := dialWith(client, target.addr())
	if err != nil {
		t.Fatalf("websocket dial with an empty pool must be direct: %v", err)
	}
	_ = conn.Close()
	target.awaitCount(t, 1)

	resp, err := client.OutboundHTTPClient().Get(server.URL)
	if err != nil {
		t.Fatalf("http request with an empty pool must be direct: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http status = %d, want 200", resp.StatusCode)
	}
	settle()
	if proxy.count() != 0 {
		t.Fatalf("empty pool still dialed a proxy %d times", proxy.count())
	}
}

// ConfigurePool 必须就地更新活跃的 Pool，而不是替换指针：任何已经持有该
// *Pool 的引用（拨号闭包、在途 RoundTripper）都必须看到新的出口列表。
func TestConfigurePoolUpdatesTheLivePoolObject(t *testing.T) {
	restorePool(t)
	if err := outbound.ConfigurePool([]string{"http://first.example:8080"}); err != nil {
		t.Fatal(err)
	}
	held := outbound.CurrentPool()
	if held == nil {
		t.Fatal("CurrentPool returned nil after configuring a pool")
	}

	if err := outbound.ConfigurePool([]string{"http://second.example:8080"}); err != nil {
		t.Fatal(err)
	}
	if outbound.CurrentPool() != held {
		t.Fatal("ConfigurePool replaced the pool object; references captured earlier stay pinned to the old exits")
	}
	items := held.List()
	if len(items) != 1 || items[0]["url"] != "http://second.example:8080" {
		t.Fatalf("previously held pool = %#v, want only the new exit", items)
	}

	if err := outbound.ConfigurePool(nil); err != nil {
		t.Fatal(err)
	}
	if got := held.List(); len(got) != 0 {
		t.Fatalf("previously held pool after clearing = %#v, want empty", got)
	}
}

// 注入路径必须保留：显式赋值的 Dialer / HTTPClient 优先于实时配置，
// 否则测试无法注入 mock 出口。
func TestInjectedTransportsOverrideLiveConfiguration(t *testing.T) {
	restorePool(t)
	proxy := newFakeProxy(t)
	if err := outbound.ConfigurePool([]string{proxy.url()}); err != nil {
		t.Fatal(err)
	}

	dialed := 0
	injectedDialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		dialed++
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return client, nil
	}}
	injectedHTTP := &http.Client{}

	client := chathub.NewClient()
	client.Dialer = injectedDialer
	client.HTTPClient = injectedHTTP

	if client.WebSocketDialer() != injectedDialer {
		t.Fatal("injected websocket dialer was ignored")
	}
	if client.OutboundHTTPClient() != injectedHTTP {
		t.Fatal("injected http client was ignored")
	}
	conn, err := dialWith(client, "upstream.test:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	settle()
	if dialed != 1 || proxy.count() != 0 {
		t.Fatalf("injected dialer calls = %d, proxy dials = %d, want 1/0", dialed, proxy.count())
	}
}
