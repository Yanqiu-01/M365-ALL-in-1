package auth

import (
	"context"
	"testing"
)

func TestValidateDeviceCodeEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "common", endpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode"},
		{name: "tenant", endpoint: "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/devicecode"},
		{name: "look alike host", endpoint: "https://login.microsoftonline.com.example.test/common/oauth2/v2.0/devicecode", wantErr: true},
		{name: "wrong operation", endpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/token", wantErr: true},
		{name: "non HTTPS", endpoint: "http://login.microsoftonline.com/common/oauth2/v2.0/devicecode", wantErr: true},
		{name: "query", endpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode?x=1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeviceCodeEndpoint(tc.endpoint)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateDeviceCodeEndpoint() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestDeviceFlowRejectsUnsafeConfiguredEndpointsBeforeNetwork(t *testing.T) {
	t.Run("device code", func(t *testing.T) {
		t.Setenv("M365_DEVICE_ENDPOINT", "https://example.test/common/oauth2/v2.0/devicecode")
		if _, err := StartDeviceCodeContext(context.Background()); err == nil {
			t.Fatal("unsafe device-code endpoint was accepted")
		}
	})
	t.Run("device token", func(t *testing.T) {
		t.Setenv("M365_DEVICE_TOKEN_ENDPOINT", "https://example.test/common/oauth2/v2.0/token")
		if _, _, err := PollDeviceCodeContext(context.Background(), "synthetic-device-code"); err == nil {
			t.Fatal("unsafe device-token endpoint was accepted")
		}
	})
}
