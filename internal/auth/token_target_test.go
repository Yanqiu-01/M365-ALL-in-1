package auth

import (
	"context"
	"net/url"
	"testing"
)

func TestValidateTokenEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "common", endpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/token"},
		{name: "tenant", endpoint: "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token"},
		{name: "look alike host", endpoint: "https://login.microsoftonline.com.example.test/common/oauth2/v2.0/token", wantErr: true},
		{name: "wrong operation", endpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize", wantErr: true},
		{name: "non HTTPS", endpoint: "http://login.microsoftonline.com/common/oauth2/v2.0/token", wantErr: true},
		{name: "port", endpoint: "https://login.microsoftonline.com:443/common/oauth2/v2.0/token", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTokenEndpoint(tc.endpoint)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateTokenEndpoint() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestRequestTokenContextRejectsUnsafeConfiguredEndpointBeforeNetwork(t *testing.T) {
	t.Setenv("M365_TOKEN_ENDPOINT", "https://example.test/common/oauth2/v2.0/token")
	_, err := requestTokenContext(context.Background(), url.Values{"grant_type": {"authorization_code"}})
	if err == nil {
		t.Fatal("unsafe configured token endpoint was accepted")
	}
}
