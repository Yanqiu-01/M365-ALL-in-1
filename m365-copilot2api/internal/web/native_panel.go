package web

// Native panel support keeps registration and OAuth orchestration inside the
// 4141 gateway. It does not start FastAPI/uvicorn or make loopback HTTP calls:
// existing Python programs remain workers, while Go owns their lifecycle.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nativePanelRootEnv       = "M365_NATIVE_PANEL_ROOT"
	nativePanelPythonEnv     = "M365_NATIVE_PANEL_PYTHON"
	nativePanelLogCap        = 800
	nativePanelPollLogCap    = 200
	nativePanelMaxRequest    = 32 << 10
	nativePanelMaxConfig     = 1 << 20
	nativePanelMaxCredential = 4 << 20
	nativePanelMaxLine       = 256 << 10
	nativePanelMaxLogLine    = 8 << 10
)

var (
	errNativePanelBusy        = errors.New("已有注册或 OAuth 任务正在运行")
	errNativePanelUnavailable = errors.New("本地 Python 工作者未配置或不可用")
)

// nativePanelConfig is process-local. Requests cannot choose a Python binary,
// script, config path, or working directory.
type nativePanelConfig struct {
	Root       string
	Python     string
	LogCap     int
	PollLogCap int
}

func defaultNativePanelConfig() nativePanelConfig {
	root := strings.TrimSpace(os.Getenv(nativePanelRootEnv))
	if root == "" {
		// Release layout: place the existing worker tree beside the gateway exe.
		if exe, err := os.Executable(); err == nil {
			root = filepath.Join(filepath.Dir(exe), "M365-自用")
		} else {
			root = "M365-自用"
		}
	}
	python := strings.TrimSpace(os.Getenv(nativePanelPythonEnv))
	if python == "" {
		python = "python"
	}
	return nativePanelConfig{Root: root, Python: python, LogCap: nativePanelLogCap, PollLogCap: nativePanelPollLogCap}
}

// persistedNativePanelConfig resolves the worker location from saved settings so
// a plain restart keeps registration and OAuth working. The environment variables
// still take precedence, which matches how every other setting behaves here.
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
	if strings.TrimSpace(os.Getenv(nativePanelPythonEnv)) == "" {
		if python := strings.TrimSpace(saved.NativePanelPython); python != "" {
			config.Python = python
		}
	}
	return config
}

func (c nativePanelConfig) normalized() nativePanelConfig {
	if c.LogCap <= 0 {
		c.LogCap = nativePanelLogCap
	}
	if c.PollLogCap <= 0 || c.PollLogCap > c.LogCap {
		c.PollLogCap = min(c.LogCap, nativePanelPollLogCap)
	}
	if strings.TrimSpace(c.Python) == "" {
		c.Python = "python"
	}
	return c
}

type nativePanelPaths struct {
	root       string
	configPath string
}

func (c nativePanelConfig) paths() (nativePanelPaths, error) {
	root := strings.TrimSpace(c.Root)
	if root == "" {
		return nativePanelPaths{}, fmt.Errorf("%w: set %s or package M365-自用 beside the gateway executable", errNativePanelUnavailable, nativePanelRootEnv)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nativePanelPaths{}, fmt.Errorf("%w: resolve worker directory", errNativePanelUnavailable)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nativePanelPaths{}, fmt.Errorf("%w: worker directory is unavailable", errNativePanelUnavailable)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nativePanelPaths{}, fmt.Errorf("%w: worker directory is unavailable", errNativePanelUnavailable)
	}
	configPath := filepath.Join(resolved, "config.json")
	configInfo, err := os.Stat(configPath)
	if err != nil || !configInfo.Mode().IsRegular() {
		return nativePanelPaths{}, fmt.Errorf("%w: config.json is unavailable", errNativePanelUnavailable)
	}
	return nativePanelPaths{root: resolved, configPath: configPath}, nil
}

func nativePanelPathInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (p nativePanelPaths) script(relative string) (string, error) {
	candidate := filepath.Clean(filepath.Join(p.root, filepath.FromSlash(relative)))
	if !nativePanelPathInside(p.root, candidate) {
		return "", fmt.Errorf("%w: invalid worker script", errNativePanelUnavailable)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || !nativePanelPathInside(p.root, resolved) {
		return "", fmt.Errorf("%w: worker script is unavailable", errNativePanelUnavailable)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: worker script is unavailable", errNativePanelUnavailable)
	}
	return resolved, nil
}

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
		EmailStartNum  int    `json:"email_start_num"`
		CredentialFile string `json:"cred_file"`
	} `json:"register"`
}

func (p nativePanelPaths) loadConfig() (nativePanelFileConfig, error) {
	f, err := os.Open(p.configPath)
	if err != nil {
		return nativePanelFileConfig{}, fmt.Errorf("%w: cannot read worker configuration", errNativePanelUnavailable)
	}
	defer f.Close()
	var cfg nativePanelFileConfig
	if err := json.NewDecoder(io.LimitReader(f, nativePanelMaxConfig)).Decode(&cfg); err != nil {
		return nativePanelFileConfig{}, fmt.Errorf("%w: worker configuration is invalid", errNativePanelUnavailable)
	}
	return cfg, nil
}

func (cfg nativePanelFileConfig) registerReady() bool {
	for _, value := range []string{cfg.Register.SiteURL, cfg.Register.TurnstileSite, cfg.Register.EmailDomain, cfg.Register.EmailPrefix, cfg.Register.Password} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
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

// The runner is structured rather than shell-based so that request data cannot
// become an executable command line or an arbitrary working directory.
type nativePanelCommand struct {
	Program string
	Args    []string
	Dir     string
	Env     []string
}

type nativePanelProcess interface {
	PID() int
	Wait() (int, error)
	KillTree() error
}

type nativePanelRunner interface {
	Start(context.Context, nativePanelCommand) (nativePanelProcess, io.ReadCloser, error)
}

type nativePanelExecRunner struct{}

type nativePanelExecProcess struct{ cmd *exec.Cmd }

func (nativePanelExecRunner) Start(ctx context.Context, spec nativePanelCommand) (nativePanelProcess, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
	cmd.Dir, cmd.Env, cmd.Stdin = spec.Dir, spec.Env, nil
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	// StdoutPipe sets cmd.Stdout to the pipe writer; sharing that writer keeps
	// Python tracebacks in the same bounded log stream.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		return nil, nil, err
	}
	return nativePanelExecProcess{cmd: cmd}, out, nil
}

func (p nativePanelExecProcess) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p nativePanelExecProcess) Wait() (int, error) {
	if p.cmd == nil {
		return -1, errors.New("worker process is unavailable")
	}
	err := p.cmd.Wait()
	if p.cmd.ProcessState == nil {
		return -1, err
	}
	return p.cmd.ProcessState.ExitCode(), err
}

func (p nativePanelExecProcess) KillTree() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" && p.cmd.Process.Pid > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(p.cmd.Process.Pid), "/T", "/F")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

type nativePanelJob struct {
	id       uint64
	running  bool
	stopping bool
	kind     string
	started  time.Time
	exit     *int
	process  nativePanelProcess
	cancel   context.CancelFunc
	logs     []string
	cleanup  func()
}

// Lines exists for the current 4141 page; Log preserves the older panel API.
type nativePanelJobSnapshot struct {
	Running bool     `json:"running"`
	Kind    string   `json:"kind"`
	Started int64    `json:"started"`
	Exit    *int     `json:"exit"`
	Log     []string `json:"log"`
	Lines   []string `json:"lines"`
}

type nativePanelManager struct {
	mu     sync.Mutex
	config nativePanelConfig
	runner nativePanelRunner
	now    func() time.Time
	nextID uint64
	job    nativePanelJob
}

