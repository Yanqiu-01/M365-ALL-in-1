package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func openPKCECallbackTestStore(t *testing.T) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("open test account store: %v", err)
	}
	return store
}

func waitForPKCEExchange(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("PKCE exchange did not start")
	}
}

func waitForPKCECallback(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rr := <-done:
		return rr
	case <-time.After(2 * time.Second):
		t.Fatal("PKCE callback did not finish")
		return nil
	}
}

func pkceStatusForTest(t *testing.T, s *Server, state string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	s.pkceStatus(rr, httptest.NewRequest(http.MethodGet, "/api/auth/status?state="+state, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("PKCE status code = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode PKCE status: %v", err)
	}
	return body
}

func blockingPKCEExchange(started chan<- struct{}, release <-chan struct{}, tok auth.TokenSet) func(string, string, string) (auth.TokenSet, error) {
	return func(_, _, _ string) (auth.TokenSet, error) {
		close(started)
		<-release
		return tok, nil
	}
}

// A new authorization must be entirely independent from an earlier browser
// callback that is already exchanging its code. The old result must not write
// an account, resurrect its state, or change the new attempt's status.
func TestStartPKCESupersedesInFlightCallback(t *testing.T) {
	store := openPKCECallbackTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	s := &Server{
		pkceAttempt: 41,
		pkce: map[string]pendingPKCE{
			"old-state": {Verifier: "old-verifier", Created: time.Now(), Attempt: 41, Status: "pending"},
		},
		tokens: store,
		exchangePKCECode: blockingPKCEExchange(started, release, auth.TokenSet{
			AccessToken: "unit-test-access-token",
			Email:       "stale@example.test",
			HomeOID:     "stale-account",
			ExpiresAt:   time.Now().Add(time.Hour),
		}),
	}

	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=old-state&code=old-code", nil))
		oldDone <- rr
	}()
	waitForPKCEExchange(t, started)

	startRR := httptest.NewRecorder()
	s.startPKCE(startRR, httptest.NewRequest(http.MethodPost, "/api/auth/start?forceLogin=1", nil))
	if startRR.Code != http.StatusOK {
		t.Fatalf("new start status = %d, body = %s", startRR.Code, startRR.Body.String())
	}
	var fresh struct {
		State   string `json:"state"`
		Attempt uint64 `json:"attempt"`
	}
	if err := json.NewDecoder(startRR.Body).Decode(&fresh); err != nil {
		t.Fatalf("decode new start: %v", err)
	}
	if fresh.State == "" || fresh.State == "old-state" {
		t.Fatalf("new state = %q, expected a fresh state", fresh.State)
	}
	if fresh.Attempt != 42 {
		t.Fatalf("new attempt = %d, want 42", fresh.Attempt)
	}

	oldStatus := pkceStatusForTest(t, s, "old-state")
	if oldStatus["status"] != "expired" || oldStatus["terminal"] != true {
		t.Fatalf("old status = %#v, want terminal expired", oldStatus)
	}
	newStatus := pkceStatusForTest(t, s, fresh.State)
	if newStatus["status"] != "pending" || newStatus["terminal"] != false {
		t.Fatalf("new status = %#v, want pending non-terminal", newStatus)
	}
	if got, ok := newStatus["attempt"].(float64); !ok || uint64(got) != fresh.Attempt {
		t.Fatalf("new status attempt = %#v, want %d", newStatus["attempt"], fresh.Attempt)
	}

	close(release)
	if oldRR := waitForPKCECallback(t, oldDone); oldRR.Code != http.StatusConflict {
		t.Fatalf("superseded callback status = %d, want 409; body = %s", oldRR.Code, oldRR.Body.String())
	}
	if accounts := store.List(); len(accounts) != 0 {
		t.Fatalf("superseded callback persisted %d account(s)", len(accounts))
	}
	if _, found := s.pkce["old-state"]; found {
		t.Fatal("superseded state was resurrected")
	}
	if pending := s.pkce[fresh.State]; pending.Status != "pending" || pending.Attempt != fresh.Attempt {
		t.Fatalf("new attempt was overwritten: %#v", pending)
	}
}

