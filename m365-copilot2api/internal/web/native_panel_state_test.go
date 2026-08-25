package web

import (
	"fmt"
	"path/filepath"
	"testing"

	"m365-copilot2api/internal/auth"
)

func TestNativePanelStateReturnsCompleteGatewayEmailSet(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	const total = 205
	for i := 0; i < total; i++ {
		email := fmt.Sprintf("account-%03d@example.test", i)
		if _, err := store.Upsert(auth.TokenSet{Email: email, HomeOID: email}); err != nil {
			t.Fatalf("upsert %s: %v", email, err)
		}
	}

	manager := newNativePanelManager(nativePanelConfig{}, nil)
	state := manager.state(&Server{tokens: store})
	emails, ok := state["gw_emails"].([]string)
	if !ok {
		t.Fatalf("gw_emails type = %T, want []string", state["gw_emails"])
	}
	if len(emails) != total {
		t.Fatalf("gw_emails length = %d, want %d", len(emails), total)
	}
	if got := state["gw_online"]; got != total {
		t.Fatalf("gw_online = %v, want %d", got, total)
	}
	if emails[0] != "account-000@example.test" || emails[total-1] != "account-204@example.test" {
		t.Fatalf("unexpected sorted bounds: first=%q last=%q", emails[0], emails[total-1])
	}
}
