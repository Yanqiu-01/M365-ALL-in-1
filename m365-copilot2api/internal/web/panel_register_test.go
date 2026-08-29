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
)

func writePanelConfig(t *testing.T, root, siteURL string) {
	t.Helper()
	config := map[string]any{
		"gateway": map[string]any{"host": "127.0.0.1", "port": 4141},
		"register": map[string]any{
			"site_url":        siteURL,
			"email_domain":    "office.example.test",
			"email_prefix":    "24s05",
			"password":        "Passw0rd!",
			"email_start_num": 5026,
			"cred_file":       "data/credentials.txt",
		},
	}
	body, _ := json.Marshal(config)
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunRegisterPostsAndWritesCredentials(t *testing.T) {
	var got map[string]any
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/register" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "24s055026@office.example.test"})
	}))
	defer site.Close()

	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	server := &Server{}
	manager := newNativePanelManager(nativePanelConfig{Root: root})
	report, err := server.runRegister(context.Background(), manager, panelRegisterRequest{
		Mode: "proxy", Count: 1, StartNum: 5026, TurnstileToken: "token-from-browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Success != 1 || len(report.Accounts) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if got["username"] != "24s055026" || got["turnstileToken"] != "token-from-browser" {
		t.Fatalf("payload = %#v", got)
	}
	stored, err := nativePanelReadCredentials(filepath.Join(root, "data", "credentials.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if stored["24s055026@office.example.test"] != "Passw0rd!" {
		t.Fatalf("credentials = %#v", stored)
	}
}

func TestRunRegisterRequiresTurnstileToken(t *testing.T) {
	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	_, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{Mode: "proxy", Count: 1})
	if err == nil || !strings.Contains(err.Error(), "turnstileToken") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmailsInRange(t *testing.T) {
	creds := map[string]string{
		"24s055026@office.example.test": "a",
		"24s055030@office.example.test": "b",
		"24s055100@office.example.test": "c",
	}
	got, err := emailsInRange(creds, "24s05", 5026, 5030)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "24s055026@office.example.test" {
		t.Fatalf("got %#v", got)
	}
}
