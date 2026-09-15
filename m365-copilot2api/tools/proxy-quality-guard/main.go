// Command proxy-quality-guard is a standalone, dependency-free screen and scorer
// for M365 gateway egress proxies.
//
// It exists because the gateway's current health check (internal/outbound/health.go:47)
// probes http://www.msftconnecttest.com/connecttest.txt over plain HTTP. That target
// says nothing about whether an exit can carry the traffic the gateway actually needs:
//
//	auth plane      CONNECT + TLS to login.microsoftonline.com:443
//	business plane  CONNECT + TLS to substrate.office.com:443 and m365.cloud.microsoft:443
//	transport       a real WebSocket Upgrade to wss://substrate.office.com/m365Copilot/Chathub
//	                (the exact host+path from internal/chathub/client.go:62)
//
// A proxy that only speaks plain HTTP passes the current check and then fails every
// chat request. This tool checks the four real conditions instead, scores each exit,
// and emits the eviction/restore decision a background guard would make.
//
// It never sends credentials. An Upgrade answered with 400/401/403/404 is a PASS:
// it proves the tunnel carried a genuine WebSocket handshake and only credentials
// were missing. Proxy-level rejection, TLS failure, or a dead tunnel is a FAIL.
//
// Usage:
//
//	proxy-quality-guard -in candidates.txt [-out result.json] [-rounds 3]
//	                    [-interval 45s] [-conc 20] [-timeout 10s] [-min-score 0.60]
//
// Exit code is 0 when at least one exit is admitted, 1 otherwise, so it can gate
// an ingest step in a script.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	authHost = "login.microsoftonline.com"
	authPath = "/common/discovery/instance?api-version=1.1" +
		"&authorization_endpoint=https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	substrateHost = "substrate.office.com"
	m365Host      = "m365.cloud.microsoft"
	chathubPath   = "/m365Copilot/Chathub"
)

// Guard thresholds. These are the state-machine constants; see the design notes in
// docs (or the delivery report) for the rationale behind each value.
const (
	// EvictAfter consecutive probe failures move an exit to evicted.
	EvictAfter = 3
	// RestoreAfter consecutive probe successes move an evicted exit back to live.
	RestoreAfter = 2
	// WindowSize is the sliding window length for the success-rate term.
	WindowSize = 20
	// LatencyBudget is the latency that scores 0.5 on the latency term.
	LatencyBudget = 1500 * time.Millisecond
	// BaseInterval is the probe period for a live exit.
	BaseInterval = 60 * time.Second
	// MaxBackoff caps the reduced-frequency reprobe of an evicted exit.
	MaxBackoff = 15 * time.Minute
)

type roundResult struct {
	Round     int    `json:"round"`
	L2Code    int    `json:"l2Code"`
	L2Ms      int64  `json:"l2Ms"`
	Substrate bool   `json:"substrate"`
	SubMs     int64  `json:"substrateMs"`
	M365      bool   `json:"m365"`
	M365Ms    int64  `json:"m365Ms"`
	WS        bool   `json:"ws"`
	WSCode    int    `json:"wsCode"`
	WSNote    string `json:"wsNote"`
	WSMs      int64  `json:"wsMs"`
	Pass      bool   `json:"pass"`
	Err       string `json:"err,omitempty"`
}

type exitReport struct {
	Proxy      string        `json:"proxy"`
	Rounds     []roundResult `json:"rounds"`
	Passes     int           `json:"passes"`
	Attempts   int           `json:"attempts"`
	MedianL2Ms int64         `json:"medianL2Ms"`
	MedianWSMs int64         `json:"medianWsMs"`
	Score      float64       `json:"score"`
	State      string        `json:"state"`
	Admitted   bool          `json:"admitted"`
}

