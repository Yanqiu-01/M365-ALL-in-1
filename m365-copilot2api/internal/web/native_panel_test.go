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

// job/poll 随 Python 子进程一并移除，必须仍注册并给出 501；job/stop 保留为活路由，
// 用来取消进行中的 WebView 求解。注册与批量 OAuth 已改成 Go 实现，不走这条路径。
//
// 这个用例会真的进到 turnstile.Cancel()，而它按 M365_DATA_DIR 决定协作目录并往里
// 写 cancel / result 两个文件。因此必须自己把这个变量指到 TempDir：否则测试会写进
// 开发机上真实的数据目录，还会取消一个正在跑的求解。
func TestNativePanelRemovedRoutesAnswer501WithReason(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	server, cookie := panelTestServer(t)
	stop := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/job/stop", `{}`)
	if stop.Code != http.StatusOK {
		t.Errorf("job/stop status = %d, want 200 body=%s", stop.Code, stop.Body.String())
	}
	// 协作目录存在，取消信号就该真的落下去，stopped 必须为真。
	if !strings.Contains(stop.Body.String(), `"stopped":true`) && !strings.Contains(stop.Body.String(), `"stopped": true`) {
		t.Errorf("job/stop body missing stopped: %s", stop.Body.String())
	}
	recorder := panelRequest(t, server, cookie, http.MethodGet, "/api/admin/panel/job/poll", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Errorf("job/poll status = %d, want 501", recorder.Code)
	}
}

// 反过来：协作目录不可用时（桌面端默认就是这样，M365_DATA_DIR 未设），取消什么也
// 做不了，接口必须如实回 stopped:false，而不是谎报任务已停。
func TestNativePanelJobStopReportsHonestlyWithoutDataDir(t *testing.T) {
	t.Setenv("M365_DATA_DIR", "")
	server, cookie := panelTestServer(t)
	stop := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/job/stop", `{}`)
	if stop.Code != http.StatusOK {
		t.Fatalf("job/stop status = %d, want 200 body=%s", stop.Code, stop.Body.String())
	}
	body := stop.Body.String()
	if strings.Contains(body, `"stopped":true`) || strings.Contains(body, `"stopped": true`) {
		t.Errorf("job/stop claimed it stopped a job while it had nowhere to write: %s", body)
	}
	if !strings.Contains(body, "detail") {
		t.Errorf("an unstoppable job should explain why: %s", body)
	}
}

// 所有面板路由（含已移除的）都必须注册，否则旧客户端会拿到 ServeMux 的裸 404。
//
// 这里会经由 mux 走到真正的 state()，它会创建面板数据目录并写入一份默认
// config.json；job/stop 还会进 turnstile.Cancel()。两者都必须锁进 TempDir，否则
// 测试会在开发机的真实数据目录里留下文件。
func TestRegisterNativePanelRoutesCoversRemovedPaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv(nativePanelRootEnv, root)
	t.Setenv("M365_DATA_DIR", root)
	server, cookie := panelTestServer(t)
	mux := http.NewServeMux()
	server.RegisterNativePanelRoutes(mux)
	for _, path := range []string{
		"/api/admin/panel/state",
		"/api/admin/panel/oauth",
		"/api/admin/panel/register",
		"/api/admin/panel/config",
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

// 数据目录即使一开始不存在也会被创建。注册与批量 OAuth 的 Go 实现始终可用。
func TestNativePanelStateReportsUnsupportedFeatures(t *testing.T) {
	manager := newNativePanelManager(nativePanelConfig{Root: filepath.Join(t.TempDir(), "missing")})
	state := manager.state(nil)
	for _, key := range []string{"register_supported", "batch_oauth_supported"} {
		if value, ok := state[key].(bool); !ok || !value {
			t.Errorf("state[%q] = %v, want true", key, state[key])
		}
	}
	if ready, _ := state["native_panel_ready"].(bool); !ready {
		t.Fatalf("auto-created data dir should be ready: %#v", state)
	}
	if _, exists := state["job"]; exists {
		t.Error("任务机制已移除，state 不应再返回 job 字段")
	}
}

func TestNativePanelPathsCreatesMissingDataDir(t *testing.T) {
	// 默认配置里的注册密码只从环境变量注入，源码不留凭据。测试必须自己决定这个
	// 变量，否则「注册是否就绪」会取决于开发机上恰好有没有设过它。
	t.Setenv("M365_REGISTER_PASSWORD", "test-only-register-password")
	root := filepath.Join(t.TempDir(), "panel-data")
	paths, err := nativePanelConfig{Root: root}.paths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.root, "config.json")); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}
	state := newNativePanelManager(nativePanelConfig{Root: root}).state(nil)
	if ready, _ := state["native_panel_ready"].(bool); !ready {
		t.Fatalf("auto-created data dir should be ready: %#v", state)
	}
	if ready, _ := state["register_ready"].(bool); !ready {
		t.Fatalf("default register config should be ready: %#v", state)
	}
	if state["site_url"] != "https://office.965007.xyz" || state["email_prefix"] != "24s05" {
		t.Fatalf("default register fields = %#v", state)
	}
	if state["flaresolverr_url"] != "http://127.0.0.1:8191/v1" {
		t.Fatalf("flaresolverr_url = %v", state["flaresolverr_url"])
	}
}

