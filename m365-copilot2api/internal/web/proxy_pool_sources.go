package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"m365-copilot2api/internal/outbound"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	freeProxySourceMaxBytes      int64 = 256 << 10
	freeProxySourceTimeout             = 12 * time.Second
	freeProxyImportTimeout             = 45 * time.Second
	freeProxyProbeConcurrency          = 4
	freeProxyDefaultImportLimit        = 10
	freeProxyMaxImportLimit            = 30
	freeProxySourceRedirectLimit       = 3
)

// These seams keep network-dependent admission checks out of unit tests. The
// production values always use the same L2 quality gate as manual imports.
var (
	freeProxySourceFetcher      = fetchFreeProxySource
	freeProxyCandidateValidator = outbound.ValidateProxyCandidate
)

type freeProxyImportRequest struct {
	SourceURL string `json:"sourceUrl"`
	Scheme    string `json:"scheme"`
	Limit     int    `json:"limit"`
}

type freeProxyImportReport struct {
	Discovered int `json:"discovered"`
	Invalid    int `json:"invalid"`
	Duplicates int `json:"duplicates"`
	Limited    int `json:"limited"`
	Checked    int `json:"checked"`
	Passed     int `json:"passed"`
	Rejected   int `json:"rejected"`
	Imported   int `json:"imported"`
}

// importFreeProxySource fetches an operator-provided public text list, accepts
// only public IP:port exits, probes them through the normal M365 L2 admission
// gate, and persists only the exits that pass. It intentionally does not expose
// the supplied source URL or candidate values in errors/responses: source URLs
// often contain short-lived provider query parameters.
func (s *Server) importFreeProxySource(w http.ResponseWriter, r *http.Request) {
	var body freeProxyImportRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if strings.TrimSpace(body.SourceURL) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "free proxy source URL is required")
		return
	}
	scheme, err := normalizeFreeProxyScheme(body.Scheme)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit := normalizeFreeProxyImportLimit(body.Limit)

	ctx, cancel := context.WithTimeout(r.Context(), freeProxyImportTimeout)
	defer cancel()
	payload, err := freeProxySourceFetcher(ctx, body.SourceURL)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "proxy_source_error", "免费代理来源获取失败；仅支持公开可访问的 http/https 文本列表")
		return
	}

	parsed, invalid := parseFreeProxyCandidates(payload, scheme)
	report := freeProxyImportReport{Discovered: len(parsed) + invalid, Invalid: invalid}
	existing := existingProxyEndpointKeys()
	seen := make(map[string]struct{}, len(existing)+len(parsed))
	for key := range existing {
		seen[key] = struct{}{}
	}
	candidates := make([]string, 0, min(limit, len(parsed)))
	for _, candidate := range parsed {
		key, ok := proxyEndpointKey(candidate)
		if !ok {
			// parseFreeProxyCandidates has already structurally checked the value;
			// keep this guard in case a future parser changes independently.
			report.Invalid++
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			report.Duplicates++
			continue
		}
		seen[key] = struct{}{}
		if len(candidates) >= limit {
			report.Limited++
			continue
		}
		candidates = append(candidates, candidate)
	}

	report.Checked = len(candidates)
	accepted := validateFreeProxyCandidates(ctx, candidates)
	report.Passed = len(accepted)
	report.Rejected = report.Checked - report.Passed
	if len(accepted) != 0 {
		imported, err := s.appendProxyPool(accepted)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", err.Error())
			return
		}
		report.Imported = imported
	}

	jsonOut(w, map[string]any{
		"ok": true, "report": report,
		"proxies": outbound.ProxyPoolStatusRedacted(),
	})
}

func normalizeFreeProxyScheme(raw string) (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(raw))
	if scheme == "" {
		return "http", nil
	}
	switch scheme {
	case "http", "https", "socks5":
		return scheme, nil
	default:
		return "", errors.New("free proxy scheme must be http, https, or socks5")
	}
}

func normalizeFreeProxyImportLimit(limit int) int {
	if limit <= 0 {
		return freeProxyDefaultImportLimit
	}
	if limit > freeProxyMaxImportLimit {
		return freeProxyMaxImportLimit
	}
	return limit
}

// parseFreeProxyCandidates tolerates the common plain-text and simple HTML
// table separators used by public lists. It intentionally admits only literal
// public IP addresses: allowing a remote list to nominate arbitrary hostnames
// would turn the M365 quality probe into an SSRF primitive against local DNS.
func parseFreeProxyCandidates(payload []byte, defaultScheme string) ([]string, int) {
	text := strings.TrimPrefix(string(payload), "\ufeff")
	for _, marker := range []string{"<br>", "<br/>", "<br />", "<BR>", "<BR/>", "<BR />"} {
		text = strings.ReplaceAll(text, marker, "\n")
	}
	tokens := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '\r', '\n', '\t', ' ', ',', ';', '|', '<', '>', '"', '\'':
			return true
		default:
			return false
		}
	})

	out := make([]string, 0, len(tokens))
	invalid := 0
	for _, token := range tokens {
		token = strings.Trim(token, "\ufeff`(){}")
		if token == "" {
			continue
		}
		candidate, _, err := normalizeFreeProxyCandidate(token, defaultScheme)
		if err != nil {
			invalid++
			continue
		}
		out = append(out, candidate)
	}
	return out, invalid
}

