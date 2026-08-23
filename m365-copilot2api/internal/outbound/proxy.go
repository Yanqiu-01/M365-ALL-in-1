package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
	EnvProxy                       = "M365_OUTBOUND_PROXY"
	EnvOutboundMaxIdleConns        = "M365_OUTBOUND_MAX_IDLE_CONNS"
	EnvOutboundMaxIdleConnsPerHost = "M365_OUTBOUND_MAX_IDLE_CONNS_PER_HOST"
	EnvOutboundMaxConnsPerHost     = "M365_OUTBOUND_MAX_CONNS_PER_HOST"
	EnvOutboundHTTPTimeoutSeconds  = "M365_OUTBOUND_HTTP_TIMEOUT_SECONDS"
	EnvOutboundMaxWebSockets       = "M365_OUTBOUND_MAX_WEBSOCKETS_PER_PROXY"

	defaultOutboundMaxIdleConns        = 512
	defaultOutboundMaxIdleConnsPerHost = 128
	defaultOutboundMaxConnsPerHost     = 128
	defaultOutboundHTTPTimeoutSeconds  = 45
	defaultOutboundMaxWebSockets       = 64
)

func outboundIntEnv(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return fallback
	}
	return value
}

func outboundWebSocketLimit() int {
	return outboundIntEnv(EnvOutboundMaxWebSockets, defaultOutboundMaxWebSockets, 1, 128)
}

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
	maxIdle := outboundIntEnv(EnvOutboundMaxIdleConns, defaultOutboundMaxIdleConns, 1, 4096)
	maxIdlePerHost := outboundIntEnv(EnvOutboundMaxIdleConnsPerHost, defaultOutboundMaxIdleConnsPerHost, 1, 1024)
	if maxIdlePerHost > maxIdle {
		maxIdlePerHost = maxIdle
	}
	maxConnsPerHost := outboundIntEnv(EnvOutboundMaxConnsPerHost, defaultOutboundMaxConnsPerHost, 1, 512)
	httpTimeout := time.Duration(outboundIntEnv(EnvOutboundHTTPTimeoutSeconds, defaultOutboundHTTPTimeoutSeconds, 5, 300)) * time.Second
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Clients{
		HTTP: &http.Client{Transport: t, Timeout: httpTimeout},
		WebSocket: &websocket.Dialer{
			// Per-attempt dial/handshake budget. chathub retries the payload-free
			// setup twice, so this caps one setup round at ~24s and lets the web
			// layer's account/exit failover run several times inside the overall
			// chatTimeoutSeconds budget instead of burning it on one dead exit.
			HandshakeTimeout: 12 * time.Second,
			ReadBufferSize:   1024 * 1024,
			WriteBufferSize:  64 * 1024,
			NetDialContext:   t.DialContext,
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
	if proxyPool != nil {
		// Drain the live pool before detaching it so a reference taken earlier stops
		// serving exits that are no longer configured.
		proxyPool.adoptEntries(&Pool{wsLimit: outboundWebSocketLimit()})
	}
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
	if proxyPool != nil {
		// Update the live pool in place instead of swapping the pointer. Anything
		// that already holds this *Pool - a dialer closure, an in-flight round
		// tripper - must observe the new exit list. Replacing the pointer left such
		// references pinned to the previous object, so deleted exits kept being
		// dialed until the process restarted.
		proxyPool.adoptEntries(p)
	} else {
		proxyPool = p
	}
	clientsMu.Unlock()
	return nil
}

// adoptEntries makes p serve exactly the exits of next. Entries whose URL
// survives the edit are reused, so their health, cooldown, sticky binding and
// in-flight WebSocket accounting are not silently reset by an unrelated add or
// remove. It lives next to ConfigurePool because it exists only to support live
// reconfiguration.
func (p *Pool) adoptEntries(next *Pool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing := make(map[string]*poolEntry, len(p.entries))
	for _, entry := range p.entries {
		existing[entry.raw] = entry
	}
	entries := make([]*poolEntry, 0, len(next.entries))
	kept := make(map[string]bool, len(next.entries))
	for _, entry := range next.entries {
		if reused, ok := existing[entry.raw]; ok {
			entry = reused
		}
		entries = append(entries, entry)
		kept[entry.raw] = true
	}
	p.entries = entries
	p.wsLimit = next.wsLimit
	if len(p.entries) == 0 {
		p.next = 0
	} else {
		p.next %= len(p.entries)
	}
	for account, raw := range p.sticky {
		if !kept[raw] {
			delete(p.sticky, account)
		}
	}
	// Wake anyone waiting for WebSocket capacity: the exit list changed, so the
	// waiter has to re-evaluate, including falling back to a direct dial once the
	// pool is empty.
	p.signalWebSocketWaitersLocked()
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

// redactProxyDisplay renders an exit for human/UI consumption. Unlike
// redactProxy (which drops userinfo entirely for log lines) it keeps the
// username visible so operators can still tell two exits on the same host
// apart, while the password is replaced by a fixed placeholder.
func redactProxyDisplay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<invalid>"
	}
	if u.User == nil {
		return u.String()
	}
	password, hasPassword := u.User.Password()
	if !hasPassword || password == "" {
		return u.String()
	}
	// Built by hand: url.URL.String() percent-encodes the placeholder into
	// %2A%2A%2A, which is neither readable nor stable to match on.
	return u.Scheme + "://" + u.User.Username() + ":" + proxyPasswordPlaceholder + "@" + u.Host
}