func newNativePanelManager(config nativePanelConfig, runner nativePanelRunner) *nativePanelManager {
	config = config.normalized()
	if runner == nil {
		runner = nativePanelExecRunner{}
	}
	return &nativePanelManager{config: config, runner: runner, now: time.Now}
}

func (m *nativePanelManager) appendLogLocked(text string) {
	text = strings.TrimRight(strings.TrimSpace(text), "\r\n")
	if text == "" {
		return
	}
	if len(text) > nativePanelMaxLogLine {
		text = text[:nativePanelMaxLogLine] + " …[truncated]"
	}
	if len(m.job.logs) >= m.config.LogCap {
		copy(m.job.logs, m.job.logs[1:])
		m.job.logs = m.job.logs[:len(m.job.logs)-1]
	}
	m.job.logs = append(m.job.logs, text)
}

func (m *nativePanelManager) snapshotLocked() nativePanelJobSnapshot {
	start := 0
	if len(m.job.logs) > m.config.PollLogCap {
		start = len(m.job.logs) - m.config.PollLogCap
	}
	logs := append([]string(nil), m.job.logs[start:]...)
	return nativePanelJobSnapshot{Running: m.job.running, Kind: m.job.kind, Started: m.job.started.Unix(), Exit: m.job.exit, Log: logs, Lines: append([]string(nil), logs...)}
}

func (m *nativePanelManager) snapshot() nativePanelJobSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *nativePanelManager) finish(id uint64, process nativePanelProcess, out io.ReadCloser) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), nativePanelMaxLine)
	for scanner.Scan() {
		m.mu.Lock()
		if m.job.id == id {
			m.appendLogLocked(scanner.Text())
		}
		m.mu.Unlock()
	}
	_ = out.Close()
	exit, waitErr := process.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job.id != id {
		return
	}
	if m.job.cleanup != nil {
		m.job.cleanup()
		m.job.cleanup = nil
	}
	m.job.running, m.job.stopping, m.job.process, m.job.cancel = false, false, nil, nil
	m.job.exit = &exit
	if waitErr != nil {
		m.appendLogLocked(fmt.Sprintf("任务结束 exit=%d: %s", exit, waitErr.Error()))
	} else {
		m.appendLogLocked(fmt.Sprintf("任务结束 exit=%d", exit))
	}
}

