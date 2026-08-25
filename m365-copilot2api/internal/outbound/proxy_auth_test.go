package outbound_test

// 回归测试：带 userinfo 的出口代理，其凭据必须真正出现在 CONNECT 报文的
// Proxy-Authorization 头里。历史 bug 是 httpProxyDialer / httpsProxyDialer 用
// req.SetBasicAuth() 写凭据，而 SetBasicAuth 写的是 Authorization 头 —— 代理
// 服务端只认 Proxy-Authorization，于是 WebSocket 拨号全部拿到
// 407 Proxy Authentication Required。
//
// 这里的假代理强制要求 Proxy-Authorization，缺失即回 407，所以修复前必红。

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/outbound"

	"github.com/gorilla/websocket"
)

const (
	authProxyUser = "m365exit"
	authProxyPass = "s3cr3t-pa55word"
)

func expectedProxyAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(authProxyUser+":"+authProxyPass))
}

// authProxy 是一个要求 Basic 代理认证的假 HTTP/HTTPS 代理。CONNECT 会被真正
// 打通到目标地址，所以隧道之后可以跑完整的 WebSocket 握手。
type authProxy struct {
	server     *httptest.Server
	connects   atomic.Int64
	rejected   atomic.Int64
	authorized atomic.Int64
}

func (p *authProxy) handler() http.Handler {
	want := expectedProxyAuthorization()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != want {
			p.rejected.Add(1)
			w.Header().Set("Proxy-Authenticate", `Basic realm="m365"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		p.authorized.Add(1)
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "forwarded")
			return
		}
		p.connects.Add(1)
		p.tunnel(w, r)
	})
}

func (p *authProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	go func() {
		defer func() { _ = upstream.Close() }()
		_, _ = io.Copy(upstream, buffered)
	}()
	go func() {
		defer func() { _ = client.Close() }()
		_, _ = io.Copy(client, upstream)
	}()
}

func newAuthProxy(t *testing.T) *authProxy {
	t.Helper()
	p := &authProxy{}
	p.server = httptest.NewServer(p.handler())
	t.Cleanup(p.server.Close)
	return p
}

func newTLSAuthProxy(t *testing.T) *authProxy {
	t.Helper()
	p := &authProxy{}
	p.server = httptest.NewTLSServer(p.handler())
	t.Cleanup(p.server.Close)
	return p
}

func (p *authProxy) addr() string {
	return strings.TrimPrefix(strings.TrimPrefix(p.server.URL, "http://"), "https://")
}

func (p *authProxy) scheme() string {
	if strings.HasPrefix(p.server.URL, "https://") {
		return "https"
	}
	return "http"
}

func (p *authProxy) urlWithCredentials() string {
	return fmt.Sprintf("%s://%s:%s@%s", p.scheme(), authProxyUser, authProxyPass, p.addr())
}

func (p *authProxy) urlWithoutCredentials() string {
	return p.scheme() + "://" + p.addr()
}

// newWebSocketEcho 是隧道另一端的真实 WebSocket 服务端：拿到 101 才算凭据真的过了。
func newWebSocketEcho(t *testing.T) string {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws://" + strings.TrimPrefix(server.URL, "http://") + "/"
}

func resetPool(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = outbound.ConfigurePool(nil) })
}

func dialEchoThroughPool(t *testing.T, wsURL string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dialer := outbound.WebSocketDialer()
	if dialer == nil {
		t.Fatal("nil websocket dialer")
	}
	return dialer.DialContext(ctx, wsURL, nil)
}

func TestWebSocketDialThroughAuthenticatedHTTPProxy(t *testing.T) {
	resetPool(t)
	proxy := newAuthProxy(t)
	wsURL := newWebSocketEcho(t)
	if err := outbound.ConfigurePool([]string{proxy.urlWithCredentials()}); err != nil {
		t.Fatal(err)
	}
	conn, resp, err := dialEchoThroughPool(t, wsURL)
	if err != nil {
		t.Fatalf("ws dial through authenticated proxy failed: %v (proxy rejected=%d)", err, proxy.rejected.Load())
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ping" {
		t.Fatalf("echo = %q, want %q", data, "ping")
	}
	if proxy.connects.Load() == 0 {
		t.Fatal("proxy never tunneled a CONNECT: the exit was not used")
	}
	if got := proxy.rejected.Load(); got != 0 {
		t.Fatalf("proxy rejected %d requests for missing Proxy-Authorization", got)
	}
}

func TestWebSocketDialThroughAuthenticatedHTTPSProxy(t *testing.T) {
	resetPool(t)
	proxy := newTLSAuthProxy(t)
	wsURL := newWebSocketEcho(t)
	if err := outbound.ConfigurePool([]string{proxy.urlWithCredentials()}); err != nil {
		t.Fatal(err)
	}
	conn, resp, err := dialEchoThroughPool(t, wsURL)
	if err != nil {
		t.Fatalf("ws dial through authenticated https proxy failed: %v (proxy rejected=%d)", err, proxy.rejected.Load())
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got := proxy.rejected.Load(); got != 0 {
		t.Fatalf("proxy rejected %d requests for missing Proxy-Authorization", got)
	}
}

// 反向对照：不带凭据时必须真的拿到 407，证明假代理的认证是有效的。
func TestWebSocketDialWithoutCredentialsIsRejectedWith407(t *testing.T) {
	resetPool(t)
	proxy := newAuthProxy(t)
	wsURL := newWebSocketEcho(t)
	if err := outbound.ConfigurePool([]string{proxy.urlWithoutCredentials()}); err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialEchoThroughPool(t, wsURL)
	if err == nil {
		conn.Close()
		t.Fatal("expected 407 without credentials")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("error = %v, want a 407 proxy authentication failure", err)
	}
	if proxy.rejected.Load() == 0 {
		t.Fatal("proxy never rejected the unauthenticated CONNECT")
	}
	if proxy.connects.Load() != 0 {
		t.Fatal("proxy tunneled a CONNECT without credentials")
	}
}

func TestPooledHTTPClientSendsProxyAuthorization(t *testing.T) {
	resetPool(t)
	proxy := newAuthProxy(t)
	if err := outbound.ConfigurePool([]string{proxy.urlWithCredentials()}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://target.invalid/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := outbound.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("pooled http request failed: %v (proxy rejected=%d)", err, proxy.rejected.Load())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "forwarded" {
		t.Fatalf("status=%d body=%q, want 200 \"forwarded\"", resp.StatusCode, body)
	}
	if got := proxy.rejected.Load(); got != 0 {
		t.Fatalf("proxy rejected %d requests for missing Proxy-Authorization", got)
	}
}

// socksProxy 是一个只接受 RFC1929 用户名/密码认证的最小 SOCKS5 服务端。
type socksProxy struct {
	listener  net.Listener
	tunnels   atomic.Int64
	authFails atomic.Int64
}

func newSocksProxy(t *testing.T) *socksProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &socksProxy{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return p
}

func (p *socksProxy) addr() string { return p.listener.Addr().String() }

func (p *socksProxy) urlWithCredentials() string {
	return fmt.Sprintf("socks5://%s:%s@%s", authProxyUser, authProxyPass, p.addr())
}

func (p *socksProxy) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	offersUserPass := false
	for _, m := range methods {
		if m == 0x02 {
			offersUserPass = true
		}
	}
	if !offersUserPass {
		p.authFails.Add(1)
		_, _ = conn.Write([]byte{5, 0xff})
		return
	}
	if _, err := conn.Write([]byte{5, 0x02}); err != nil {
		return
	}
	user, pass, err := readSocksCredentials(conn)
	if err != nil {
		return
	}
	if user != authProxyUser || pass != authProxyPass {
		p.authFails.Add(1)
		_, _ = conn.Write([]byte{1, 1})
		return
	}
	if _, err := conn.Write([]byte{1, 0}); err != nil {
		return
	}
	target, err := readSocksTarget(conn)
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	p.tunnels.Add(1)
	_ = conn.SetDeadline(time.Time{})
	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func readSocksCredentials(conn net.Conn) (string, string, error) {
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(conn, prefix); err != nil {
		return "", "", err
	}
	if prefix[0] != 1 {
		return "", "", fmt.Errorf("unexpected auth version %d", prefix[0])
	}
	user := make([]byte, int(prefix[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return "", "", err
	}
	length := make([]byte, 1)
	if _, err := io.ReadFull(conn, length); err != nil {
		return "", "", err
	}
	pass := make([]byte, int(length[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return "", "", err
	}
	return string(user), string(pass), nil
}

func readSocksTarget(conn net.Conn) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return "", err
	}
	if head[0] != 5 || head[1] != 1 {
		return "", fmt.Errorf("unsupported socks request %v", head[:2])
	}
	var host string
	switch head[3] {
	case 1:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		host = net.IP(raw).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", err
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		host = string(raw)
	default:
		return "", fmt.Errorf("unsupported address type %d", head[3])
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprint(int(port[0])<<8|int(port[1]))), nil
}

func TestWebSocketDialThroughAuthenticatedSOCKS5Proxy(t *testing.T) {
	resetPool(t)
	proxy := newSocksProxy(t)
	wsURL := newWebSocketEcho(t)
	if err := outbound.ConfigurePool([]string{proxy.urlWithCredentials()}); err != nil {
		t.Fatal(err)
	}
	conn, resp, err := dialEchoThroughPool(t, wsURL)
	if err != nil {
		t.Fatalf("ws dial through authenticated socks5 proxy failed: %v (auth failures=%d)", err, proxy.authFails.Load())
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if proxy.tunnels.Load() == 0 {
		t.Fatal("socks5 proxy never tunneled: the exit was not used")
	}
	if got := proxy.authFails.Load(); got != 0 {
		t.Fatalf("socks5 proxy reported %d auth failures", got)
	}
}

// 凭据不得出现在任何错误消息里 —— 这些消息会被 admin API 原样回给前端。
func TestProxyErrorsDoNotLeakCredentials(t *testing.T) {
	resetPool(t)
	bad := fmt.Sprintf("ftp://%s:%s@127.0.0.1:3128", authProxyUser, authProxyPass)
	err := outbound.ConfigurePool([]string{bad})
	if err == nil {
		t.Fatal("expected unsupported scheme error")
	}
	if strings.Contains(err.Error(), authProxyPass) {
		t.Fatalf("ConfigurePool error leaked the proxy password: %v", err)
	}
	if err := outbound.ConfigurePool([]string{"http://127.0.0.1:3128"}); err != nil {
		t.Fatal(err)
	}
	missing := fmt.Sprintf("http://%s:%s@127.0.0.1:9", authProxyUser, authProxyPass)
	err = outbound.RemoveProxy(missing)
	if err == nil {
		t.Fatal("expected missing proxy error")
	}
	if strings.Contains(err.Error(), authProxyPass) {
		t.Fatalf("RemoveProxy error leaked the proxy password: %v", err)
	}
}