func TestNativePanelSaveRegisterConfig(t *testing.T) {
	root := t.TempDir()
	manager := newNativePanelManager(nativePanelConfig{Root: root})
	cfg, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		SiteURL: "https://office.example.test/", EmailDomain: "@office.example.test",
		EmailPrefix: "user", Password: "changed", EmailStartNum: 2000,
		FlareSolverrURL: "http://127.0.0.1:18191/v1",
		PhoneSOCKS:      "socks5://127.0.0.1:1080", ClashAPI: "http://127.0.0.1:9090/",
		ClashSecret: "secret", ClashGroup: "GLOBAL", ClashProxy: "socks5://127.0.0.1:7891",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Register.SiteURL != "https://office.example.test" {
		t.Fatalf("site url = %q", cfg.Register.SiteURL)
	}
	if cfg.Register.EmailDomain != "office.example.test" || cfg.Register.EmailPrefix != "user" || cfg.Register.Password != "changed" {
		t.Fatalf("saved = %#v", cfg.Register)
	}
	if cfg.Register.EmailStartNum != 2000 {
		t.Fatalf("start num = %d", cfg.Register.EmailStartNum)
	}
	if cfg.Register.FlareSolverrURL != "http://127.0.0.1:18191/v1" {
		t.Fatalf("flaresolverr = %q", cfg.Register.FlareSolverrURL)
	}
	if cfg.Register.PhoneSOCKS != "socks5://127.0.0.1:1080" {
		t.Fatalf("phone socks = %q", cfg.Register.PhoneSOCKS)
	}
	if cfg.Register.ClashAPI != "http://127.0.0.1:9090" || cfg.Register.ClashSecret != "secret" || cfg.Register.ClashGroup != "GLOBAL" || cfg.Register.ClashProxy != "socks5://127.0.0.1:7891" {
		t.Fatalf("saved phone registration config = %#v", cfg.Register)
	}
}

func TestNativePanelLoadFillsEmptyRegisterFields(t *testing.T) {
	// 同上：空密码要由默认值补齐，而默认值本身来自环境变量。
	t.Setenv("M365_REGISTER_PASSWORD", "test-only-register-password")
	root := t.TempDir()
	empty := `{"gateway":{"host":"127.0.0.1","port":4141},"register":{"email_domain":"","email_prefix":"","password":"","cred_file":"credentials.txt"}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(empty), 0o600); err != nil {
		t.Fatal(err)
	}
	state := newNativePanelManager(nativePanelConfig{Root: root}).state(nil)
	if ready, _ := state["register_ready"].(bool); !ready {
		t.Fatalf("empty config should be filled with defaults: %#v", state)
	}
	if state["site_url"] != "https://office.965007.xyz" {
		t.Fatalf("site_url = %v", state["site_url"])
	}
	if state["flaresolverr_url"] != "http://127.0.0.1:8191/v1" {
		t.Fatalf("flaresolverr_url = %v", state["flaresolverr_url"])
	}
}

// 没有人配过注册密码时，面板不得声称注册就绪。上面两个用例显式注入了环境变量，
// 这一条把相反方向钉住：源码里不带凭据，缺密码就必须是未就绪，否则界面会在无法
// 真正注册的状态下放出注册入口。
func TestRegisterNotReadyWhenPasswordUnset(t *testing.T) {
	t.Setenv("M365_REGISTER_PASSWORD", "")
	root := filepath.Join(t.TempDir(), "panel-data")
	state := newNativePanelManager(nativePanelConfig{Root: root}).state(nil)
	if ready, _ := state["native_panel_ready"].(bool); !ready {
		t.Fatalf("panel itself should still be ready: %#v", state)
	}
	if ready, _ := state["register_ready"].(bool); ready {
		t.Error("register_ready must be false when no register password is configured")
	}
	if pw, _ := state["register_password"].(string); pw != "" {
		t.Errorf("register_password = %q, want empty", pw)
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