func (m *nativePanelManager) start(kind, script string, args []string, prepare func() (func(), error)) (nativePanelJobSnapshot, error) {
	paths, err := m.config.paths()
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	worker, err := paths.script(script)
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.job.running {
		m.mu.Unlock()
		cancel()
		return nativePanelJobSnapshot{}, errNativePanelBusy
	}
	m.nextID++
	id := m.nextID
	m.job = nativePanelJob{id: id, running: true, kind: kind, started: m.now(), cancel: cancel, logs: make([]string, 0, min(16, m.config.LogCap))}
	if prepare != nil {
		cleanup, prepErr := prepare()
		if prepErr != nil {
			exit := -1
			m.job.running, m.job.exit, m.job.cancel = false, &exit, nil
			m.mu.Unlock()
			cancel()
			return nativePanelJobSnapshot{}, prepErr
		}
		m.job.cleanup = cleanup
	}
	m.appendLogLocked("启动 " + kind)
	m.mu.Unlock()

	env := append([]string(nil), os.Environ()...)
	env = append(env, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8", "M365_CONFIG="+paths.configPath)
	process, out, err := m.runner.Start(ctx, nativePanelCommand{Program: m.config.Python, Args: append([]string{worker}, args...), Dir: paths.root, Env: env})
	if err != nil {
		cancel()
		m.mu.Lock()
		if m.job.id == id {
			if m.job.cleanup != nil {
				m.job.cleanup()
			}
			exit := -1
			m.job.running, m.job.exit, m.job.cleanup, m.job.cancel = false, &exit, nil, nil
			m.appendLogLocked("启动失败")
		}
		m.mu.Unlock()
		return nativePanelJobSnapshot{}, fmt.Errorf("启动本地 Python 工作者失败: %w", err)
	}
	m.mu.Lock()
	stopped := m.job.id != id || m.job.stopping || !m.job.running
	if !stopped {
		m.job.process = process
	}
	m.mu.Unlock()
	if stopped {
		cancel()
		_ = process.KillTree()
		// finish owns cleanup and the transition to idle even when stop won the
		// race with process startup.
		go m.finish(id, process, out)
		return nativePanelJobSnapshot{}, context.Canceled
	}
	go m.finish(id, process, out)
	return m.snapshot(), nil
}

func (m *nativePanelManager) stop() nativePanelJobSnapshot {
	m.mu.Lock()
	if !m.job.running {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot
	}
	m.job.stopping = true
	cancel, process := m.job.cancel, m.job.process
	m.appendLogLocked("已请求停止任务")
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if process != nil {
		_ = process.KillTree()
	}
	return snapshot
}

func (m *nativePanelManager) workerConfig() (nativePanelPaths, nativePanelFileConfig, error) {
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

func nativePanelPositive(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func nativePanelClamp(value, lower, upper int) int {
	if value < lower {
		return lower
	}
	if value > upper {
		return upper
	}
	return value
}

type nativePanelRegisterRequest struct {
	Mode        string `json:"mode"`
	Count       int    `json:"count"`
	Concurrent  bool   `json:"concurrent"`
	Concurrency int    `json:"concurrency"`
	Timeout     int    `json:"timeout"`
	Start       int    `json:"start"`
	Resume      bool   `json:"resume"`
	Reset       bool   `json:"reset"`
}

func (m *nativePanelManager) startRegister(request nativePanelRegisterRequest) (nativePanelJobSnapshot, error) {
	_, cfg, err := m.workerConfig()
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	if !cfg.registerReady() {
		return nativePanelJobSnapshot{}, errors.New("注册配置不完整，未启动任务")
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "phone"
	}
	count := nativePanelPositive(request.Count, 1)
	start := nativePanelPositive(request.Start, 1)
	if count < 1 || count > 1000 || start < 1 || start > 1_000_000 {
		return nativePanelJobSnapshot{}, errors.New("注册数量或起始序号无效")
	}
	if request.Concurrent && mode == "proxy" {
		concurrency := nativePanelClamp(nativePanelPositive(request.Concurrency, 3), 1, 16)
		timeout := nativePanelClamp(nativePanelPositive(request.Timeout, 180), 30, 600)
		args := []string{"--concurrency", strconv.Itoa(concurrency), "--limit", strconv.Itoa(count), "--timeout", strconv.Itoa(timeout)}
		if request.Resume {
			args = append(args, "--resume")
		}
		if request.Reset {
			args = append(args, "--reset")
		}
		if start > 1 {
			args = append(args, "--start", strconv.Itoa(start))
		}
		return m.start(fmt.Sprintf("register:proxy×%d", concurrency), "Register/register_accounts_concurrent.py", args, nil)
	}

	var script string
	switch mode {
	case "phone":
		script = "Register/register_m365_phone.py"
	case "clash":
		script = "Register/register_m365_clash.py"
	case "proxy":
		script = "Register/register_accounts.py"
	default:
		return nativePanelJobSnapshot{}, errors.New("未知注册模式")
	}
	args := []string{"--limit", strconv.Itoa(count)}
	if start > 1 {
		if mode == "phone" || mode == "clash" {
			base := cfg.Register.EmailStartNum
			if base <= 0 {
				base = 1000
			}
			args = append(args, "--start", strconv.Itoa(base+start-1))
		} else {
			args = append(args, "--start", strconv.Itoa(start))
		}
	}
	return m.start("register:"+mode, script, args, nil)
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

func (m *nativePanelManager) startOAuth(request nativePanelOAuthRequest) (nativePanelJobSnapshot, error) {
	email := strings.TrimSpace(request.Email)
	if !nativePanelValidEmail(email) {
		return nativePanelJobSnapshot{}, errors.New("email 格式无效")
	}
	paths, cfg, err := m.workerConfig()
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	credentialPath, err := paths.credentialPath(cfg)
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	credentials, err := nativePanelReadCredentials(credentialPath)
	if err != nil {
		return nativePanelJobSnapshot{}, err
	}
	password := credentials[email]
	if strings.TrimSpace(password) == "" {
		return nativePanelJobSnapshot{}, errors.New("账密文件中找不到该邮箱")
	}
	probe := filepath.Join(paths.root, "oauth", "_pw_probe.txt")
	prepare := func() (func(), error) {
		f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		_, writeErr := f.WriteString(password)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(probe)
			if writeErr != nil {
				return nil, writeErr
			}
			return nil, closeErr
		}
		return func() { _ = os.Remove(probe) }, nil
	}
	return m.start("oauth:"+email, "oauth/oauth_auto.py", []string{email}, prepare)
}

type nativePanelOAuthBatchRequest struct {
	Concurrent  *bool `json:"concurrent"`
	Serial      *bool `json:"serial"`
	Concurrency int   `json:"concurrency"`
	Timeout     int   `json:"timeout"`
	Start       int   `json:"start"`
	Limit       int   `json:"limit"`
	Resume      bool  `json:"resume"`
	Reset       bool  `json:"reset"`
}

func (r nativePanelOAuthBatchRequest) serial() bool {
	if r.Serial != nil {
		return *r.Serial
	}
	return r.Concurrent != nil && !*r.Concurrent
}

func (m *nativePanelManager) startOAuthBatch(request nativePanelOAuthBatchRequest) (nativePanelJobSnapshot, error) {
	if _, _, err := m.workerConfig(); err != nil {
		return nativePanelJobSnapshot{}, err
	}
	start := nativePanelPositive(request.Start, 1)
	if start < 1 || start > 1_000_000 || request.Limit < 0 || request.Limit > 1000 {
		return nativePanelJobSnapshot{}, errors.New("批量 OAuth 参数无效")
	}
	if request.serial() {
		args := []string{}
		if start > 1 {
			args = append(args, "--start", strconv.Itoa(start))
		}
		if request.Limit > 0 {
			args = append(args, "--limit", strconv.Itoa(request.Limit))
		}
		return m.start("oauth:batch", "oauth/oauth_batch.py", args, nil)
	}
	concurrency := nativePanelClamp(nativePanelPositive(request.Concurrency, 3), 1, 16)
	timeout := nativePanelClamp(nativePanelPositive(request.Timeout, 300), 60, 900)
	args := []string{"--concurrency", strconv.Itoa(concurrency), "--timeout", strconv.Itoa(timeout)}
	if request.Resume {
		args = append(args, "--resume")
	}
	if request.Reset {
		args = append(args, "--reset")
	}
	if start > 1 {
		args = append(args, "--start", strconv.Itoa(start))
	}
	if request.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(request.Limit))
	}
	return m.start(fmt.Sprintf("oauth:batch×%d", concurrency), "oauth/oauth_batch_concurrent.py", args, nil)
}

func (m *nativePanelManager) state(server *Server) map[string]any {
	state := map[string]any{"native_panel": true, "native_panel_ready": false, "cred_total": 0, "register_ready": false, "job": m.snapshot()}
	if server != nil && server.tokens != nil {
		accounts := server.tokens.List()
		emails := make([]string, 0, len(accounts))
		for _, account := range accounts {
			if email := strings.TrimSpace(account.Email); email != "" {
				emails = append(emails, email)
			}
		}
		sort.Strings(emails)
		if len(emails) > 200 {
			emails = emails[:200]
		}
		state["gw_online"], state["gw_emails"] = len(accounts), emails
	}
	paths, cfg, err := m.workerConfig()
	if err != nil {
		state["native_panel_error"] = err.Error()
		return state
	}
	state["native_panel_ready"], state["register_ready"] = true, cfg.registerReady()
	credentialPath, err := paths.credentialPath(cfg)
	if err == nil {
		state["cred_file"] = credentialPath
		if credentials, readErr := nativePanelReadCredentials(credentialPath); readErr == nil {
			state["cred_total"] = len(credentials)
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

type nativePanelController struct {
	server  *Server
	manager *nativePanelManager
}

func newNativePanelController(server *Server, manager *nativePanelManager) *nativePanelController {
	if manager == nil {
		manager = newNativePanelManager(defaultNativePanelConfig(), nil)
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

	switch r.URL.Path {
	case "/api/admin/panel/state":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		jsonOut(w, c.manager.state(c.server))
	case "/api/admin/panel/job/poll":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		jsonOut(w, c.manager.snapshot())
	case "/api/admin/panel/job/stop":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		jsonOut(w, map[string]any{"ok": true, "job": c.manager.stop()})
	case "/api/admin/panel/register":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body nativePanelRegisterRequest
		if nativePanelDecodeJSON(w, r, &body, false) {
			snapshot, err := c.manager.startRegister(body)
			c.writeStart(w, snapshot, err)
		}
	case "/api/admin/panel/oauth":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body nativePanelOAuthRequest
		if nativePanelDecodeJSON(w, r, &body, false) {
			snapshot, err := c.manager.startOAuth(body)
			c.writeStart(w, snapshot, err)
		}
	case "/api/admin/panel/oauth/batch":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var body nativePanelOAuthBatchRequest
		if nativePanelDecodeJSON(w, r, &body, true) {
			snapshot, err := c.manager.startOAuthBatch(body)
			c.writeStart(w, snapshot, err)
		}
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown native panel route")
	}
}

func (c *nativePanelController) writeStart(w http.ResponseWriter, snapshot nativePanelJobSnapshot, err error) {
	if err == nil {
		jsonOut(w, map[string]any{"ok": true, "kind": snapshot.Kind, "job": snapshot})
		return
	}
	if errors.Is(err, errNativePanelBusy) {
		writeOpenAIError(w, http.StatusConflict, "job_conflict", err.Error())
		return
	}
	if errors.Is(err, errNativePanelUnavailable) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "panel_unavailable", err.Error())
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
}

var nativePanelManagers sync.Map // map[*Server]*nativePanelManager

func nativePanelManagerFor(server *Server) *nativePanelManager {
	if current, ok := nativePanelManagers.Load(server); ok {
		return current.(*nativePanelManager)
	}
	created := newNativePanelManager(persistedNativePanelConfig(server), nil)
	actual, _ := nativePanelManagers.LoadOrStore(server, created)
	return actual.(*nativePanelManager)
}

// NativePanelHandler is the direct 4141 replacement for panelProxy. No 8555
// listener and no HTTP proxy are involved.
func (s *Server) NativePanelHandler(w http.ResponseWriter, r *http.Request) {
	newNativePanelController(s, nativePanelManagerFor(s)).ServeHTTP(w, r)
}

// RegisterNativePanelRoutes is the single server.go integration point. Replace
// the six panelProxy registrations with this one call on the fresh ServeMux.
func (s *Server) RegisterNativePanelRoutes(mux *http.ServeMux) {
	for _, path := range []string{
		"/api/admin/panel/register",
		"/api/admin/panel/oauth",
		"/api/admin/panel/oauth/batch",
		"/api/admin/panel/state",
		"/api/admin/panel/job/stop",
		"/api/admin/panel/job/poll",
	} {
		mux.HandleFunc(path, s.NativePanelHandler)
	}
}
