package web

import (
	"encoding/json"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func assertProxyPoolStoredAndLive(t *testing.T, s *Server, want []string) {
	t.Helper()
	if got := outbound.ProxyPoolRawURLs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime pool=%v want=%v", got, want)
	}
	data, err := os.ReadFile(s.settings.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk runtimeSettings
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk.ProxyPool, want) || !reflect.DeepEqual(s.settings.get().ProxyPool, want) {
		t.Fatalf("persisted pool changed: disk=%v memory=%v want=%v", disk.ProxyPool, s.settings.get().ProxyPool, want)
	}
}

func TestSettingsSavePreservesProxyPool(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)
	want := []string{"http://user:secret@127.0.0.1:18081", "socks5://127.0.0.1:18082"}
	v := s.settings.get()
	v.ProxyPool = want
	v.OutboundProxy = "http://fallback:password@127.0.0.1:18083"
	if err := s.settings.save(v); err != nil {
		t.Fatal(err)
	}
	if err := outbound.ConfigurePool(want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		s.adminSettings(w, httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"chatTimeoutSeconds":111}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("settings PUT=%d %s", w.Code, w.Body.String())
		}
		assertProxyPoolStoredAndLive(t, s, want)
		if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "password@") {
			t.Fatal("settings PUT exposed proxy credentials")
		}
	}
}

func TestAppendProxyPreservesPersistedPoolAfterRuntimeReset(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "duplicate"}[duplicate], func(t *testing.T) {
			resetProxyPoolForFeatureTest(t)
			s := proxyPoolFeatureTestServer(t)
			original := "http://user:secret@127.0.0.1:18081"
			v := s.settings.get()
			v.ProxyPool = []string{original}
			if err := s.settings.save(v); err != nil {
				t.Fatal(err)
			}
			if err := outbound.Configure(""); err != nil {
				t.Fatal(err)
			}
			candidate := "socks5://127.0.0.1:18082"
			want := []string{original, candidate}
			wantAdded := 1
			if duplicate {
				candidate, want, wantAdded = original, []string{original}, 0
			}
			added, err := s.appendProxyPool([]string{candidate})
			if err != nil || added != wantAdded {
				t.Fatalf("append: added=%d want=%d err=%v", added, wantAdded, err)
			}
			assertProxyPoolStoredAndLive(t, s, want)
		})
	}
}

func TestSettingsAndPoolMutationsShareLock(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)
	v := s.settings.get()
	if err := s.settings.save(v); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.adminSettings(w, httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"chatTimeoutSeconds":111}`)))
			if w.Code != http.StatusOK {
				t.Errorf("settings PUT=%d", w.Code)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.appendProxyPool([]string{"http://127.0.0.1:18081"}); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()
	assertProxyPoolStoredAndLive(t, s, []string{"http://127.0.0.1:18081"})
	if s.settings.get().ChatTimeoutSeconds != 111 {
		t.Fatal("pool mutation lost unrelated settings update")
	}
}

func TestSettingsCanExplicitlyDisableProxyPool(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)
	if _, err := s.appendProxyPool([]string{"http://127.0.0.1:18081"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.adminSettings(w, httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"proxyPool":[],"outboundProxy":"http://127.0.0.1:18082"}`)))
	if w.Code != http.StatusOK || len(outbound.ProxyPoolRawURLs()) != 0 || len(s.settings.get().ProxyPool) != 0 {
		t.Fatalf("explicit disable failed: %d", w.Code)
	}
}
