package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func proxyPoolFeatureTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		settings: &settingsStore{
			path: filepath.Join(t.TempDir(), "settings.json"),
			v:    defaultRuntimeSettings(),
		},
		adminPassword: "test-admin-password",
		adminSessions: map[string]time.Time{"test-admin-session": time.Now().Add(time.Hour)},
	}
}

func resetProxyPoolForFeatureTest(t *testing.T) {
	t.Helper()
	previous := append([]string(nil), outbound.ProxyPoolRawURLs()...)
	if err := outbound.ConfigurePool(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = outbound.ConfigurePool(previous)
	})
}

func setFreeProxyTestSeams(t *testing.T, fetch func(context.Context, string) ([]byte, error), validate func(context.Context, string) error) {
	t.Helper()
	oldFetch, oldValidate := freeProxySourceFetcher, freeProxyCandidateValidator
	freeProxySourceFetcher, freeProxyCandidateValidator = fetch, validate
	t.Cleanup(func() {
		freeProxySourceFetcher, freeProxyCandidateValidator = oldFetch, oldValidate
	})
}

func TestFreeProxyImportFiltersDeduplicatesAndPersists(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	if err := outbound.ConfigurePool([]string{"http://8.8.8.8:8080"}); err != nil {
		t.Fatal(err)
	}

	const sourceURL = "https://source.example/proxies.txt?short_lived_token=do-not-echo"
	var checkedMu sync.Mutex
	var checked []string
	setFreeProxyTestSeams(t,
		func(ctx context.Context, got string) ([]byte, error) {
			if got != sourceURL {
				t.Fatalf("source URL = %q", got)
			}
			return []byte("8.8.8.8:8080\n1.1.1.1:8080\n1.1.1.1:8080\n192.168.1.9:3128\n"), nil
		},
		func(ctx context.Context, candidate string) error {
			checkedMu.Lock()
			checked = append(checked, candidate)
			checkedMu.Unlock()
			if candidate == "http://1.1.1.1:8080" {
				return nil
			}
			return errors.New("quality gate rejected candidate")
		},
	)

	s := proxyPoolFeatureTestServer(t)
	body := `{"sourceUrl":"` + sourceURL + `","scheme":"http","limit":30}`
	rec := httptest.NewRecorder()
	s.proxyPool(rec, httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "short_lived_token") {
		t.Fatalf("source URL token leaked in response: %s", rec.Body.String())
	}

	var response struct {
		OK     bool                  `json:"ok"`
		Report freeProxyImportReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if response.Report.Discovered != 4 || response.Report.Invalid != 1 || response.Report.Duplicates != 2 {
		t.Fatalf("report = %+v", response.Report)
	}
	if response.Report.Checked != 1 || response.Report.Passed != 1 || response.Report.Rejected != 0 || response.Report.Imported != 1 {
		t.Fatalf("report = %+v", response.Report)
	}
	checkedMu.Lock()
	defer checkedMu.Unlock()
	if len(checked) != 1 || checked[0] != "http://1.1.1.1:8080" {
		t.Fatalf("checked = %#v", checked)
	}
	got := outbound.ProxyPoolRawURLs()
	if len(got) != 2 || got[0] != "http://8.8.8.8:8080" || got[1] != "http://1.1.1.1:8080" {
		t.Fatalf("pool = %#v", got)
	}
	if stored := s.settings.get().ProxyPool; len(stored) != 2 || stored[1] != "http://1.1.1.1:8080" {
		t.Fatalf("persisted pool = %#v", stored)
	}
}

func TestFreeProxyImportRouteIsAdminProtected(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	var fetchCalls atomic.Int32
	setFreeProxyTestSeams(t,
		func(context.Context, string) ([]byte, error) {
			fetchCalls.Add(1)
			return []byte("1.1.1.1:8080"), nil
		},
		func(context.Context, string) error { return nil },
	)

	s := proxyPoolFeatureTestServer(t)
	routes := s.Routes()
	payload := `{"sourceUrl":"https://source.example/list","scheme":"http","limit":1}`

	anonymousReq := httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import", strings.NewReader(payload))
	anonymousReq.RemoteAddr = "198.51.100.20:12345"
	anonymousRec := httptest.NewRecorder()
	routes.ServeHTTP(anonymousRec, anonymousReq)
	if anonymousRec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", anonymousRec.Code, anonymousRec.Body.String())
	}
	if fetchCalls.Load() != 0 {
		t.Fatalf("unauthenticated request invoked source fetcher %d times", fetchCalls.Load())
	}

	authorizedReq := httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import", strings.NewReader(payload))
	authorizedReq.RemoteAddr = "198.51.100.20:12345"
	authorizedReq.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: "test-admin-session"})
	authorizedRec := httptest.NewRecorder()
	routes.ServeHTTP(authorizedRec, authorizedReq)
	if authorizedRec.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", authorizedRec.Code, authorizedRec.Body.String())
	}
	if fetchCalls.Load() != 1 {
		t.Fatalf("authenticated source fetcher calls=%d want 1", fetchCalls.Load())
	}
}

