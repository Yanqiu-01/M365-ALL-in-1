package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// panelTestServer 造一个带有效管理会话的最小 Server，用于直接打面板路由。
func panelTestServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	server := &Server{adminSessions: map[string]time.Time{"panel-test-session": time.Now().Add(time.Hour)}}
	return server, &http.Cookie{Name: "m365_admin_session", Value: "panel-test-session"}
}

func panelRequest(t *testing.T, server *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	newNativePanelController(server, newNativePanelManager(nativePanelConfig{})).ServeHTTP(recorder, request)
	return recorder
}

// job/poll 与 job/stop 随 Python 子进程一并移除，必须仍给出 501。
// 注册与批量 OAuth 已经改成 Go 实现，不再走这条路径。
func TestNativePanelRemovedRoutesAnswer501WithReason(t *testing.T) {
	server, cookie := panelTestServer(t)
	stop := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/job/stop", `{}`)
	if stop.Code != http.StatusNotImplemented {
		t.Errorf("job/stop status = %d, want 501", stop.Code)
	}
	if !strings.Contains(stop.Body.String(), "feature_removed") {
		t.Errorf("job/stop body missing feature_removed: %s", stop.Body.String())
	}
	if strings.Contains(stop.Body.String(), "Python 工作者未配置") {
		t.Errorf("job/stop 仍在提示依赖 Python 工作者: %s", stop.Body.String())
	}
	recorder := panelRequest(t, server, cookie, http.MethodGet, "/api/admin/panel/job/poll", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Errorf("job/poll status = %d, want 501", recorder.Code)
	}
}

// 所有面板路由（含已移除的）都必须注册，否则旧客户端会拿到 ServeMux 的裸 404。
func TestRegisterNativePanelRoutesCoversRemovedPaths(t *testing.T) {
	server, cookie := panelTestServer(t)
	mux := http.NewServeMux()
	server.RegisterNativePanelRoutes(mux)
	for _, path := range []string{
		"/api/admin/panel/state",
		"/api/admin/panel/oauth",
		"/api/admin/panel/register",
		"/api/admin/panel/oauth/batch",
		"/api/admin/panel/job/poll",
		"/api/admin/panel/job/stop",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound && strings.Contains(recorder.Body.String(), "404 page not found") {
			t.Errorf("%s 未注册到 ServeMux", path)
		}
	}
}

// 数据目录缺失时状态要如实报告。注册与批量 OAuth 的 Go 实现仍然可用，
// 但 native_panel_ready 必须为 false，避免界面假装已经读到账密清单。
func TestNativePanelStateReportsUnsupportedFeatures(t *testing.T) {
	manager := newNativePanelManager(nativePanelConfig{Root: filepath.Join(t.TempDir(), "missing")})
	state := manager.state(nil)
	for _, key := range []string{"register_supported", "batch_oauth_supported"} {
		if value, ok := state[key].(bool); !ok || !value {
			t.Errorf("state[%q] = %v, want true", key, state[key])
		}
	}
	if ready, _ := state["native_panel_ready"].(bool); ready {
		t.Error("缺失数据目录时 native_panel_ready 应为 false")
	}
	if _, exists := state["job"]; exists {
		t.Error("任务机制已移除，state 不应再返回 job 字段")
	}
}

// 数据目录存在时，账密清单与号段仍要读出来：账密补齐依赖同一份清单。
func TestNativePanelStateReadsCredentialsFromDataDir(t *testing.T) {
	root := t.TempDir()
	config := `{"gateway":{"host":"127.0.0.1","port":4141},"register":{"email_prefix":"24s05","email_domain":"office.bo.edu.kg","cred_file":"data/credentials.txt"}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	credentials := "24s055026@office.bo.edu.kg----pw1\n24s055100@office.bo.edu.kg----pw2\n"
	if err := os.WriteFile(filepath.Join(root, "data", "credentials.txt"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}

	state := newNativePanelManager(nativePanelConfig{Root: root}).state(nil)
	if ready, _ := state["native_panel_ready"].(bool); !ready {
		t.Fatalf("native_panel_ready = %v, want true (error=%v)", state["native_panel_ready"], state["native_panel_error"])
	}
	if state["cred_total"] != 2 {
		t.Errorf("cred_total = %v, want 2", state["cred_total"])
	}
	if state["cred_num_min"] != 5026 || state["cred_num_max"] != 5100 {
		t.Errorf("号段 = (%v,%v), want (5026,5100)", state["cred_num_min"], state["cred_num_max"])
	}
	if state["gw_url"] != "http://127.0.0.1:4141" {
		t.Errorf("gw_url = %v", state["gw_url"])
	}
}

// 单账号授权走 Go 内置 PKCE：既不要求本机装 Python，也不能假装已经完成。
func TestNativePanelOAuthUsesGoPKCE(t *testing.T) {
	server, cookie := panelTestServer(t)
	recorder := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/oauth", `{"email":"user@example.com"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("oauth status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["mode"] != "pkce" || payload["status"] != "manual_step_required" {
		t.Errorf("unexpected oauth payload: %#v", payload)
	}
	if complete, _ := payload["complete"].(bool); complete {
		t.Error("授权尚未回调，complete 不能为 true")
	}
	if url, _ := payload["authorizationUrl"].(string); !strings.Contains(url, "oauth2") {
		t.Errorf("authorizationUrl = %q，应为 Microsoft 授权端点", url)
	}

	// 邮箱格式仍要校验，避免把明显无效的输入送进授权流程。
	if bad := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/oauth", `{"email":"not-an-email"}`); bad.Code != http.StatusBadRequest {
		t.Errorf("invalid email status = %d, want 400", bad.Code)
	}
}

func TestPersistedNativePanelConfigPrefersEnvThenSettings(t *testing.T) {
	t.Setenv(nativePanelRootEnv, "")
	server := &Server{settings: &settingsStore{v: runtimeSettings{
		NativePanelRoot: filepath.Join("E:", "persisted", "panel-data"),
	}}}

	// A restart with no environment variable must still find the panel data.
	saved := persistedNativePanelConfig(server)
	if saved.Root != filepath.Join("E:", "persisted", "panel-data") {
		t.Fatalf("persisted root not used: %q", saved.Root)
	}

	// An explicit environment override still wins over the stored value.
	t.Setenv(nativePanelRootEnv, filepath.Join("E:", "env", "panel-data"))
	if overridden := persistedNativePanelConfig(server); overridden.Root != filepath.Join("E:", "env", "panel-data") {
		t.Fatalf("environment override ignored: %q", overridden.Root)
	}

	// A server without settings must not panic and must keep a data root.
	if fallback := persistedNativePanelConfig(nil); fallback.Root == "" {
		t.Fatal("nil server must still yield a usable panel data root")
	}
}
