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

	"m365-copilot2api/internal/exitrotate"
)

func stubRotate(t *testing.T) {
	t.Helper()
	old := rotateExit
	t.Cleanup(func() { rotateExit = old })
	rotateExit = func(ctx context.Context, req exitrotate.Request) (exitrotate.Result, error) {
		return exitrotate.Result{OK: true, Mode: req.Mode, IP: "1.1.1.1"}, nil
	}
}

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

func TestRunRegisterDefaultsToProxyMode(t *testing.T) {
	stubRotate(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	report, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{
		Count: 1, StartNum: 5026, TurnstileToken: "token-from-browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != "proxy" {
		t.Fatalf("mode = %q", report.Mode)
	}
}

func TestRunRegisterAcceptsInPageSubmit(t *testing.T) {
	stubRotate(t)
	posted := 0
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	report, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{
		Mode: "proxy", Count: 1, StartNum: 5026, TurnstileToken: "SUBMITTED:24s055026@office.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if posted != 0 {
		t.Fatalf("in-page submit still posted /api/register %d times", posted)
	}
	if !report.OK || report.Success != 1 {
		t.Fatalf("report = %#v", report)
	}
	stored, err := nativePanelReadCredentials(filepath.Join(root, "data", "credentials.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if stored["24s055026@office.example.test"] != "Passw0rd!" {
		t.Fatalf("credentials = %#v", stored)
	}
}

func TestRunRegisterPostsAndWritesCredentials(t *testing.T) {
	stubRotate(t)
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

func TestRunRegisterUsesFlareSolverrWhenTokenMissing(t *testing.T) {
	stubRotate(t)
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
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"solution": map[string]any{
				"response": `<input name="cf-turnstile-response" value="flare-solved-token-abcdefghij">`,
			},
		})
	}))
	defer flare.Close()

	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	cfgPath := filepath.Join(root, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	reg := cfg["register"].(map[string]any)
	reg["flaresolverr_url"] = flare.URL
	// 这个用例考的就是 FlareSolverr 那条路，所以要把后端钉住。默认（auto）在装了
	// Chrome/Edge 的机器上会走浏览器 —— 那时它会真的去开 site.URL，测不到这里想测的东西，
	// 而且结果会随机器而变。
	reg["solver"] = "flaresolverr"
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{Mode: "proxy", Count: 1, StartNum: 5026})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Success != 1 {
		t.Fatalf("report = %#v", report)
	}
	if got["turnstileToken"] != "flare-solved-token-abcdefghij" {
		t.Fatalf("payload = %#v", got)
	}
}

func TestRunRegisterReportsFlareSolverrFailure(t *testing.T) {
	stubRotate(t)
	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	cfgPath := filepath.Join(root, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	reg := cfg["register"].(map[string]any)
	reg["flaresolverr_url"] = "http://127.0.0.1:1/v1"
	reg["solver"] = "flaresolverr" // 同上：这条用例考的是 FlareSolverr 不可用时如实报错。
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{Mode: "proxy", Count: 1, StartNum: 5026})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Failed != 1 {
		t.Fatalf("report = %#v", report)
	}
	if report.Accounts[0].Detail == "" {
		t.Fatalf("detail = %q", report.Accounts[0].Detail)
	}
}

func TestRunRegisterRotatesOnlyAfterSuccessfulWrite(t *testing.T) {
	var events []string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "register")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	old := rotateExit
	t.Cleanup(func() { rotateExit = old })
	rotateExit = func(ctx context.Context, req exitrotate.Request) (exitrotate.Result, error) {
		events = append(events, req.Mode)
		return exitrotate.Result{OK: true, Mode: req.Mode, IP: "8.8.8.8"}, nil
	}

	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	_, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{
		Mode: "phone", Count: 2, StartNum: 5026, TurnstileToken: "token-from-browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"proxy", "register", "phone", "proxy", "register"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", events, want)
	}
}

func TestRunRegisterDoesNotRotateAfterFailure(t *testing.T) {
	var events []string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "register")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "taken"})
	}))
	defer site.Close()
	old := rotateExit
	t.Cleanup(func() { rotateExit = old })
	rotateExit = func(ctx context.Context, req exitrotate.Request) (exitrotate.Result, error) {
		events = append(events, req.Mode)
		return exitrotate.Result{OK: true, Mode: req.Mode, IP: "8.8.8.8"}, nil
	}
	root := t.TempDir()
	writePanelConfig(t, root, site.URL)
	report, err := (&Server{}).runRegister(context.Background(), newNativePanelManager(nativePanelConfig{Root: root}), panelRegisterRequest{
		Mode: "phone", Count: 2, StartNum: 5026, TurnstileToken: "token-from-browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Success != 0 || report.Failed != 2 {
		t.Fatalf("report = %#v", report)
	}
	for _, event := range events {
		if event == "phone" {
			t.Fatalf("airplane-mode ran after a failed register: %v", events)
		}
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