func main() {
	in := flag.String("in", "", "candidate file, one scheme://host:port per line")
	out := flag.String("out", "", "write JSON report to this path (default stdout summary only)")
	rounds := flag.Int("rounds", 3, "probe rounds per exit")
	interval := flag.Duration("interval", 45*time.Second, "sleep between rounds")
	conc := flag.Int("conc", 20, "max concurrent exits under probe (keep <=20 against Microsoft)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-probe timeout")
	minScore := flag.Float64("min-score", 0.60, "admit an exit only at or above this score")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}
	if *conc > 20 {
		fmt.Fprintf(os.Stderr, "clamping -conc %d to 20 to stay gentle on Microsoft\n", *conc)
		*conc = 20
	}
	cands, err := readCandidates(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read candidates: %v\n", err)
		os.Exit(2)
	}
	if len(cands) == 0 {
		fmt.Fprintln(os.Stderr, "no candidates")
		os.Exit(1)
	}

	reports := map[string]*exitReport{}
	for _, c := range cands {
		reports[c] = &exitReport{Proxy: c, State: "probing"}
	}

	alive := cands
	for r := 1; r <= *rounds; r++ {
		fmt.Fprintf(os.Stderr, "round %d/%d over %d exits\n", r, *rounds, len(alive))
		results := probeAll(alive, r, *conc, *timeout)
		var next []string
		for _, res := range results {
			rep := reports[res.proxy]
			rep.Rounds = append(rep.Rounds, res.rr)
			rep.Attempts++
			if res.rr.Pass {
				rep.Passes++
				next = append(next, res.proxy)
			}
		}
		// Only exits that passed continue; a single failure disqualifies for the
		// "all rounds green" requirement, and reprobing losers wastes Microsoft calls.
		alive = next
		if len(alive) == 0 {
			fmt.Fprintln(os.Stderr, "no exit survived; stopping early")
			break
		}
		if r < *rounds {
			time.Sleep(*interval)
		}
	}

	final := make([]*exitReport, 0, len(reports))
	for _, rep := range reports {
		rep.MedianL2Ms = medianOf(rep.Rounds, func(x roundResult) int64 { return x.L2Ms })
		rep.MedianWSMs = medianOf(rep.Rounds, func(x roundResult) int64 { return x.WSMs })
		rep.Score = Score(rep.Passes, rep.Attempts, time.Duration(rep.MedianL2Ms)*time.Millisecond)
		switch {
		case rep.Passes == *rounds && rep.Attempts == *rounds:
			rep.State = "live"
		case rep.Passes == 0:
			rep.State = "evicted"
		default:
			rep.State = "suspect"
		}
		rep.Admitted = rep.State == "live" && rep.Score >= *minScore
		final = append(final, rep)
	}
	sort.Slice(final, func(i, j int) bool {
		if final[i].Score != final[j].Score {
			return final[i].Score > final[j].Score
		}
		return final[i].MedianL2Ms < final[j].MedianL2Ms
	})

	admitted := 0
	fmt.Printf("%-38s %-8s %6s %6s %7s %s\n", "proxy", "state", "pass", "l2ms", "score", "admit")
	for _, rep := range final {
		if rep.Admitted {
			admitted++
		}
		fmt.Printf("%-38s %-8s %3d/%-3d %6d %7.3f %v\n",
			rep.Proxy, rep.State, rep.Passes, rep.Attempts, rep.MedianL2Ms, rep.Score, rep.Admitted)
	}
	fmt.Printf("\ncandidates=%d admitted=%d (min-score %.2f, %d rounds)\n",
		len(cands), admitted, *minScore, *rounds)

	if *out != "" {
		blob, _ := json.MarshalIndent(final, "", "  ")
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		}
	}
	if admitted == 0 {
		os.Exit(1)
	}
}

// Score is the selection weight for an exit.
//
//	score = 0.60*successRate + 0.25*latencyTerm + 0.15*wsTerm
//	latencyTerm = budget / (budget + medianLatency)   (0.5 at exactly the budget)
//	wsTerm      = 1 when the most recent round carried a WebSocket Upgrade, else 0
//
// successRate uses a Laplace-smoothed ratio so a single lucky probe cannot score 1.0.
func Score(passes, attempts int, medianLatency time.Duration) float64 {
	if attempts <= 0 {
		return 0
	}
	success := float64(passes+1) / float64(attempts+2)
	lat := 0.0
	if medianLatency > 0 {
		lat = float64(LatencyBudget) / float64(LatencyBudget+medianLatency)
	}
	ws := 0.0
	if passes > 0 {
		ws = 1.0
	}
	return 0.60*success + 0.25*lat + 0.15*ws
}