func TestDeleteSelectedProxiesUsesCredentialFreeIDs(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	const secret = "dont-put-this-in-api"
	if err := outbound.ConfigurePool([]string{
		"http://alice:" + secret + "@8.8.8.8:8080",
		"socks5://1.1.1.1:1080",
		"https://9.9.9.9:443",
	}); err != nil {
		t.Fatal(err)
	}
	items := outbound.ProxyPoolStatusRedacted()
	if len(items) != 3 {
		t.Fatalf("status = %#v", items)
	}
	id0, _ := items[0]["id"].(string)
	id2, _ := items[2]["id"].(string)
	if id0 == "" || id2 == "" {
		t.Fatalf("missing IDs in status: %#v", items)
	}

	s := proxyPoolFeatureTestServer(t)
	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"ids":["` + id0 + `","` + id2 + `","missing-id","` + id0 + `"]}`)
	s.proxyPool(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/proxy-pool?action=selected", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("proxy password leaked in delete response: %s", rec.Body.String())
	}
	var response struct {
		Deleted int `json:"deleted"`
		Missing int `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Deleted != 2 || response.Missing != 1 {
		t.Fatalf("response = %+v", response)
	}
	if got := outbound.ProxyPoolRawURLs(); len(got) != 1 || got[0] != "socks5://1.1.1.1:1080" {
		t.Fatalf("remaining pool = %#v", got)
	}
	if stored := s.settings.get().ProxyPool; len(stored) != 1 || stored[0] != "socks5://1.1.1.1:1080" {
		t.Fatalf("persisted pool = %#v", stored)
	}
}

func TestParseFreeProxyCandidatesRejectsPrivateOrNonIPTargets(t *testing.T) {
	candidates, invalid := parseFreeProxyCandidates([]byte("8.8.4.4:8080\nhttps://1.0.0.1:443\n127.0.0.1:1080\n192.168.1.2:3128\nproxy.example:8080\n"), "http")
	if invalid != 3 {
		t.Fatalf("invalid=%d want 3", invalid)
	}
	want := []string{"http://8.8.4.4:8080", "https://1.0.0.1:443"}
	if strings.Join(candidates, "|") != strings.Join(want, "|") {
		t.Fatalf("candidates=%#v want %#v", candidates, want)
	}
	if _, _, err := normalizeFreeProxyCandidate("10.0.0.1:80", "http"); err == nil {
		t.Fatal("private address was accepted")
	}
}

func TestReadFreeProxySourceWithHTTPTestServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Fatal("source request omitted user agent")
		}
		_, _ = w.Write([]byte("8.8.8.8:8080\n"))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := readFreeProxySource(context.Background(), server.Client(), target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "8.8.8.8:8080\n" {
		t.Fatalf("payload=%q", payload)
	}
	if _, err := fetchFreeProxySource(context.Background(), server.URL); err == nil {
		t.Fatal("loopback source URL was accepted by hardened fetcher")
	}
}

func TestFreeProxyValidationCapsConcurrency(t *testing.T) {
	oldFetch, oldValidate := freeProxySourceFetcher, freeProxyCandidateValidator
	defer func() { freeProxySourceFetcher, freeProxyCandidateValidator = oldFetch, oldValidate }()
	var active, peak atomic.Int32
	freeProxyCandidateValidator = func(context.Context, string) error {
		current := active.Add(1)
		for {
			seen := peak.Load()
			if current <= seen || peak.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return nil
	}
	candidates := []string{
		"http://1.1.1.1:8080", "http://1.0.0.1:8080", "http://8.8.8.8:8080", "http://8.8.4.4:8080",
		"http://9.9.9.9:8080", "http://208.67.222.222:8080", "http://208.67.220.220:8080", "http://64.6.64.6:8080",
	}
	accepted := validateFreeProxyCandidates(context.Background(), candidates)
	if len(accepted) != len(candidates) {
		t.Fatalf("accepted=%#v", accepted)
	}
	if peak.Load() > freeProxyProbeConcurrency {
		t.Fatalf("peak concurrency=%d cap=%d", peak.Load(), freeProxyProbeConcurrency)
	}
}

func TestProxyPoolDashboardIncludesConfigurablePurchaseAndSelectedDelete(t *testing.T) {
	pagePath := filepath.Join("..", "..", "web", "index.html")
	page, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(page)
	for _, marker := range []string{"proxyFreeImport()", "proxyPurchaseURL", "proxyDeleteSelected()", "proxySelectAll"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("dashboard missing %q", marker)
		}
	}
	for _, forbidden := range []string{"bapi.51daili.com", "accessPassword", "accessName"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("dashboard contains hard-coded provider credential field %q", forbidden)
		}
	}
}