const proxyPasswordPlaceholder = "***"

// sameProxyURL reports whether candidate identifies entry, accepting either the
// raw URL or the redacted display form. The admin UI round-trips whatever it was
// shown, so DELETE ?url=... must keep working once the GET body is redacted.
func sameProxyURL(entry, candidate string) bool {
	normalize := func(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }
	entry, candidate = normalize(entry), normalize(candidate)
	if entry == candidate {
		return true
	}
	return normalize(redactProxyDisplay(entry)) == candidate
}

// ProxyPoolRawURLs returns the exits verbatim, credentials included. It exists
// for the settings persistence path, which must round-trip a usable URL. Never
// route this into an API response, a log line, or an error message.
func ProxyPoolRawURLs() []string {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return []string{}
	}
	return p.rawURLs()
}

// ProxyPoolStatusRedacted is ProxyPoolStatus with the password of every exit
// replaced by a placeholder. This is what belongs in an admin API response:
// ProxyPoolStatus reports the raw URL, so serving it directly puts the proxy
// password into the dashboard HTML and the browser devtools network tab.
func ProxyPoolStatusRedacted() []map[string]any {
	items := ProxyPoolStatus()
	for _, item := range items {
		if raw, ok := item["url"].(string); ok {
			item["url"] = redactProxyDisplay(raw)
		}
	}
	return items
}

func AddProxy(raw string) error {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return ConfigurePool([]string{raw})
	}
	return ConfigurePool(append(p.rawURLs(), raw))
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
	for _, v := range p.rawURLs() {
		// Accept the redacted form too: the dashboard deletes by the URL it was
		// shown, and that URL no longer carries the password.
		if !found && sameProxyURL(v, raw) {
			found = true
			continue
		}
		items = append(items, v)
	}
	if !found {
		// redactProxy keeps a mistyped or stale credentialed URL out of the admin
		// API error body, which the dashboard renders verbatim.
		return fmt.Errorf("proxy not found: %s", redactProxy(raw))
	}
	return ConfigurePool(items)
}

// HTTPClient and WebSocketDialer resolve the current outbound configuration.
// Callers must invoke them per request / per dial and must not cache the result:
// the proxy pool is edited at runtime through the admin API.
func HTTPClient() *http.Client {
	clientsMu.RLock()
	p, c := proxyPool, clients.HTTP
	clientsMu.RUnlock()
	if p != nil && p.size() > 0 {
		return p.HTTPClient()
	}
	return c
}
func WebSocketDialer() *websocket.Dialer {
	clientsMu.RLock()
	p, c := proxyPool, clients.WebSocket
	clientsMu.RUnlock()
	if p != nil && p.size() > 0 {
		return p.WebSocketDialer()
	}
	// An empty pool is a direct exit. Reuse the shared direct transport rather than
	// a pool wrapper that would build a fresh transport per dial and lose
	// connection reuse.
	d := *c
	return &d
}

