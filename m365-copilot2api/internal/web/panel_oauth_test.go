package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"m365-copilot2api/internal/auth"
)

func TestRunOAuthBatchImportsViaROPC(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/organizations/oauth2/v2.0/token" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		form := string(body)
		if !strings.Contains(form, "grant_type=password") || !strings.Contains(form, "user%40example.test") {
			t.Errorf("unexpected form %s", form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid",
		})
	}))
	defer tokenServer.Close()
	t.Setenv("M365_AUTHORITY", tokenServer.URL+"/common")

	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	if err := os.WriteFile(filepath.Join(root, "data", "credentials.txt"), []byte("user@example.test----secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{tokens: store}
	report, err := server.runOAuthBatch(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelOAuthBatchRequest{
		Emails: []string{"user@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Imported != 1 || report.Accounts[0].Mode != "ropc" {
		t.Fatalf("report = %#v", report)
	}
	if _, ok := store.Get("user@example.test"); !ok {
		t.Fatal("account was not upserted")
	}
}

func TestRunOAuthBatchSkipsExistingWhenResume(t *testing.T) {
	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	if err := os.WriteFile(filepath.Join(root, "data", "credentials.txt"), []byte("user@example.test----secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{Email: "user@example.test", HomeOID: "user@example.test"}); err != nil {
		t.Fatal(err)
	}
	report, err := (&Server{tokens: store}).runOAuthBatch(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelOAuthBatchRequest{
		Emails: []string{"user@example.test"},
		Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 1 || report.Imported != 0 {
		t.Fatalf("report = %#v", report)
	}
}
