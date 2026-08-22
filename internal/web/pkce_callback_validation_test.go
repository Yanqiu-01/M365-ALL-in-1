package web

import (
	"m365-copilot2api/internal/auth"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestCallbackPKCERejectsAmbiguousParametersBeforeStateConsumption(t *testing.T) {
	pasted := url.QueryEscape("https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&code=provider-code")
	tests := []struct {
		name string
		path string
	}{
		{name: "duplicate state", path: "/api/auth/callback?state=state&state=other&code=code"},
		{name: "duplicate code", path: "/api/auth/callback?state=state&code=one&code=two"},
		{name: "code and error", path: "/api/auth/callback?state=state&code=code&error=access_denied"},
		{name: "pasted and direct code", path: "/api/auth/callback?url=" + pasted + "&code=code"},
		{name: "duplicate pasted URL", path: "/api/auth/callback?url=" + pasted + "&url=" + pasted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{pkce: map[string]pendingPKCE{
				"state": {Verifier: "verifier", Created: time.Now(), Status: "pending", RedirectURI: auth.DefaultRedirectURI},
			}, exchangeCode: func(_, _, _ string) (auth.TokenSet, error) {
				t.Fatal("ambiguous callback reached code exchange")
				return auth.TokenSet{}, nil
			}}
			rr := httptest.NewRecorder()
			s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if got := s.pkce["state"].Status; got != "pending" {
				t.Fatalf("ambiguous callback changed pending state to %q", got)
			}
		})
	}
}

func TestCallbackPKCERevalidatesPendingRedirect(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"state": {Verifier: "verifier", Created: time.Now(), Status: "pending", RedirectURI: "http://127.0.0.1:4141/api/auth/callback"},
	}, exchangeCode: func(_, _, _ string) (auth.TokenSet, error) {
		t.Fatal("unsafe pending redirect reached code exchange")
		return auth.TokenSet{}, nil
	}}
	rr := httptest.NewRecorder()
	s.callbackPKCE(rr, httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=state&code=code", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := s.pkce["state"].Status; got != "error" {
		t.Fatalf("pending state status = %q, want error", got)
	}
}

func TestStartPKCERejectsUnsafeTokenEndpoint(t *testing.T) {
	t.Setenv("M365_BROWSER_AUTHORITY", "")
	t.Setenv("M365_AUTHORITY", "")
	t.Setenv("M365_BROWSER_REDIRECT_URI", "")
	t.Setenv("M365_REDIRECT_URI", "")
	t.Setenv("M365_AUTHORIZE_ENDPOINT", "")
	t.Setenv("M365_TOKEN_ENDPOINT", "https://example.test/common/oauth2/v2.0/token")
	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	s.startPKCE(rr, httptest.NewRequest(http.MethodPost, "/api/auth/start", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(s.pkce) != 0 {
		t.Fatal("unsafe token endpoint created pending PKCE state")
	}
}
