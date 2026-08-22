package chathub

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestParseRemoteImageURLRejectsSSRFRepresentations(t *testing.T) {
	rejected := []string{
		"http://example.com/a.png",
		"https://localhost/a.png",
		"https://metadata.google.internal/a.png",
		"https://127.0.0.1/a.png",
		"https://127.1/a.png",
		"https://0177.0.0.1/a.png",
		"https://2130706433/a.png",
		"https://0x7f000001/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://100.100.100.200/latest/meta-data",
		"https://168.63.129.16/a.png",
		"https://[::1]/a.png",
		"https://[::ffff:127.0.0.1]/a.png",
		"https://[fe80::1]/a.png",
		"https://[fc00::1]/a.png",
	}
	for _, raw := range rejected {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseRemoteImageURL(raw); err == nil {
				t.Fatalf("accepted %q", raw)
			}
		})
	}
	if _, err := parseRemoteImageURL("https://example.com/a.png"); err != nil {
		t.Fatalf("public URL rejected: %v", err)
	}
	if _, err := parseRemoteImageURL("https://93.184.216.34/a.png"); err != nil {
		t.Fatalf("public IPv4 literal rejected: %v", err)
	}
	if _, err := parseRemoteImageURL("https://[2606:4700:4700::1111]/a.png"); err != nil {
		t.Fatalf("public IPv6 literal rejected: %v", err)
	}
}

func TestAuthenticatedRedirectStaysOnTrustedDesignerHost(t *testing.T) {
	client := newRemoteImageHTTPClient("placeholder")
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "designerapp.officeapps.live.com"}}}
	crossHost := &http.Request{URL: &url.URL{Scheme: "https", Host: "cdn.example", Path: "/image.png"}, Header: make(http.Header)}
	if err := client.CheckRedirect(crossHost, via); err == nil {
		t.Fatal("authenticated image redirect to another host was accepted")
	}
	sameHost := &http.Request{URL: &url.URL{Scheme: "https", Host: "designerapp.officeapps.live.com", Path: "/image.png"}, Header: make(http.Header)}
	if err := client.CheckRedirect(sameHost, via); err != nil {
		t.Fatalf("trusted same-host redirect rejected: %v", err)
	}
	if sameHost.Header.Get("Authorization") == "" {
		t.Fatal("trusted redirect did not retain authorization")
	}
}

func TestRemoteImageRedirectLimit(t *testing.T) {
	client := newRemoteImageHTTPClient("")
	via := make([]*http.Request, remoteImageRedirects+1)
	for i := range via {
		via[i] = &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com", Path: "/image.png"}, Header: make(http.Header)}
	if err := client.CheckRedirect(req, via); err == nil {
		t.Fatal("redirect limit was not enforced")
	}
}

func TestSafeRemoteDialPinsResolvedAddress(t *testing.T) {
	lookups := 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	var dialed string
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("stop")
	}
	_, _ = safeRemoteDialContext(lookup, dial)(context.Background(), "tcp", "cdn.example:443")
	if lookups != 1 || dialed != "93.184.216.34:443" {
		t.Fatalf("lookups=%d dialed=%q", lookups, dialed)
	}
}

func TestSafeRemoteDialRejectsReboundPrivateAddress(t *testing.T) {
	dialed := false
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected")
	}
	if _, err := safeRemoteDialContext(lookup, dial)(context.Background(), "tcp", "cdn.example:443"); err == nil {
		t.Fatal("private rebound address was accepted")
	}
	if dialed {
		t.Fatal("dialer was called for a private address")
	}
}

func TestImageDataURLAndAttachmentLimits(t *testing.T) {
	tooLarge := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 33)))
	if _, _, err := decodeImageDataURL("data:image/png;base64,"+tooLarge, 32); err == nil {
		t.Fatal("oversize data URL was accepted")
	}
	attachments := make([]Attachment, MaxAttachments+1)
	if err := ValidateAttachments(attachments); err == nil {
		t.Fatal("too many attachments were accepted")
	}
}