func (p *Pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
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
		// Keep proxy selection in NetDialContext so a shared Dialer can use a
		// different pool exit for every WebSocket connection.
		c.WebSocket.Proxy = nil
		c.WebSocket.NetDialContext = httpProxyDialer{proxyURL: u}.DialContext
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
		x := socksContextDialer{dialer: d}
		c.HTTP.Transport.(*http.Transport).DialContext = x.DialContext
		c.WebSocket.NetDialContext = x.DialContext
	default:
		return nil, fmt.Errorf("outbound proxy scheme %q is unsupported; use socks5, http, or https", u.Scheme)
	}
	return c, nil
}

// setProxyAuthorization writes the proxy credentials onto a hand-rolled CONNECT
// request. It must be Proxy-Authorization, not Authorization: http.Request's
// SetBasicAuth sets the latter, which a proxy ignores, so every CONNECT came
// back 407 Proxy Authentication Required on exits configured with userinfo.
func setProxyAuthorization(header http.Header, proxyURL *url.URL) {
	if header == nil || proxyURL == nil || proxyURL.User == nil {
		return
	}
	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	if username == "" && password == "" {
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	header.Set("Proxy-Authorization", "Basic "+encoded)
}

type httpProxyDialer struct{ proxyURL *url.URL }

func (d httpProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("HTTP proxy only supports tcp, got %q", network)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	proxyAddress := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		proxyAddress = net.JoinHostPort(d.proxyURL.Hostname(), "80")
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, proxyAddress)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	defer conn.SetDeadline(time.Time{})
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	setProxyAuthorization(req.Header, d.proxyURL)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("HTTP proxy CONNECT %s: %s", address, resp.Status)
	}
	resp.Body.Close()
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type httpsProxyDialer struct{ proxyURL *url.URL }

func (d httpsProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("HTTPS proxy only supports tcp, got %q", network)
	}
	a := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		a = net.JoinHostPort(d.proxyURL.Hostname(), "443")
	}
	raw, e := (&net.Dialer{}).DialContext(ctx, network, a)
	if e != nil {
		return nil, e
	}
	// Proxy endpoints commonly present a certificate for their hostname while users
	// configure an IP address. This option affects only the TLS hop to the proxy;
	// target-site certificate verification remains enabled.
	insecureProxyTLS := os.Getenv("M365_PROXY_INSECURE_TLS") == "1" || os.Getenv("M365_PROXY_INSECURE_TLS") == "true" || net.ParseIP(d.proxyURL.Hostname()) != nil
	conn := tls.Client(raw, &tls.Config{ServerName: d.proxyURL.Hostname(), MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureProxyTLS}) // #nosec G402 -- explicitly scoped to configured proxy TLS

	if e = conn.HandshakeContext(ctx); e != nil {
		raw.Close()
		return nil, e
	}
	q := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	setProxyAuthorization(q.Header, d.proxyURL)
	if e = q.Write(conn); e != nil {
		conn.Close()
		return nil, e
	}
	rd := bufio.NewReader(conn)
	resp, e := http.ReadResponse(rd, q)
	if e != nil {
		conn.Close()
		return nil, e
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("HTTPS proxy CONNECT %s: %s", address, resp.Status)
	}
	return &bufferedConn{Conn: conn, reader: rd}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type socksContextDialer struct{ dialer proxy.Dialer }

func (d socksContextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ch := make(chan struct {
		c net.Conn
		e error
	}, 1)
	go func() {
		c, e := d.dialer.Dial(network, address)
		ch <- struct {
			c net.Conn
			e error
		}{c, e}
	}()
	select {
	case r := <-ch:
		return r.c, r.e
	case <-ctx.Done():
		go func() {
			r := <-ch
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
