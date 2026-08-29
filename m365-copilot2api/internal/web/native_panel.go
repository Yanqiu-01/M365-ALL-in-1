package web

// 原生面板：账号数据目录 + Go 内置注册 / OAuth。
//
// 历史：注册与批量 OAuth 曾由 4141 网关拉起 M365-自用/ 下的 Python 工作者
// （Register/*.py、oauth/*.py）完成。那些脚本依赖 Playwright、桌面 Chromium
// 与 Cloudflare Turnstile，已随工程删除。当前分工：
//
//   - 数据目录（config.json、账密清单）仍然读取，供面板状态与账密补齐使用。
//     目录位置由 M365_NATIVE_PANEL_ROOT 或已保存设置决定，属用户数据而非仓库代码。
//   - 批量 OAuth 走 Go：ROPC → Device Code → PKCE，不再驱动浏览器填账密。
//   - 注册走 Go：换 IP（Rust CLI 优先，Go 回退）+ POST /api/register。Turnstile
//     token 仍须由调用方从浏览器拿到，网关不再内嵌 Chromium。
//   - job/poll 与 job/stop 仍返回 501：已经没有可轮询的子进程。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/auth"
)

const (
	nativePanelRootEnv       = "M365_NATIVE_PANEL_ROOT"
	nativePanelMaxRequest    = 32 << 10
	nativePanelMaxConfig     = 1 << 20
	nativePanelMaxCredential = 4 << 20
	nativePanelMaxLine       = 256 << 10
)

// errNativePanelUnavailable 表示本机没有可用的面板数据目录。它不再与任何
// 外部运行时相关：目录缺失只影响账密清单与号段展示。
var errNativePanelUnavailable = errors.New("本地面板数据目录未配置或不可用")

// nativePanelConfig is process-local. Requests cannot choose a data directory
// or configuration path.
type nativePanelConfig struct {
	Root string
}

func defaultNativePanelConfig() nativePanelConfig {
	root := strings.TrimSpace(os.Getenv(nativePanelRootEnv))
	if root == "" {
		// Android GatewayService 把 M365_DATA_DIR 指到 files/gw/data，那才是可写目录。
		// 可执行文件在 nativeLibraryDir，旁边根本没有 M365-自用。
		root = strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	}
	if root == "" {
		if exe, err := os.Executable(); err == nil {
			root = filepath.Join(filepath.Dir(exe), "M365-自用")
		} else {
			root = "M365-自用"
		}
	}
	return nativePanelConfig{Root: root}
}

// persistedNativePanelConfig resolves the panel data location from saved
// settings so a plain restart keeps credentials and configuration available.
// The environment variable still takes precedence.
func persistedNativePanelConfig(server *Server) nativePanelConfig {
	config := defaultNativePanelConfig()
	if server == nil || server.settings == nil {
		return config
	}
	saved := server.settings.get()
	if strings.TrimSpace(os.Getenv(nativePanelRootEnv)) == "" {
		if root := strings.TrimSpace(saved.NativePanelRoot); root != "" {
			config.Root = root
		}
	}
	return config
}

type nativePanelPaths struct {
	root       string
	configPath string
}

func (c nativePanelConfig) paths() (nativePanelPaths, error) {
	root := strings.TrimSpace(c.Root)
	if root == "" {
		return nativePanelPaths{}, fmt.Errorf("%w: set %s or M365_DATA_DIR", errNativePanelUnavailable, nativePanelRootEnv)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nativePanelPaths{}, fmt.Errorf("%w: resolve data directory", errNativePanelUnavailable)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nativePanelPaths{}, fmt.Errorf("%w: create data directory: %v", errNativePanelUnavailable, err)
	}
	resolved := abs
	if link, err := filepath.EvalSymlinks(abs); err == nil && link != "" {
		resolved = link
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nativePanelPaths{}, fmt.Errorf("%w: data directory is unavailable", errNativePanelUnavailable)
	}
	configPath := filepath.Join(resolved, "config.json")
	if err := ensureNativePanelConfigFile(configPath); err != nil {
		return nativePanelPaths{}, err
	}
	return nativePanelPaths{root: resolved, configPath: configPath}, nil
}

func ensureNativePanelConfigFile(path string) error {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: config.json is unavailable", errNativePanelUnavailable)
	}
	body := []byte(`{
  "gateway": {"host": "127.0.0.1", "port": 4141},
  "register": {
    "email_domain": "",
    "email_prefix": "",
    "password": "",
    "cred_file": "credentials.txt"
  }
}
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("%w: write default config.json: %v", errNativePanelUnavailable, err)
	}
	return nil
}

// nativePanelFileConfig 只保留 Go 侧真正会读的字段：网关地址用于状态展示，
// register 段用于账密清单、邮箱编号、换 IP 与 /api/register 提交。
type nativePanelFileConfig struct {
	Gateway struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"gateway"`
	Register struct {
		SiteURL        string `json:"site_url"`
		TurnstileSite  string `json:"turnstile_sitekey"`
		EmailDomain    string `json:"email_domain"`
		EmailPrefix    string `json:"email_prefix"`
		Password       string `json:"password"`
		PlanID         string `json:"plan_id"`
		DomainID       string `json:"domain_id"`
		EmailStartNum  int    `json:"email_start_num"`
		DisplayBase    int    `json:"display_base"`
		CredentialFile string `json:"cred_file"`
		PhoneSOCKS     string `json:"phone_socks"`
		ClashAPI       string `json:"clash_api"`
		ClashSecret    string `json:"clash_secret"`
		ClashGroup     string `json:"clash_group"`
		ClashProxy     string `json:"clash_proxy"`
		ClashNodes     []struct {
			Name     string `json:"name"`
			ExpectIP string `json:"expect_ip"`
		} `json:"clash_nodes"`
	} `json:"register"`
}

