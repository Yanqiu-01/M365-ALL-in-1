package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyPrefixWithoutRegistrationIsRejected(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	record, raw, err := store.create("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{apiKeys: store}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("x-api-key", record.Prefix+strings.Repeat("z", len(raw)-len(record.Prefix)))
	if s.validAPIKey(request) {
		t.Fatal("unregistered API key with a valid-looking prefix was accepted")
	}
	request.Header.Set("x-api-key", raw)
	if !s.validAPIKey(request) {
		t.Fatal("registered API key was rejected")
	}
}

func TestAPIKeyPersistenceAndListingRedactRawValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	store := newAPIKeyStore(path)
	_, raw, err := store.create("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(raw)) {
		t.Fatal("API key was persisted in plaintext")
	}
	listed := store.list()
	if len(listed) != 1 || listed[0].Raw != "" || listed[0].Hash != "" {
		t.Fatal("API key listing exposed sensitive material")
	}
}

func TestStatsEndpointsRequireAdminSession(t *testing.T) {
	verifier, err := newPasswordVerifier(strings.Repeat("r", 24))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{adminPassword: verifier, adminSessions: map[string]time.Time{}}
	next := s.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		path   string
		method string
	}{
		{path: "/api/stats", method: http.MethodGet},
		{path: "/api/stats/reset", method: http.MethodPost},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		next.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d", test.method, test.path, response.Code)
		}
	}
}