func medianOf(rs []roundResult, pick func(roundResult) int64) int64 {
	var vals []int64
	for _, r := range rs {
		if v := pick(r); v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals[len(vals)/2]
}

func readCandidates(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.Fields(line)[0]
		if !strings.Contains(line, "://") {
			line = "http://" + line
		}
		if _, err := url.Parse(line); err != nil || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out, sc.Err()
}

type probeOut struct {
	proxy string
	rr    roundResult
}

func probeAll(cands []string, round, conc int, timeout time.Duration) []probeOut {
	sem := make(chan struct{}, conc)
	outs := make([]probeOut, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i] = probeOut{proxy: c, rr: probeOne(c, round, timeout)}
		}(i, c)
	}
	wg.Wait()
	return outs
}

func probeOne(raw string, round int, timeout time.Duration) roundResult {
	rr := roundResult{Round: round}
	u, err := url.Parse(raw)
	if err != nil {
		rr.Err = "bad url"
		return rr
	}

	// L2 : auth plane. Only a real HTTP status code counts.
	code, ms, err := httpThrough(u, authHost, authPath, timeout)
	rr.L2Code, rr.L2Ms = code, ms
	if err != nil {
		rr.Err = "l2: " + err.Error()
		return rr
	}
	if code < 200 || code >= 500 {
		rr.Err = fmt.Sprintf("l2 status %d", code)
		return rr
	}

	// L3a/L3b : business plane reachability.
	if ms, err := tlsThrough(u, substrateHost, timeout); err == nil {
		rr.Substrate, rr.SubMs = true, ms
	} else {
		rr.Err += " substrate: " + err.Error()
	}
	if ms, err := tlsThrough(u, m365Host, timeout); err == nil {
		rr.M365, rr.M365Ms = true, ms
	} else {
		rr.Err += " m365: " + err.Error()
	}

	// L3c : the transport the chathub actually needs.
	okws, wsCode, note, wsMs, err := wsThrough(u, timeout)
	rr.WS, rr.WSCode, rr.WSNote, rr.WSMs = okws, wsCode, note, wsMs
	if err != nil && note == "" {
		rr.WSNote = err.Error()
	}

	rr.Pass = rr.Substrate && rr.M365 && rr.WS
	return rr
}

// dialTunnel returns a raw byte tunnel to host:443 through the proxy.
func dialTunnel(ctx context.Context, u *url.URL, host string, timeout time.Duration) (net.Conn, error) {
	target := net.JoinHostPort(host, "443")
	d := &net.Dialer{Timeout: timeout}

	switch u.Scheme {
	case "http", "https":
		pa := u.Host
		if u.Port() == "" {
			if u.Scheme == "https" {
				pa = net.JoinHostPort(u.Hostname(), "443")
			} else {
				pa = net.JoinHostPort(u.Hostname(), "80")
			}
		}
		c, err := d.DialContext(ctx, "tcp", pa)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "https" {
			// The hop to the proxy itself; target verification stays on.
			tc := tls.Client(c, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12,
				InsecureSkipVerify: net.ParseIP(u.Hostname()) != nil}) // #nosec G402 -- proxy hop only
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			c = tc
		}
		_ = c.SetDeadline(time.Now().Add(timeout))
		req := &http.Request{Method: http.MethodConnect,
			URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
		if u.User != nil {
			pw, _ := u.User.Password()
			// SetBasicAuth writes "Authorization", but proxies require "Proxy-Authorization".
			req.Header.Set("Proxy-Authorization", "Basic "+
				base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw)))
		}
		if err := req.Write(c); err != nil {
			c.Close()
			return nil, err
		}
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			c.Close()
			return nil, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			c.Close()
			return nil, fmt.Errorf("connect %s", resp.Status)
		}
		_ = c.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufConn{Conn: c, r: br}, nil
		}
		return c, nil

	case "socks5", "socks5h":
		c, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Now().Add(timeout))
		if err := socks5Connect(c, u, host, 443); err != nil {
			c.Close()
			return nil, err
		}
		_ = c.SetDeadline(time.Time{})
		return c, nil
	}
	return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
}

