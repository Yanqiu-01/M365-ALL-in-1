package chathub

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	remoteImageTimeout   = 15 * time.Second
	remoteImageRedirects = 2
	maxRemoteURLLength   = 8192
	maxResponseHeaders   = 64 << 10
)

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var blockedRemotePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure platform virtual IP.
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
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func parseRemoteImageURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxRemoteURLLength {
		return nil, fmt.Errorf("invalid remote image URL")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return nil, fmt.Errorf("invalid remote image URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("remote image URL requires https")
	}
	if u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("remote image URL contains forbidden components")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || strings.ContainsAny(host, "\x00\r\n\t %") {
		return nil, fmt.Errorf("invalid remote image host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid remote image port")
		}
	}
	if isLocalHostname(host) {
		return nil, fmt.Errorf("remote image host is not public")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicRemoteAddr(addr) {
			return nil, fmt.Errorf("remote image address is not public")
		}
	} else {
		// Only reject non-standard numeric forms after strict parsing has had
		// a chance to accept a valid public IPv4 literal.
		if strings.Contains(host, ":") || looksLikeObscuredIP(host) {
			return nil, fmt.Errorf("remote image address is not public")
		}
	}
	return u, nil
}

func isLocalHostname(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".home.arpa") || host == "instance-data"
}

func looksLikeObscuredIP(host string) bool {
	allNumeric := true
	for _, r := range host {
		if (r < '0' || r > '9') && r != '.' {
			allNumeric = false
			break
		}
	}
	if allNumeric {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(strings.ToLower(label), "0x") {
			return true
		}
	}
	return false
}

func isPublicRemoteAddr(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedRemotePrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func resolvePublicRemoteAddrs(ctx context.Context, lookup lookupNetIPFunc, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicRemoteAddr(addr) {
			return nil, fmt.Errorf("remote image address is not public")
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	addrs, err := lookup(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("remote image host does not resolve publicly")
	}
	for _, addr := range addrs {
		if !isPublicRemoteAddr(addr) {
			return nil, fmt.Errorf("remote image host resolved to a non-public address")
		}
	}
	return addrs, nil
}

func safeRemoteDialContext(lookup lookupNetIPFunc, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid remote image address: %w", err)
		}
		addrs, err := resolvePublicRemoteAddrs(ctx, lookup, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, addr := range addrs {
			if (network == "tcp4" && !addr.Is4()) || (network == "tcp6" && !addr.Is6()) {
				continue
			}
			conn, err := dial(ctx, network, net.JoinHostPort(addr.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("remote image host has no usable public address")
		}
		return nil, lastErr
	}
}

func newRemoteImageHTTPClient(token string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = safeRemoteDialContext(net.DefaultResolver.LookupNetIP, dialer.DialContext)
	transport.DialTLSContext = nil
	transport.DisableKeepAlives = true
	transport.MaxIdleConns = 0
	transport.MaxResponseHeaderBytes = maxResponseHeaders
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 8 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   remoteImageTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > remoteImageRedirects {
				return fmt.Errorf("too many remote image redirects")
			}
			if _, err := parseRemoteImageURL(req.URL.String()); err != nil {
				return err
			}
			if token != "" && !trustedImageTokenURL(req.URL) {
				return fmt.Errorf("authenticated remote image redirect changed host")
			}
			req.Header.Del("Authorization")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return nil
		},
	}
}

func trustedImageTokenURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") ||
		!strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "designerapp.officeapps.live.com") {
		return false
	}
	return u.Port() == "" || u.Port() == "443"
}

func DownloadRemoteImage(ctx context.Context, raw string, maxBytes int64, token string) ([]byte, string, error) {
	u, err := parseRemoteImageURL(raw)
	if err != nil {
		return nil, "", err
	}
	if maxBytes <= 0 || maxBytes > MaxAttachmentBytes*2 {
		return nil, "", fmt.Errorf("invalid remote image size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, remoteImageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/*")
	effectiveToken := ""
	if token != "" && trustedImageTokenURL(u) {
		effectiveToken = token
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := newRemoteImageHTTPClient(effectiveToken).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("remote image download canceled or timed out: %w", ctx.Err())
		}
		return nil, "", fmt.Errorf("remote image download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("remote image download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, "", fmt.Errorf("remote image exceeds %d bytes", maxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxBytes {
		return nil, "", fmt.Errorf("remote image exceeds %d bytes", maxBytes)
	}
	contentType := http.DetectContentType(body)
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return nil, "", fmt.Errorf("remote response is not an image")
	}
	return body, mediaType, nil
}

func validateImageSource(raw string) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		_, _, err := decodeImageDataURL(raw, MaxAttachmentBytes)
		return err
	}
	_, err := parseRemoteImageURL(raw)
	return err
}

func decodeImageDataURL(raw string, maxBytes int64) ([]byte, string, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < 0 || !strings.HasPrefix(strings.ToLower(raw[:comma]), "data:") {
		return nil, "", fmt.Errorf("invalid image data URL")
	}
	meta := raw[len("data:"):comma]
	parts := strings.Split(meta, ";")
	mediaType := strings.ToLower(strings.TrimSpace(parts[0]))
	if !strings.HasPrefix(mediaType, "image/") {
		return nil, "", fmt.Errorf("data URL is not an image")
	}
	base64Encoded := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			base64Encoded = true
		}
	}
	if !base64Encoded {
		return nil, "", fmt.Errorf("image data URL is not base64")
	}
	encoded := raw[comma+1:]
	if int64(len(encoded)) > ((maxBytes+2)/3)*4+8 {
		return nil, "", fmt.Errorf("image data exceeds %d bytes", maxBytes)
	}
	decoded, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	if int64(len(decoded)) > maxBytes {
		return nil, "", fmt.Errorf("image data exceeds %d bytes", maxBytes)
	}
	return decoded, mediaType, nil
}
