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
//   - 注册走 Go：换 IP（Rust CLI 优先，Go 回退）+ FlareSolverr 取 Turnstile
//     token + POST /api/register。调用方仍可手动提供 token 作为回退。
//   - job/poll 仍返回 501。job/stop 会取消当前内嵌注册。

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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/turnstile"
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
	if root == "" && runtime.GOOS == "android" {
		// Android GatewayService 把 M365_DATA_DIR 指到 files/gw/data，那才是可写目录。
		// 可执行文件在 nativeLibraryDir，旁边根本没有 M365-自用。
		//
		// 这个回退只对 Android 成立。M365_DATA_DIR 在本模块另有 8 处读者（账号
		// 缓存、凭据、管理口令、部署、诊断、设置、用量、turnstile），PC 上的操作
		// 员为那些用途设了它，就会连带把面板根目录搬走：paths() 会在新位置建目录
		// 并写一份默认 config.json，state() 随即报 native_panel_ready=true 且
		// cred_total=0 —— 真正的 credentials.txt 变成看不见，注册又从 1000 号重新
		// 开始。而且没有任何界面能把它改回来。
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
	body, _ := json.MarshalIndent(defaultNativePanelFileConfig(), "", "  ")
	body = append(body, '\n')
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
		SiteURL string `json:"site_url"`
		// TurnstileSite 只解码保留，本模块没有任何读者：站点 key 由注册页自己
		// 提供，registerReady() 也不再要求它。保留字段是为了让操作员配置里已有
		// 的这个键在 saveRegisterConfig 整体覆盖时不被丢掉，而不是它还有用。
		TurnstileSite   string `json:"turnstile_sitekey"`
		FlareSolverrURL string `json:"flaresolverr_url"`
		// Solver 选谁来解 Turnstile：auto（默认）、chrome、flaresolverr。
		//
		// 默认 auto 会在本机装了浏览器时用浏览器 —— 这不是偏好，是能力差别：
		// FlareSolverr 只能回一张页面快照，而 token 写在隐藏 input 的 value
		// property 上，快照里没有它。留下这个键是为了让「我就要用 FlareSolverr」
		// 这种意图能表达出来，而不是被代码悄悄改掉。
		Solver         string            `json:"solver"`
		EmailDomain    string            `json:"email_domain"`
		EmailPrefix    string            `json:"email_prefix"`
		Password       string            `json:"password"`
		PlanID         string            `json:"plan_id"`
		DomainID       string            `json:"domain_id"`
		EmailStartNum  int               `json:"email_start_num"`
		DisplayBase    int               `json:"display_base"`
		CredentialFile string            `json:"cred_file"`
		PhoneSOCKS     string            `json:"phone_socks"`
		// ADB 是 adb 可执行文件的路径。phone 模式靠它切飞行模式换运营商 IP，而
		// exitrotate 在找不到配置时只会执行 PATH 上的 "adb" —— Windows 上 adb 通常
		// 装在 WinGet 的包目录里并不在 PATH，于是每次换 IP 都以「adb 找不到」失败，
		// 整批注册在第二个号就断掉。配置里必须能钉住绝对路径。
		ADB            string            `json:"adb"`
		ClashAPI       string            `json:"clash_api"`
		ClashSecret    string            `json:"clash_secret"`
		ClashGroup     string            `json:"clash_group"`
		ClashProxy     string            `json:"clash_proxy"`
		ClashNodes     []clashNodeConfig `json:"clash_nodes"`
	} `json:"register"`
}

// clashNodeConfig 是配置文件里的一个 Clash 节点。原先它是匿名结构体，
// 于是 saveRegisterConfig 无法为它构造值，节点表只能靠手工改 config.json ——
// 这就是 clash 模式在界面上无法配置的直接原因。
type clashNodeConfig struct {
	Name     string `json:"name"`
	ExpectIP string `json:"expect_ip"`
}