func socks5Connect(c net.Conn, u *url.URL, host string, port int) error {
	greet := []byte{0x05, 0x01, 0x00}
	if u.User != nil {
		greet = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := c.Write(greet); err != nil {
		return err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		return err
	}
	if rep[0] != 0x05 {
		return fmt.Errorf("not socks5")
	}
	switch rep[1] {
	case 0x00:
	case 0x02:
		if u.User == nil {
			return fmt.Errorf("socks5 auth required")
		}
		pw, _ := u.User.Password()
		user := u.User.Username()
		msg := []byte{0x01, byte(len(user))}
		msg = append(msg, user...)
		msg = append(msg, byte(len(pw)))
		msg = append(msg, pw...)
		if _, err := c.Write(msg); err != nil {
			return err
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(c, ar); err != nil {
			return err
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("socks5 auth rejected")
		}
	default:
		return fmt.Errorf("socks5 method %d", rep[1])
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := c.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5 reply %d", head[1])
	}
	switch head[3] {
	case 0x01:
		_, err := io.ReadFull(c, make([]byte, 4+2))
		return err
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return err
		}
		_, err := io.ReadFull(c, make([]byte, int(l[0])+2))
		return err
	case 0x04:
		_, err := io.ReadFull(c, make([]byte, 16+2))
		return err
	}
	return fmt.Errorf("socks5 atyp %d", head[3])
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func tlsThrough(u *url.URL, host string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	raw, err := dialTunnel(ctx, u, host, timeout)
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		return 0, err
	}
	tc.Close()
	return time.Since(start).Milliseconds(), nil
}

func httpThrough(u *url.URL, host, path string, timeout time.Duration) (int, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	raw, err := dialTunnel(ctx, u, host, timeout)
	if err != nil {
		return 0, 0, err
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		return 0, 0, err
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(timeout))
	// HEAD, no credentials, minimal footprint on the auth endpoint.
	req := "HEAD " + path + " HTTP/1.1\r\nHost: " + host +
		"\r\nUser-Agent: m365-proxy-quality-guard\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := tc.Write([]byte(req)); err != nil {
		return 0, 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return 0, 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, time.Since(start).Milliseconds(), nil
}

// wsThrough performs a real WebSocket Upgrade to the chathub endpoint.
// A 101 is ideal. A 400/401/403/404 still proves the proxy carried the Upgrade
// and only credentials were missing, which is what we are testing for.
func wsThrough(u *url.URL, timeout time.Duration) (bool, int, string, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	raw, err := dialTunnel(ctx, u, substrateHost, timeout)
	if err != nil {
		return false, 0, "tunnel: " + err.Error(), 0, err
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{ServerName: substrateHost, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		return false, 0, "tls: " + err.Error(), 0, err
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(timeout))
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, 0, "nonce", 0, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET " + chathubPath + " HTTP/1.1\r\nHost: " + substrateHost +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\nOrigin: https://" + m365Host +
		"\r\nUser-Agent: m365-proxy-quality-guard\r\n\r\n"
	if _, err := tc.Write([]byte(req)); err != nil {
		return false, 0, "write", 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return false, 0, "read: " + err.Error(), time.Since(start).Milliseconds(), err
	}
	resp.Body.Close()
	ms := time.Since(start).Milliseconds()
	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
		return true, resp.StatusCode, "upgraded", ms, nil
	case 400, 401, 403, 404:
		return true, resp.StatusCode, "ms_rejected_no_creds", ms, nil
	default:
		return false, resp.StatusCode, "unexpected", ms, nil
	}
}
