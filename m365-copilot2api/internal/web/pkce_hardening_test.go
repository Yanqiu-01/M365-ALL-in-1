package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startPKCE must survive a zero-value Server: the routing smoke test builds one
// without the map and previously panicked with "assignment to entry in nil map".
func TestStartPKCEInitializesStateMap(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.startPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/start", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(s.pkce) != 1 {
		t.Fatalf("pending states = %d, want 1", len(s.pkce))
	}
}

// A fresh authorization must not leave earlier attempts behind; a stale
// terminal state is what kept the UI re-entering the previous callback.
func TestStartPKCEClearsStaleAttempts(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"done":    {Created: time.Now(), Status: "authenticated", Consumed: true},
		"failed":  {Created: time.Now(), Status: "error", Error: "boom"},
		"idle":    {Created: time.Now(), Status: "pending"},
		"expired": {Created: time.Now().Add(-11 * time.Minute), Status: "pending"},
		"busy":    {Created: time.Now(), Status: "exchanging", Exchanging: true},
	}}
	rr := httptest.NewRecorder()
	s.startPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/start", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, gone := range []string{"done", "failed", "idle", "expired", "busy"} {
		if _, ok := s.pkce[gone]; ok {
			t.Errorf("stale state %q survived a new authorization", gone)
		}
	}
	if len(s.pkce) != 1 {
		t.Fatalf("pending states = %d, want 1 (new attempt only)", len(s.pkce))
	}
}

// The default prompt has to force a fresh Microsoft login. select_account
// still auto-continues a single signed-in session, so a new account cannot
// be added from the same browser.
func TestStartPKCEForcesLogin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		env   *string
		want  string
	}{
		{name: "default", want: "login"},
		{name: "forceLogin", query: "?forceLogin=1", want: "login"},
		{name: "explicit prompt", query: "?prompt=consent", want: "consent"},
		{name: "select_account override", query: "?prompt=select_account", want: "select_account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			rr := httptest.NewRecorder()
			s.startPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/start"+tc.query, nil))
			var response struct {
				URL       string `json:"url"`
				Prompt    string `json:"prompt"`
				LogoutURL string `json:"logoutUrl"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Prompt != tc.want {
				t.Fatalf("prompt = %q, want %q", response.Prompt, tc.want)
			}
			u, err := url.Parse(response.URL)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("prompt"); got != tc.want {
				t.Fatalf("authorization prompt = %q, want %q", got, tc.want)
			}
			if !strings.Contains(response.LogoutURL, "/logout") {
				t.Fatalf("logoutUrl = %q, want a logout endpoint", response.LogoutURL)
			}
		})
	}
}

// A replayed callback must be refused instead of re-entering the login flow.
func TestCallbackPKCERejectsReplayAndConcurrentExchange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state pendingPKCE
		want  int
	}{
		{
			name:  "already consumed",
			state: pendingPKCE{Created: time.Now(), Status: "authenticated", Consumed: true},
			want:  http.StatusConflict,
		},
		{
			name:  "exchange in flight",
			state: pendingPKCE{Created: time.Now(), Status: "exchanging", Exchanging: true},
			want:  http.StatusConflict,
		},
		{
			name:  "expired",
			state: pendingPKCE{Created: time.Now().Add(-11 * time.Minute), Status: "pending"},
			want:  http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{pkce: map[string]pendingPKCE{"st": tc.state}}
			rr := httptest.NewRecorder()
			s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=st&code=abc", nil))
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestCallbackPKCERejectsUnknownState(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=nope&code=abc", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestResetPKCEClearsEverything(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"a": {Created: time.Now(), Status: "pending"},
		"b": {Created: time.Now(), Status: "exchanging", Exchanging: true},
	}}
	rr := httptest.NewRecorder()
	s.resetPKCE(rr, httptest.NewRequest(http.MethodPost, "/api/auth/reset", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var response struct {
		Cleared   int    `json:"cleared"`
		LogoutURL string `json:"logoutUrl"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Cleared != 2 {
		t.Fatalf("cleared = %d, want 2", response.Cleared)
	}
	if response.LogoutURL == "" {
		t.Fatal("reset must tell the UI where to drop the Microsoft session")
	}
	if len(s.pkce) != 0 {
		t.Fatalf("pending states = %d, want 0", len(s.pkce))
	}
}

// prunePKCELocked keeps a just-finished state visible for the status poller but
// drops it once the grace period passes.
func TestPrunePKCEKeepsRecentTerminalState(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"fresh": {Created: time.Now(), Status: "authenticated", Consumed: true},
		"old":   {Created: time.Now().Add(-3 * time.Minute), Status: "authenticated", Consumed: true},
		"stale": {Created: time.Now().Add(-3 * time.Minute), Status: "error"},
	}}
	s.mu.Lock()
	s.prunePKCELocked()
	s.mu.Unlock()
	if _, ok := s.pkce["fresh"]; !ok {
		t.Error("recent terminal state must stay readable by the poller")
	}
	for _, gone := range []string{"old", "stale"} {
		if _, ok := s.pkce[gone]; ok {
			t.Errorf("state %q should have been pruned", gone)
		}
	}
}