func normalizeFreeProxyCandidate(raw, defaultScheme string) (canonical, endpointKey string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("empty proxy")
	}
	if !strings.Contains(raw, "://") {
		raw = defaultScheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", "", errors.New("invalid proxy URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if _, err := normalizeFreeProxyScheme(scheme); err != nil {
		return "", "", err
	}
	if u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("proxy URL must not include path, query, or fragment")
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", "", errors.New("proxy must include host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", errors.New("proxy port is invalid")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !isPublicProxyAddress(addr) {
		return "", "", errors.New("free proxy must use a public IP address")
	}

	canonicalURL := &url.URL{Scheme: scheme, Host: net.JoinHostPort(addr.String(), port)}
	canonical = canonicalURL.String()
	if err := outbound.ValidateProxyURL(canonical); err != nil {
		return "", "", err
	}
	return canonical, scheme + "://" + strings.ToLower(canonicalURL.Host), nil
}

func existingProxyEndpointKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	for _, raw := range outbound.ProxyPoolRawURLs() {
		if key, ok := proxyEndpointKey(raw); ok {
			keys[key] = struct{}{}
		}
	}
	return keys
}

// proxyEndpointKey intentionally excludes URL userinfo, so a public IP source
// cannot add a second route for an already configured host:port by changing a
// harmless textual representation.
func proxyEndpointKey(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

func validateFreeProxyCandidates(ctx context.Context, candidates []string) []string {
	type result struct {
		candidate string
		accepted  bool
	}
	results := make(chan result, len(candidates))
	semaphore := make(chan struct{}, freeProxyProbeConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- result{candidate: candidate}
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, proxyAdmissionTimeout)
			err := freeProxyCandidateValidator(probeCtx, candidate)
			cancel()
			results <- result{candidate: candidate, accepted: err == nil}
		}()
	}
	wg.Wait()
	close(results)

	accepted := make([]string, 0, len(candidates))
	for result := range results {
		if result.accepted {
			accepted = append(accepted, result.candidate)
		}
	}
	return accepted
}

func fetchFreeProxySource(ctx context.Context, rawURL string) ([]byte, error) {
	target, err := parseFreeProxySourceURL(rawURL)
	if err != nil {
		return nil, err
	}
	return readFreeProxySource(ctx, newFreeProxySourceHTTPClient(), target)
}

func parseFreeProxySourceURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || len(rawURL) > 8192 {
		return nil, errors.New("invalid source URL")
	}
	u, err := url.ParseRequestURI(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid source URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("unsupported source URL scheme")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !isPublicProxyAddress(ip) {
		return nil, errors.New("non-public source URL")
	}
	return u, nil
}

func newFreeProxySourceHTTPClient() *http.Client {
	return &http.Client{
		Timeout: freeProxySourceTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialPublicProxySource,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			MaxConnsPerHost:       2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= freeProxySourceRedirectLimit {
				return errors.New("too many redirects")
			}
			_, err := parseFreeProxySourceURL(req.URL.String())
			return err
		},
	}
}

func readFreeProxySource(ctx context.Context, client *http.Client, target *url.URL) ([]byte, error) {
	if client == nil || target == nil {
		return nil, errors.New("source client is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("source request is invalid")
	}
	req.Header.Set("Accept", "text/plain, text/html;q=0.8, application/octet-stream;q=0.5")
	req.Header.Set("User-Agent", "M365-Copilot2API-ProxyPool/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("source request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > freeProxySourceMaxBytes {
		return nil, errors.New("source response is too large")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, freeProxySourceMaxBytes+1))
	if err != nil {
		return nil, errors.New("source response could not be read")
	}
	if int64(len(body)) > freeProxySourceMaxBytes {
		return nil, errors.New("source response is too large")
	}
	return body, nil
}

// dialPublicProxySource deliberately bypasses the configured proxy pool and
// rejects loopback, private, and documentation ranges. This prevents an admin
// source URL (or a redirect/DNS answer controlled by it) from reaching local
// services while fetching a public proxy list.
func dialPublicProxySource(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid source destination")
	}
	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("source host could not be resolved")
		}
	}

	dialer := &net.Dialer{}
	var lastErr error
	for _, addr := range addresses {
		if !isPublicProxyAddress(addr) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, errors.New("public source host is unreachable")
	}
	return nil, errors.New("source host has no public address")
}

var nonPublicProxyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicProxyAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range nonPublicProxyPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return addr.IsGlobalUnicast()
}