func (p nativePanelPaths) loadConfig() (nativePanelFileConfig, error) {
	f, err := os.Open(p.configPath)
	if err != nil {
		return nativePanelFileConfig{}, fmt.Errorf("%w: cannot read panel configuration", errNativePanelUnavailable)
	}
	defer f.Close()
	var cfg nativePanelFileConfig
	if err := json.NewDecoder(io.LimitReader(f, nativePanelMaxConfig)).Decode(&cfg); err != nil {
		return nativePanelFileConfig{}, fmt.Errorf("%w: panel configuration is invalid", errNativePanelUnavailable)
	}
	return cfg, nil
}

func nativePanelExpandHome(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if raw == "~" {
				return home
			}
			return filepath.Join(home, raw[2:])
		}
	}
	return raw
}

func (p nativePanelPaths) credentialPath(cfg nativePanelFileConfig) (string, error) {
	raw := nativePanelExpandHome(cfg.Register.CredentialFile)
	if raw == "" {
		raw = filepath.Join("data", "credentials.txt")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(p.root, raw)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("%w: credential path is invalid", errNativePanelUnavailable)
	}
	return filepath.Clean(abs), nil
}

func nativePanelReadCredentials(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > nativePanelMaxCredential {
		return nil, errors.New("credential file is unavailable or too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	defer f.Close()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), nativePanelMaxLine)
	for scanner.Scan() {
		email, password, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "----")
		if ok && strings.TrimSpace(email) != "" {
			out[strings.TrimSpace(email)] = strings.TrimSpace(password)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	return out, nil
}

// nativePanelManager 现在只是数据目录的读取入口。没有子进程，也就没有任务
// 状态、日志环形缓冲和并发控制。
type nativePanelManager struct {
	config nativePanelConfig
}

func newNativePanelManager(config nativePanelConfig) *nativePanelManager {
	return &nativePanelManager{config: config}
}

// panelData 解析数据目录并读出 config.json。名字不再叫 workerConfig：这里已经
// 没有工作者，只有本机数据。
func (m *nativePanelManager) panelData() (nativePanelPaths, nativePanelFileConfig, error) {
	paths, err := m.config.paths()
	if err != nil {
		return nativePanelPaths{}, nativePanelFileConfig{}, err
	}
	cfg, err := paths.loadConfig()
	if err != nil {
		return nativePanelPaths{}, nativePanelFileConfig{}, err
	}
	return paths, cfg, nil
}

type nativePanelOAuthRequest struct {
	Email string `json:"email"`
}

func nativePanelValidEmail(email string) bool {
	if len(email) < 3 || len(email) > 320 || strings.ContainsAny(email, " \t\r\n\"'") {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	return at > 0 && at < len(email)-1 && strings.Count(email, "@") == 1
}

func (m *nativePanelManager) state(server *Server) map[string]any {
	// register_supported / batch_oauth_supported 告诉前端：Go 内置实现可用。
	// 注册仍需要调用方提供 Turnstile token；批量 OAuth 走 ROPC / 设备码 / PKCE。
	state := map[string]any{
		"native_panel":             true,
		"native_panel_ready":       false,
		"cred_total":               0,
		"register_supported":       true,
		"batch_oauth_supported":    true,
		"register_needs_turnstile": true,
		"oauth_modes":              []string{"ropc", "device_code", "pkce"},
	}
	if server != nil && server.tokens != nil {
		accounts := server.tokens.List()
		emails := make([]string, 0, len(accounts))
		for _, account := range accounts {
			if email := strings.TrimSpace(account.Email); email != "" {
				emails = append(emails, email)
			}
		}
		sort.Strings(emails)
		state["gw_online"], state["gw_emails"] = len(accounts), emails
	}
	paths, cfg, err := m.panelData()
	if err != nil {
		state["native_panel_error"] = err.Error()
		return state
	}
	state["native_panel_ready"] = true
	state["register_ready"] = cfg.registerReady()
	credentialPath, err := paths.credentialPath(cfg)
	if err == nil {
		state["cred_file"] = credentialPath
		if credentials, readErr := nativePanelReadCredentials(credentialPath); readErr == nil {
			state["cred_total"] = len(credentials)
			// 号段边界让界面能显示账密清单实际覆盖的编号范围。
			if low, high, ok := credentialEmailNumBounds(credentials, strings.TrimSpace(cfg.Register.EmailPrefix)); ok {
				state["cred_num_min"], state["cred_num_max"] = low, high
			}
		}
	}
	host, port := strings.TrimSpace(cfg.Gateway.Host), cfg.Gateway.Port
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 || port > 65535 {
		port = 4141
	}
	state["gw_url"] = "http://" + host + ":" + strconv.Itoa(port)
	// 邮箱构成规则：<prefix><num>@<domain>，供界面把编号显示成真实邮箱。
	state["email_prefix"] = strings.TrimSpace(cfg.Register.EmailPrefix)
	state["email_domain"] = strings.TrimSpace(cfg.Register.EmailDomain)
	state["email_start_num"] = cfg.Register.EmailStartNum
	return state
}

func nativePanelOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" { // non-browser admin clients still need the admin session
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}

func nativePanelDecodeJSON(w http.ResponseWriter, r *http.Request, target any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, nativePanelMaxRequest)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(target)
	if errors.Is(err, io.EOF) && allowEmpty {
		return true
	}
	if err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid panel request")
		return false
	}
	return true
}

// nativePanelRemovedRoutes 保留旧路由但明确答复 501。逐条给出原因和可行替代，
// 避免用户在界面上按下按钮后只看到一句无法定位的失败。
var nativePanelRemovedRoutes = map[string]string{
	"/api/admin/panel/job/poll": "任务日志已移除：网关不再拉起本地 Python 工作者进程。注册与批量 OAuth 改为同步返回结果。",
	"/api/admin/panel/job/stop": "任务停止已移除：网关不再拉起本地 Python 工作者进程。",
}

type nativePanelController struct {
	server  *Server
	manager *nativePanelManager
}

func newNativePanelController(server *Server, manager *nativePanelManager) *nativePanelController {
	if manager == nil {
		manager = newNativePanelManager(defaultNativePanelConfig())
	}
	return &nativePanelController{server: server, manager: manager}
}

func (c *nativePanelController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.server == nil || c.manager == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "panel_unavailable", "native panel is unavailable")
		return
	}
	if !c.server.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	if !nativePanelOriginAllowed(r) {
		writeOpenAIError(w, http.StatusForbidden, "csrf_error", "cross-site panel request denied")
		return
	}
	if reason, removed := nativePanelRemovedRoutes[r.URL.Path]; removed {
		writeOpenAIError(w, http.StatusNotImplemented, "feature_removed", reason)
		return
	}

	switch r.URL.Path {
	case "/api/admin/panel/state":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		jsonOut(w, c.manager.state(c.server))
	case "/api/admin/panel/register":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body panelRegisterRequest
		if !nativePanelDecodeJSON(w, r, &body, false) {
			return
		}
		report, err := c.server.runRegister(r.Context(), c.manager, body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, report)
	case "/api/admin/panel/oauth/batch":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body panelOAuthBatchRequest
		if !nativePanelDecodeJSON(w, r, &body, true) {
			return
		}
		report, err := c.server.runOAuthBatch(r.Context(), c.manager, body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, report)
	case "/api/admin/panel/oauth":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body nativePanelOAuthRequest
		if !nativePanelDecodeJSON(w, r, &body, false) {
			return
		}
		email := strings.TrimSpace(body.Email)
		if !nativePanelValidEmail(email) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "email 格式无效")
			return
		}
		state, authorizationURL, attempt, redirectURI, err := c.server.beginPKCEAuthorization("login")
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "pkce_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{
			"ok":               true,
			"status":           "manual_step_required",
			"mode":             "pkce",
			"complete":         false,
			"email":            email,
			"state":            state,
			"attempt":          attempt,
			"authorizationUrl": authorizationURL,
			"redirectUri":      redirectURI,
			"logoutUrl":        auth.LogoutURL(),
			"nextStep":         "请在打开的 Microsoft 页面完成登录,回调完成后账号会自动加入网关。",
		})
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown native panel route")
	}
}

var nativePanelManagers sync.Map // map[*Server]*nativePanelManager

func nativePanelManagerFor(server *Server) *nativePanelManager {
	if current, ok := nativePanelManagers.Load(server); ok {
		return current.(*nativePanelManager)
	}
	created := newNativePanelManager(persistedNativePanelConfig(server))
	actual, _ := nativePanelManagers.LoadOrStore(server, created)
	return actual.(*nativePanelManager)
}

// NativePanelHandler is the direct 4141 replacement for panelProxy. No 8555
// listener and no HTTP proxy are involved.
func (s *Server) NativePanelHandler(w http.ResponseWriter, r *http.Request) {
	newNativePanelController(s, nativePanelManagerFor(s)).ServeHTTP(w, r)
}

// RegisterNativePanelRoutes is the single server.go integration point.
func (s *Server) RegisterNativePanelRoutes(mux *http.ServeMux) {
	paths := []string{
		"/api/admin/panel/state",
		"/api/admin/panel/oauth",
		"/api/admin/panel/oauth/batch",
		"/api/admin/panel/register",
	}
	for path := range nativePanelRemovedRoutes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		mux.HandleFunc(path, s.NativePanelHandler)
	}
}
