package auth

import "testing"

func TestParseNativeClientCallbackURL(t *testing.T) {
	valid := "https://login.microsoftonline.com/common/oauth2/nativeclient?code=authorization-code&state=state-value"
	callback, err := ParseNativeClientCallbackURL(valid)
	if err != nil {
		t.Fatalf("valid Microsoft callback rejected: %v", err)
	}
	if callback.State != "state-value" || callback.Code != "authorization-code" || callback.Error != "" {
		t.Fatalf("unexpected callback: %#v", callback)
	}

	tests := []string{
		"https://example.test/common/oauth2/nativeclient?state=state&error=access_denied",
		"https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=state&error=access_denied",
		"https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&state=second&error=access_denied",
		"https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&code=one&error=access_denied",
		"https://login.microsoftonline.com/common/oauth2/nativeclient?code=authorization-code",
		"https://login.microsoftonline.com/common/oauth2/nativeclient?state=state&error=access_denied#fragment",
	}
	for _, raw := range tests {
		if _, err := ParseNativeClientCallbackURL(raw); err == nil {
			t.Fatalf("unsafe callback accepted: %s", raw)
		}
	}
}

func TestValidateAuthorizationTarget(t *testing.T) {
	validEndpoint := "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	validRedirect := "https://login.microsoftonline.com/common/oauth2/nativeclient"
	if err := ValidateAuthorizationTarget(validEndpoint, validRedirect); err != nil {
		t.Fatalf("default Microsoft target rejected: %v", err)
	}

	tests := []struct {
		name     string
		endpoint string
		redirect string
	}{
		{"non-Microsoft endpoint", "https://example.com/common/oauth2/v2.0/authorize", validRedirect},
		{"non-HTTPS endpoint", "http://login.microsoftonline.com/common/oauth2/v2.0/authorize", validRedirect},
		{"lookalike endpoint", "https://login.microsoftonline.com.example.test/common/oauth2/v2.0/authorize", validRedirect},
		{"endpoint userinfo", "https://login.microsoftonline.com@evil.example/common/oauth2/v2.0/authorize", validRedirect},
		{"endpoint explicit port", "https://login.microsoftonline.com:444/common/oauth2/v2.0/authorize", validRedirect},
		{"endpoint query", "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?x=1", validRedirect},
		{"wrong endpoint path", "https://login.microsoftonline.com/common/oauth2/v2.0/token", validRedirect},
		{"non-Microsoft redirect", validEndpoint, "https://example.com/common/oauth2/nativeclient"},
		{"redirect query", validEndpoint, "https://login.microsoftonline.com/common/oauth2/nativeclient?x=1"},
		{"redirect fragment", validEndpoint, "https://login.microsoftonline.com/common/oauth2/nativeclient#x"},
		{"wrong redirect path", validEndpoint, "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAuthorizationTarget(tc.endpoint, tc.redirect); err == nil {
				t.Fatal("unsafe OAuth target was accepted")
			}
		})
	}
}