func defaultNativePanelFileConfig() nativePanelFileConfig {
	var cfg nativePanelFileConfig
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = 4141
	cfg.Register.SiteURL = "https://office.965007.xyz"
	cfg.Register.FlareSolverrURL = turnstile.DefaultEndpoint
	cfg.Register.EmailDomain = "office.bo.edu.kg"
	cfg.Register.EmailPrefix = "24s05"
	// Never ship a credential in source. Seed from the environment when it is
	// available, otherwise leave it blank and let the operator set it in the
	// panel (POST /api/admin/panel/config) on first run.
	cfg.Register.Password = strings.TrimSpace(os.Getenv("M365_REGISTER_PASSWORD"))
	cfg.Register.PlanID = "1"
	cfg.Register.DomainID = "1"
	cfg.Register.EmailStartNum = 1000
	cfg.Register.DisplayBase = 1
	cfg.Register.CredentialFile = "credentials.txt"
	return cfg
}

func (p nativePanelPaths) saveConfig(cfg nativePanelFileConfig) error {
	if strings.TrimSpace(cfg.Register.CredentialFile) == "" {
		cfg.Register.CredentialFile = "credentials.txt"
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode panel configuration: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(p.configPath, body, 0o600); err != nil {
		return fmt.Errorf("write panel configuration: %w", err)
	}
	return nil
}

// nativePanelClashNodeRequest 是一个 Clash 节点。ExpectIP 可空：填了就由
// exitrotate 在实际出口 IP 与预期不符时给出告警。
type nativePanelClashNodeRequest struct {
	Name     string `json:"name"`
	ExpectIP string `json:"expectIp"`
}

type nativePanelRegisterConfigRequest struct {
	SiteURL         string `json:"siteUrl"`
	EmailDomain     string `json:"emailDomain"`
	EmailPrefix     string `json:"emailPrefix"`
	Password        string `json:"password"`
	EmailStartNum   int    `json:"emailStartNum"`
	FlareSolverrURL string `json:"flaresolverrUrl"`
	PhoneSOCKS      string `json:"phoneSocks"`
	ADB             string `json:"adb"`
	ClashAPI        string `json:"clashApi"`
	ClashSecret     string `json:"clashSecret"`
	ClashGroup      string `json:"clashGroup"`
	ClashProxy      string `json:"clashProxy"`
	// ClashNodes 之前根本不存在，而 clash 模式注册离了它就跑不起来：
	// exitrotate.rotateClash 要求 api/group/node 三者齐备，节点名只能来自
	// cfg.Register.ClashNodes（panel_register.go 的 clashNodes()）。配置结构里
	// 有 clash_nodes 这个键、状态接口回报 clash_node_total、界面上也能选「Clash
	// 节点」模式，唯独没有任何入口能把它写进去 —— 于是选了这个模式必然以
	// 「clash api, group and node are required」失败。
	//
	// 指针语义区分「没提这个字段」（保持不变，与其余字段一致）和「显式传了空
	// 数组」（清空节点表）。节点表是个列表，没有「非空即覆盖」可言，所以不能
	// 沿用其它字段的 TrimSpace 判断。
	ClashNodes *[]nativePanelClashNodeRequest `json:"clashNodes,omitempty"`
}

func (m *nativePanelManager) saveRegisterConfig(req nativePanelRegisterConfigRequest) (nativePanelFileConfig, error) {
	paths, cfg, err := m.panelData()
	if err != nil {
		return nativePanelFileConfig{}, err
	}
	if v := strings.TrimSpace(req.SiteURL); v != "" {
		cfg.Register.SiteURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(req.FlareSolverrURL); v != "" {
		cfg.Register.FlareSolverrURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(req.EmailDomain); v != "" {
		cfg.Register.EmailDomain = strings.TrimPrefix(v, "@")
	}
	if v := strings.TrimSpace(req.EmailPrefix); v != "" {
		cfg.Register.EmailPrefix = v
	}
	if v := strings.TrimSpace(req.Password); v != "" {
		cfg.Register.Password = v
	}
	if req.EmailStartNum > 0 {
		cfg.Register.EmailStartNum = req.EmailStartNum
	}
	if v := strings.TrimSpace(req.PhoneSOCKS); v != "" {
		cfg.Register.PhoneSOCKS = v
	}
	if v := strings.TrimSpace(req.ADB); v != "" {
		cfg.Register.ADB = v
	}
	if v := strings.TrimSpace(req.ClashAPI); v != "" {
		cfg.Register.ClashAPI = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(req.ClashSecret); v != "" {
		cfg.Register.ClashSecret = v
	}
	if v := strings.TrimSpace(req.ClashGroup); v != "" {
		cfg.Register.ClashGroup = v
	}
	if v := strings.TrimSpace(req.ClashProxy); v != "" {
		cfg.Register.ClashProxy = v
	}
	if req.ClashNodes != nil {
		// 整表替换而不是追加：界面上编辑的就是一份完整清单，追加语义会让删掉
		// 一个节点变成做不到的事。名字为空的行直接丢掉 —— rotateClash 对空节点
		// 名一律报错，留着它只会让整批注册在某一轮突然失败。
		nodes := make([]clashNodeConfig, 0, len(*req.ClashNodes))
		seen := make(map[string]bool, len(*req.ClashNodes))
		for _, node := range *req.ClashNodes {
			name := strings.TrimSpace(node.Name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			nodes = append(nodes, clashNodeConfig{Name: name, ExpectIP: strings.TrimSpace(node.ExpectIP)})
		}
		cfg.Register.ClashNodes = nodes
	}
	if err := paths.saveConfig(cfg); err != nil {
		return nativePanelFileConfig{}, err
	}
	return cfg, nil
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
	// 默认值只在内存里补齐，不回写。
	//
	// 早先这里是 if applyRegisterDefaults(&cfg) { p.saveConfig(cfg) }：saveConfig
	// 用 MarshalIndent 整体覆盖 config.json，而 nativePanelFileConfig 没有兜住未
	// 知字段的 RawMessage，于是操作员自己加的任何键都会在第一次读取时消失。触发
	// 点还是 GET /api/admin/panel/state —— 一个名义上只读的端点，无备份、无日志。
	// 落盘只应该发生在真正的写入路径（saveRegisterConfig）。
	applyRegisterDefaults(&cfg)
	return cfg, nil
}

func applyRegisterDefaults(cfg *nativePanelFileConfig) bool {
	if cfg == nil {
		return false
	}
	def := defaultNativePanelFileConfig()
	changed := false
	fill := func(dst *string, src string) {
		if strings.TrimSpace(*dst) == "" && src != "" {
			*dst = src
			changed = true
		}
	}
	fill(&cfg.Register.SiteURL, def.Register.SiteURL)
	fill(&cfg.Register.FlareSolverrURL, def.Register.FlareSolverrURL)
	fill(&cfg.Register.EmailDomain, def.Register.EmailDomain)
	fill(&cfg.Register.EmailPrefix, def.Register.EmailPrefix)
	fill(&cfg.Register.Password, def.Register.Password)
	if strings.TrimSpace(cfg.Register.CredentialFile) == "" {
		cfg.Register.CredentialFile = def.Register.CredentialFile
		changed = true
	}
	if cfg.Register.PlanID == "" {
		cfg.Register.PlanID = def.Register.PlanID
		changed = true
	}
	if cfg.Register.DomainID == "" {
		cfg.Register.DomainID = def.Register.DomainID
		changed = true
	}
	if cfg.Gateway.Host == "" {
		cfg.Gateway.Host = def.Gateway.Host
		changed = true
	}
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = def.Gateway.Port
		changed = true
	}
	return changed
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
	// 注册由内置 FlareSolverr 取 token；批量 OAuth 走 ROPC / 设备码 / PKCE。
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
	state["site_url"] = strings.TrimSpace(cfg.Register.SiteURL)
	state["flaresolverr_url"] = strings.TrimSpace(cfg.Register.FlareSolverrURL)
	// 只报告是否已设置，不回显明文。
	//
	// 这里原本是 state["register_password"] = cfg.Register.Password，于是
	// GET /api/admin/panel/state 会把注册账号的密码明文返回。密码框本来就不该预填真
	// 值：前端拿到 register_password_set 就能显示占位符，用户不改就不提交该字段，
	// saveRegisterConfig 里 `if v := TrimSpace(req.Password); v != ""` 的写法已经支持
	// 留空即保持不变。与 clash_secret_set 的处理保持一致。
	state["register_password_set"] = strings.TrimSpace(cfg.Register.Password) != ""
	// phone / clash 五个字段 saveRegisterConfig 收得下也存得住，却一直没在 state
	// 里回传，配置往返是单向的：界面无法显示已存的值，也无法看出缺了什么。而
	// regMode 里明明就有「Clash 节点」这个选项 —— 选了它注册，rotateClash 会以
	// 「clash api, group and node are required」失败，第二个账号起全部报「上一号
	// 已写入本地，但换 IP 失败」，而用户在界面上找不到任何能填这些值的地方。
	state["phone_socks"] = strings.TrimSpace(cfg.Register.PhoneSOCKS)
	state["adb"] = strings.TrimSpace(cfg.Register.ADB)
	state["clash_api"] = strings.TrimSpace(cfg.Register.ClashAPI)
	state["clash_group"] = strings.TrimSpace(cfg.Register.ClashGroup)
	state["clash_proxy"] = strings.TrimSpace(cfg.Register.ClashProxy)
	// 密钥只回传是否已设置，不回传原值。
	state["clash_secret_set"] = strings.TrimSpace(cfg.Register.ClashSecret) != ""
	state["clash_node_total"] = len(cfg.Register.ClashNodes)
	// 节点清单本身也要回传，否则界面只知道「有几个」而无法显示或编辑它们，
	// 配置往返仍然是单向的。节点名与预期 IP 都不是机密，回显是安全的。
	nodes := make([]map[string]string, 0, len(cfg.Register.ClashNodes))
	for _, node := range clashNodes(cfg) {
		nodes = append(nodes, map[string]string{"name": node.Name, "expect_ip": node.ExpectIP})
	}
	state["clash_nodes"] = nodes
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
	case "/api/admin/panel/config":
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			w.Header().Set("Allow", "POST, PUT")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST or PUT required")
			return
		}
		var body nativePanelRegisterConfigRequest
		if !nativePanelDecodeJSON(w, r, &body, false) {
			return
		}
		cfg, err := c.manager.saveRegisterConfig(body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{
			"ok":             true,
			"register_ready": cfg.registerReady(),
			"siteUrl":        cfg.Register.SiteURL,
			"emailDomain":    cfg.Register.EmailDomain,
			"emailPrefix":    cfg.Register.EmailPrefix,
			"emailStartNum":  cfg.Register.EmailStartNum,
		})
	case "/api/admin/panel/job/stop":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		// 如实回报。turnstile.Cancel 在协作目录不存在时（桌面端默认如此）什么也
		// 做不了，早先这里无条件回 stopped:true，等于告诉用户任务停了而它还在跑。
		stopped := turnstile.Cancel()
		out := map[string]any{"ok": true, "stopped": stopped}
		if !stopped {
			out["detail"] = "当前没有可取消的 WebView 求解任务；注册请求需要等本轮结束或断开客户端连接"
		}
		jsonOut(w, out)
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
	case "/api/admin/panel/job/register":
		// 长跑批量注册。单次 /panel/register 上限 20 个号，几千个号只能靠反复调用；
		// 放在网关里跑，进度可查、可停，也不再需要一个外部脚本进程。
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body registerJobRequest
		if !nativePanelDecodeJSON(w, r, &body, false) {
			return
		}
		state, err := c.server.startRegisterJob(c.manager, body)
		if err != nil {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "job": state})
	case "/api/admin/panel/job/register/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		jsonOut(w, map[string]any{"ok": true, "job": c.server.registerJob().snapshot()})
	case "/api/admin/panel/job/register/stop":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		// 如实回报有没有任务被停掉，不假装停了什么。当前批次要等它自己结束 ——
		// 注册中途硬断会留下站点侧已建号、本地没有密码的孤号。
		stopped := c.server.registerJob().stop()
		out := map[string]any{"ok": true, "stopped": stopped, "job": c.server.registerJob().snapshot()}
		if !stopped {
			out["detail"] = "当前没有在跑的批量注册任务"
		} else {
			out["detail"] = "已请求停止，当前这一批会跑完再退出"
		}
		jsonOut(w, out)
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
		// 单账号端点，语义与 /api/auth/start 的默认一致：独占。
		state, authorizationURL, attempt, redirectURI, err := c.server.beginPKCEAuthorization("login", false)
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
		"/api/admin/panel/config",
		"/api/admin/panel/job/stop",
		"/api/admin/panel/job/register",
		"/api/admin/panel/job/register/status",
		"/api/admin/panel/job/register/stop",
	}
	for path := range nativePanelRemovedRoutes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		mux.HandleFunc(path, s.NativePanelHandler)
	}
}