// Reset must clear an unfinished exchange atomically. A network response that
// returns afterwards is rejected and cannot recreate state or import an old
// account.
func TestResetPKCEInvalidatesInFlightCallback(t *testing.T) {
	store := openPKCECallbackTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	s := &Server{
		pkceAttempt: 7,
		pkce: map[string]pendingPKCE{
			"reset-state": {Verifier: "reset-verifier", Created: time.Now(), Attempt: 7, Status: "pending"},
		},
		tokens: store,
		exchangePKCECode: blockingPKCEExchange(started, release, auth.TokenSet{
			AccessToken: "unit-test-access-token",
			Email:       "reset@example.test",
			HomeOID:     "reset-account",
			ExpiresAt:   time.Now().Add(time.Hour),
		}),
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=reset-state&code=reset-code", nil))
		done <- rr
	}()
	waitForPKCEExchange(t, started)

	resetRR := httptest.NewRecorder()
	s.resetPKCE(resetRR, httptest.NewRequest(http.MethodPost, "/api/auth/reset", nil))
	if resetRR.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", resetRR.Code, resetRR.Body.String())
	}
	var reset struct {
		Status  string `json:"status"`
		Cleared int    `json:"cleared"`
	}
	if err := json.NewDecoder(resetRR.Body).Decode(&reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if reset.Status != "reset" || reset.Cleared != 1 {
		t.Fatalf("reset result = %#v, want one cleared state", reset)
	}
	if len(s.pkce) != 0 {
		t.Fatalf("reset left %d PKCE states", len(s.pkce))
	}

	close(release)
	if rr := waitForPKCECallback(t, done); rr.Code != http.StatusConflict {
		t.Fatalf("reset-invalidated callback status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if accounts := store.List(); len(accounts) != 0 {
		t.Fatalf("reset-invalidated callback persisted %d account(s)", len(accounts))
	}
	status := pkceStatusForTest(t, s, "reset-state")
	if status["status"] != "expired" || status["terminal"] != true {
		t.Fatalf("reset state status = %#v, want terminal expired", status)
	}
}

// Two callbacks for the same state race frequently when a browser refreshes
// the loopback page. Exactly one may exchange and commit the account.
func TestCallbackPKCEClaimsStateOnceDuringExchange(t *testing.T) {
	store := openPKCECallbackTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	s := &Server{
		pkceAttempt: 1,
		pkce: map[string]pendingPKCE{
			"once-state": {Verifier: "once-verifier", Created: time.Now(), Attempt: 1, Status: "pending"},
		},
		tokens: store,
		exchangePKCECode: blockingPKCEExchange(started, release, auth.TokenSet{
			AccessToken: "unit-test-access-token",
			Email:       "once@example.test",
			HomeOID:     "once-account",
			ExpiresAt:   time.Now().Add(time.Hour),
		}),
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=once-state&code=first-code", nil))
		firstDone <- rr
	}()
	waitForPKCEExchange(t, started)

	secondRR := httptest.NewRecorder()
	s.callbackPKCE(secondRR, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=once-state&code=second-code", nil))
	if secondRR.Code != http.StatusConflict {
		t.Fatalf("second callback status = %d, want 409; body = %s", secondRR.Code, secondRR.Body.String())
	}

	close(release)
	if rr := waitForPKCECallback(t, firstDone); rr.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if accounts := store.List(); len(accounts) != 1 {
		t.Fatalf("committed account count = %d, want 1", len(accounts))
	}
	status := pkceStatusForTest(t, s, "once-state")
	if status["status"] != "authenticated" || status["terminal"] != true {
		t.Fatalf("completed status = %#v, want authenticated terminal", status)
	}
}
