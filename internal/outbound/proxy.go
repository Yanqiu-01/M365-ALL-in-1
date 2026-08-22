package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
	EnvProxy            = "M365_OUTBOUND_PROXY"
	proxyConnectTimeout = 30 * time.Second
)

type Clients struct {
	HTTP      *http.Client
	WebSocket *websocket.Dialer
}

var (
	clientsMu sync.RWMutex
	clients   = directClients()
	proxyPool *Pool
)

func directClients() *Clients {
	tlsCache := tls.NewLRUClientSessionCache(32)
	httpTLSConf := &tls.Config{ClientSessionCache: tlsCache}
	wsTLSConf := &tls.Config{ClientSessionCache: tlsCache, NextProtos: []string{"http/1.1"}}
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       httpTLSConf,
	}
	return &Clients{
		HTTP: &http.Client{Transport: t},
		WebSocket: &websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			ReadBufferSize:   256 * 1024,
			WriteBufferSize:  16 * 1024,
			NetDialContext:   t.DialContext,
			TLSClientConfig:  wsTLSConf,
		},
	}
}
func ConfigureFromEnv() error {
	raw := strings.TrimSpace(os.Getenv("M365_PROXY_POOL"))
	if raw != "" {
		return ConfigurePool(strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }))
	}
	return Configure(os.Getenv(EnvProxy))
}
func Configure(raw string) error {
	c, e := New(raw)
	if e != nil {
		return e
	}
	clientsMu.Lock()
	clients = c
	proxyPool = nil
	clientsMu.Unlock()
	return nil
}
func ConfigurePool(raw []string) error {
	p, e := NewPool(raw)
	if e != nil {
		return e
	}
	clientsMu.Lock()
	proxyPool = p
	clientsMu.Unlock()
	return nil
}

func CurrentPool() *Pool { clientsMu.RLock(); defer clientsMu.RUnlock(); return proxyPool }

func ProxyPoolStatus() []map[string]any {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return []map[string]any{}
	}
	return p.List()
}

func AddProxy(raw string) error {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return ConfigurePool([]string{raw})
	}
	items := make([]string, 0)
	for _, item := range p.List() {
		if v, ok := item["url"].(string); ok {
			items = append(items, v)
		}
	}
	items = append(items, raw)
	return ConfigurePool(items)
}

func RemoveProxy(raw string) error {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return nil
	}
	items := make([]string, 0)
	found := false
	for _, item := range p.List() {
		if v, ok := item["url"].(string); ok {
			if strings.TrimRight(strings.TrimSpace(v), "/") == raw {
				found = true
				continue
			}
			items = append(items, v)
		}
	}
	if !found {
		return fmt.Errorf("proxy not found: %s", raw)
	}
	return ConfigurePool(items)
}
func HTTPClient() *http.Client {
	clientsMu.RLock()
	p, c := proxyPool, clients.HTTP
	clientsMu.RUnlock()
	if p != nil {
		return p.HTTPClient()
	}
	return c
}
func WebSocketDialer() *websocket.Dialer {
	clientsMu.RLock()
	p, c := proxyPool, clients.WebSocket
	clientsMu.RUnlock()
	if p != nil {
		return p.WebSocketDialer()
	}
	d := *c
	return &d
}
func ValidateProxyURL(raw string) error { _, e := New(raw); return e }
func New(raw string) (*Clients, error) {
	c := directClients()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c, nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return nil, fmt.Errorf("outbound proxy must be a complete socks5://, http://, or https:// URL")
	}
	if u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("outbound proxy URL must not include a path, query, or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		c.HTTP.Transport.(*http.Transport).Proxy = http.ProxyURL(u)
		c.WebSocket.Proxy = http.ProxyURL(u)
	case "https":
		// Do not use Transport.Proxy here: Go's standard transport performs its
		// own proxy TLS handshake and bypasses our IP-certificate compatibility.
		transport := c.HTTP.Transport.(*http.Transport)
		transport.Proxy = nil
		transport.DialContext = httpsProxyDialer{proxyURL: u}.DialContext
		c.WebSocket.NetDialContext = httpsProxyDialer{proxyURL: u}.DialContext
	case "socks5":
		var a *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			a = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, e := proxy.SOCKS5("tcp", u.Host, a, proxy.Direct)
		if e != nil {
			return nil, fmt.Errorf("configure SOCKS5 proxy: %w", e)
		}
		contextDialer, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("configured SOCKS5 dialer does not support context cancellation")
		}
		c.HTTP.Transport.(*http.Transport).DialContext = contextDialer.DialContext
		c.WebSocket.NetDialContext = contextDialer.DialContext
	default:
		return nil, fmt.Errorf("outbound proxy scheme %q is unsupported; use socks5, http, or https", u.Scheme)
	}
	return c, nil
}

type httpsProxyDialer struct{ proxyURL *url.URL }

func (d httpsProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if network != "tcp" {
		return nil, fmt.Errorf("HTTPS proxy only supports tcp, got %q", network)
	}
	a := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		a = net.JoinHostPort(d.proxyURL.Hostname(), "443")
	}
	raw, e := (&net.Dialer{Timeout: proxyConnectTimeout}).DialContext(ctx, network, a)
	if e != nil {
		return nil, e
	}
	insecureProxyTLS := os.Getenv("M365_PROXY_INSECURE_TLS") == "1" || os.Getenv("M365_PROXY_INSECURE_TLS") == "true" || net.ParseIP(d.proxyURL.Hostname()) != nil
	conn := tls.Client(raw, &tls.Config{ServerName: d.proxyURL.Hostname(), MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureProxyTLS}) // #nosec G402 -- explicitly scoped to configured proxy TLS
	deadline := time.Now().Add(proxyConnectTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if e = conn.SetDeadline(deadline); e != nil {
		_ = raw.Close()
		return nil, e
	}
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopContextClose()
	if e = conn.HandshakeContext(ctx); e != nil {
		_ = conn.Close()
		return nil, e
	}
	q := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.proxyURL.User != nil {
		pw, _ := d.proxyURL.User.Password()
		q.SetBasicAuth(d.proxyURL.User.Username(), pw)
	}
	if e = q.Write(conn); e != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, e
	}
	rd := bufio.NewReader(conn)
	resp, e := http.ReadResponse(rd, q)
	if e != nil {
		closeResponseBody(resp)
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, e
	}
	if resp == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTPS proxy CONNECT %s returned no response", address)
	}
	if resp.StatusCode != http.StatusOK {
		closeResponseBody(resp)
		_ = conn.Close()
		return nil, fmt.Errorf("HTTPS proxy CONNECT %s: %s", address, resp.Status)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = conn.Close()
		return nil, ctxErr
	}
	if !stopContextClose() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = conn.Close()
			return nil, ctxErr
		}
	}
	if e = conn.SetDeadline(time.Time{}); e != nil {
		_ = conn.Close()
		return nil, e
	}
	return &bufferedConn{Conn: conn, reader: rd}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
