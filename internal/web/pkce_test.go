package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestStartPKCEUsesBrowserClientDefaults(t *testing.T) {
	t.Setenv("M365_CLIENT_ID", "")
	t.Setenv("M365_AUTHORITY", "")
	t.Setenv("M365_REDIRECT_URI", "")

	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/start", nil)
	r.Host = "172.30.0.214"
	r.Header.Set("X-Forwarded-Host", "unregistered.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	s.startPKCE(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response struct {
		State       string `json:"state"`
		URL         string `json:"url"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.State == "" {
		t.Fatal("response omitted state")
	}
	if got, want := response.RedirectURI, "https://login.microsoftonline.com/common/oauth2/nativeclient"; got != want {
		t.Fatalf("redirect URI = %q, want %q", got, want)
	}
	u, err := url.Parse(response.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Query().Get("client_id"), "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"; got != want {
		t.Fatalf("client_id = %q, want %q", got, want)
	}
	if got := u.Query().Get("redirect_uri"); got != response.RedirectURI {
		t.Fatalf("authorization redirect URI = %q, response redirect URI = %q", got, response.RedirectURI)
	}
}

func TestCallbackPKCERejectsMissingUnknownExpiredAndConsumedState(t *testing.T) {
	tests := []struct {
		name   string
		state  string
		entry  pendingPKCE
		add    bool
		status int
	}{
		{name: "missing", status: http.StatusBadRequest},
		{name: "unknown", state: "unknown", status: http.StatusBadRequest},
		{name: "expired", state: "expired", entry: pendingPKCE{Created: time.Now().Add(-11 * time.Minute), Status: "pending"}, add: true, status: http.StatusBadRequest},
		{name: "consumed", state: "consumed", entry: pendingPKCE{Created: time.Now(), Status: "authenticated"}, add: true, status: http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{pkce: map[string]pendingPKCE{}}
			if tc.add {
				s.pkce[tc.state] = tc.entry
			}
			path := "/api/auth/callback"
			if tc.state != "" {
				path += "?state=" + url.QueryEscape(tc.state) + "&code=code"
			}
			rr := httptest.NewRecorder()
			s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestCallbackPKCERejectsUntrustedPastedURLWithoutConsumingState(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			name: "non-Microsoft host",
			url:  "https://example.test/common/oauth2/nativeclient?state=state&error=access_denied",
		},
		{
			name: "wrong Microsoft path",
			url:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=state&error=access_denied",
		},
		{
			name: "duplicate state",
			url:  "https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&state=second&error=access_denied",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{pkce: map[string]pendingPKCE{
				"state": {Created: time.Now(), Status: "pending"},
			}}
			rr := httptest.NewRecorder()
			s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?url="+url.QueryEscape(tc.url), nil))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if got := s.pkce["state"].Status; got != "pending" {
				t.Fatalf("unsafe pasted URL changed state to %q", got)
			}
		})
	}
}

func TestCallbackPKCEAcceptsTrustedPastedMicrosoftNativeClientError(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"state": {Created: time.Now(), Status: "pending"},
	}}
	callbackURL := "https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&error=access_denied"
	rr := httptest.NewRecorder()
	s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?url="+url.QueryEscape(callbackURL), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := s.pkce["state"].Status; got != "error" {
		t.Fatalf("trusted provider denial state = %q, want error", got)
	}
}

func TestCallbackPKCEConsumesMicrosoftErrorOnce(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"state": {Created: time.Now(), Status: "pending"},
	}}
	path := "/api/auth/callback?state=state&error=access_denied"
	first := httptest.NewRecorder()
	s.callbackPKCE(first, httptest.NewRequest(http.MethodGet, path, nil))
	if first.Code != http.StatusBadRequest {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	s.callbackPKCE(second, httptest.NewRequest(http.MethodGet, path, nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
}

func TestCallbackPKCENativeClientCallbackDoesNotExposeAuthorizationResult(t *testing.T) {
	const code = "sensitive-authorization-code"
	store, err := auth.OpenStore(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, pkce: map[string]pendingPKCE{
		"state": {Verifier: "verifier", Created: time.Now(), Status: "pending", RedirectURI: auth.DefaultRedirectURI},
	}, exchangeCode: func(gotCode, verifier, redirectURI string) (auth.TokenSet, error) {
		if gotCode != code || verifier != "verifier" || redirectURI != auth.DefaultRedirectURI {
			t.Fatalf("unexpected code exchange arguments")
		}
		return auth.TokenSet{AccessToken: "header.payload.signature", RefreshToken: "sensitive-refresh-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	rr := httptest.NewRecorder()
	s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=state&code="+url.QueryEscape(code), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{code, "sensitive-refresh-token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("callback response exposed sensitive value %q", secret)
		}
	}
}

func TestCallbackPKCEFailureDoesNotExposeAuthorizationCode(t *testing.T) {
	const code = "sensitive-failed-code"
	s := &Server{pkce: map[string]pendingPKCE{
		"state": {Verifier: "verifier", Created: time.Now(), Status: "pending", RedirectURI: auth.DefaultRedirectURI},
	}, exchangeCode: func(_, _, _ string) (auth.TokenSet, error) {
		return auth.TokenSet{}, &auth.OAuthError{Code: "invalid_grant"}
	}}
	rr := httptest.NewRecorder()
	s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=state&code="+code, nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), code) || strings.Contains(s.pkce["state"].Error, code) {
		t.Fatal("failed exchange exposed authorization code")
	}
}

func TestStartPKCERejectsUnsafeConfiguredTargets(t *testing.T) {
	tests := []struct {
		name              string
		authorizeEndpoint string
		redirectURI       string
	}{
		{
			name:              "non-Microsoft authorization endpoint",
			authorizeEndpoint: "https://example.test/common/oauth2/v2.0/authorize",
		},
		{
			name:        "non-Microsoft redirect URI",
			redirectURI: "https://app.example.test/api/auth/callback",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("M365_BROWSER_AUTHORITY", "")
			t.Setenv("M365_AUTHORITY", "")
			t.Setenv("M365_BROWSER_REDIRECT_URI", "")
			t.Setenv("M365_REDIRECT_URI", tc.redirectURI)
			t.Setenv("M365_AUTHORIZE_ENDPOINT", tc.authorizeEndpoint)

			s := &Server{pkce: map[string]pendingPKCE{}}
			rr := httptest.NewRecorder()
			s.startPKCE(rr, httptest.NewRequest(http.MethodPost, "/api/auth/start", nil))

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if len(s.pkce) != 0 {
				t.Fatal("unsafe OAuth configuration created pending PKCE state")
			}
			if strings.Contains(rr.Body.String(), "example.test") {
				t.Fatal("unsafe OAuth configuration was reflected to the client")
			}
		})
	}
}

func TestPKCEStatusReportsPendingAndExpired(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"pending": {Created: time.Now(), Status: "pending"},
		"expired": {Created: time.Now().Add(-11 * time.Minute), Status: "pending"},
	}}

	for _, tc := range []struct {
		state string
		want  string
	}{
		{state: "pending", want: "pending"},
		{state: "expired", want: "expired"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.pkceStatus(rr, httptest.NewRequest(http.MethodGet, "/api/auth/status?state="+tc.state, nil))
			var response map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if got := response["status"]; got != tc.want {
				t.Fatalf("status = %v, want %q", got, tc.want)
			}
		})
	}
}
