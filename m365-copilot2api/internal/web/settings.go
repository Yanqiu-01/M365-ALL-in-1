package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"
)

type modelMapping struct {
	PublicModel           string `json:"publicModel"`
	UpstreamTone          string `json:"upstreamTone"`
	DisplayName           string `json:"displayName"`
	DefaultReasoningLevel string `json:"defaultReasoningLevel"`
}

var defaultModelMappings = []modelMapping{
	{PublicModel: "gpt-5.6-sol", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "xhigh"},
	{PublicModel: "gpt-5.6-terra", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Terra", DefaultReasoningLevel: "xhigh"},
	{PublicModel: "gpt-5.6-luna", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Luna", DefaultReasoningLevel: "xhigh"},
}

var publicModelID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

var configurableCodexModels = []string{
	"gpt-5.2",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.5",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"codex-auto-review",
}

type runtimeSettings struct {
	MaxToolCallsPerTurn int            `json:"maxToolCallsPerTurn"`
	MaxToolRounds       int            `json:"maxToolRounds"`
	ContextWindow       int            `json:"contextWindow"`
	MaxOutputTokens     int            `json:"maxOutputTokens"`
	ChatTimeoutSeconds  int            `json:"chatTimeoutSeconds"`
	ImageTimeoutSeconds int            `json:"imageTimeoutSeconds"`
	LogLevel            string         `json:"logLevel"`
	DebugLogPath        string         `json:"debugLogPath"`
	ListenAddress       string         `json:"listenAddress"`
	ConfigPath          string         `json:"configPath"`
	TokenCachePath      string         `json:"tokenCachePath"`
	SessionCachePath    string         `json:"sessionCachePath"`
	OutboundProxy       string         `json:"outboundProxy"`
	ProxyPool           []string       `json:"proxyPool,omitempty"`
	ClientID            string         `json:"clientId"`
	Authority           string         `json:"authority"`
	RedirectURI         string         `json:"redirectUri"`
	Scope               string         `json:"scope"`
	ModelMappings       []modelMapping `json:"modelMappings"`
	ToolPlanningMode    string         `json:"toolPlanningMode"`
	CaptureRouterFrames bool           `json:"captureRouterFrames"`
	ClientProfile       string         `json:"clientProfile"`
	// NativePanelRoot/NativePanelPython persist where the local registration and
	// OAuth workers live, so 4141 keeps working after a restart without relying on
	// per-process environment variables. The matching env vars still win when set.
	NativePanelRoot     string         `json:"nativePanelRoot,omitempty"`
	NativePanelPython   string         `json:"nativePanelPython,omitempty"`

	// 以下字段对应既有的环境变量开关，之前只能改环境变量、无法在界面上调。
	// 参照 grok2api 的配置分组（质量守护 / 并发 / 诊断 / 清理）补齐。
	// 零值表示「沿用内置默认」，ApplyStartupSettingsEnv 不会写入空值。

	// 代理质量守护：巡检间隔、并发、总开关。见 internal/outbound/guard.go。
	ProxyGuardInterval    string `json:"proxyGuardInterval,omitempty"`
	ProxyGuardConcurrency int    `json:"proxyGuardConcurrency,omitempty"`
	ProxyGuardDisabled    bool   `json:"proxyGuardDisabled,omitempty"`
	// AllowFakeIPSource 放行 198.18.0.0/15：Clash TUN 的 fake-ip 会把真实域名
	// 映射到该段，一律拦死会让免费代理拉取在这类环境里永远失败。
	AllowFakeIPSource bool `json:"allowFakeIpSource,omitempty"`

	// 并发上限：全局聊天并发与单账号并发。
	MaxConcurrentChats int `json:"maxConcurrentChats,omitempty"`
	AccountConcurrency int `json:"accountConcurrency,omitempty"`

	// 对话自动清理：间隔（分钟）、过期时长（小时）、云端保留条数。
	AutoCleanupIntervalMinutes int  `json:"autoCleanupIntervalMinutes,omitempty"`
	AutoCleanupMaxAgeHours     int  `json:"autoCleanupMaxAgeHours,omitempty"`
	AutoCleanupKeepN           int  `json:"autoCleanupKeepN,omitempty"`
	AutoCleanupDisabled        bool `json:"autoCleanupDisabled,omitempty"`

	// 诊断：落盘间隔与 HTTP 访问日志详略。
	PersistInterval  string `json:"persistInterval,omitempty"`
	HTTPTraceVerbose bool   `json:"httpTraceVerbose,omitempty"`
}

type settingsStore struct {
	mu   sync.RWMutex
	path string
	v    runtimeSettings
}

func envInt(name string, fallback int) int {
	n, e := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if e == nil && n > 0 {
		return n
	}
	return fallback
}
func defaultRuntimeSettings() runtimeSettings {
	return runtimeSettings{
		MaxToolCallsPerTurn: envInt("M365_MAX_TOOL_CALLS_PER_TURN", 32), MaxToolRounds: envInt("M365_MAX_TOOL_ROUNDS", 512),
		ContextWindow: envInt("M365_CONTEXT_WINDOW", 262144), MaxOutputTokens: envInt("M365_MAX_OUTPUT_TOKENS", 16384),
		ChatTimeoutSeconds: envInt("M365_CHAT_TIMEOUT_SECONDS", 600), ImageTimeoutSeconds: envInt("M365_IMAGE_TIMEOUT_SECONDS", 180), LogLevel: firstNonEmptySetting(os.Getenv("M365_LOG_LEVEL"), "info"),
		DebugLogPath: os.Getenv("M365_DEBUG_LOG"), ListenAddress: os.Getenv("M365_LISTEN"), ConfigPath: os.Getenv("M365_CONFIG"),
		TokenCachePath: os.Getenv("M365_TOKEN_CACHE"), SessionCachePath: os.Getenv("M365_SESSION_CACHE"), OutboundProxy: os.Getenv(outbound.EnvProxy), ClientID: os.Getenv("M365_CLIENT_ID"),
		Authority: os.Getenv("M365_AUTHORITY"), RedirectURI: os.Getenv("M365_REDIRECT_URI"), Scope: os.Getenv("M365_SCOPE"),
		ModelMappings:    append([]modelMapping(nil), defaultModelMappings...),
		ToolPlanningMode: toolPlanningMode(os.Getenv("M365_TOOL_PLANNING_MODE")),
		ClientProfile:    "office",
		NativePanelRoot:   strings.TrimSpace(os.Getenv(nativePanelRootEnv)),
		NativePanelPython: strings.TrimSpace(os.Getenv(nativePanelPythonEnv)),
	}
}
func settingsPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	if p := strings.TrimSpace(os.Getenv("M365_SETTINGS_FILE")); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "m365-copilot2api", "settings.json")
}

var openSettingsStore = sync.OnceValue(func() *settingsStore {
	s := &settingsStore{path: settingsPath(), v: defaultRuntimeSettings()}
	if b, e := os.ReadFile(s.path); e == nil {
		_ = json.Unmarshal(b, &s.v)
	}
	if e := validateSettings(s.v); e != nil {
		log.Printf("[settings] invalid persisted settings: %v", e)
	}
	return s
})

func firstNonEmptySetting(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func validateSettings(v runtimeSettings) error {
	if v.MaxToolCallsPerTurn < 1 || v.MaxToolCallsPerTurn > 64 {
		return fmt.Errorf("每轮工具调用数必须为 1-64")
	}
	if v.MaxToolRounds < 1 || v.MaxToolRounds > 512 {
		return fmt.Errorf("最大工具轮次必须为 1-512")
	}
	if v.ContextWindow < 1024 {
		return fmt.Errorf("上下文窗口不能小于 1024")
	}
	if v.MaxOutputTokens < 1 || v.MaxOutputTokens >= v.ContextWindow {
		return fmt.Errorf("最大输出必须大于 0 且小于上下文窗口")
	}
	if v.ChatTimeoutSeconds < 5 || v.ChatTimeoutSeconds > 3600 {
		return fmt.Errorf("聊天超时必须为 5-3600 秒")
	}
	if v.ImageTimeoutSeconds < 5 || v.ImageTimeoutSeconds > 3600 {
		return fmt.Errorf("图片超时必须为 5-3600 秒")
	}
	if v.LogLevel != "silent" && v.LogLevel != "error" && v.LogLevel != "warn" && v.LogLevel != "info" && v.LogLevel != "debug" {
		return fmt.Errorf("日志等级必须为 silent、error、warn、info 或 debug")
	}
	if err := outbound.ValidateProxyURL(v.OutboundProxy); err != nil {
		return err
	}
	for _, proxyURL := range v.ProxyPool {
		if err := outbound.ValidateProxyURL(strings.TrimSpace(proxyURL)); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(v.ModelMappings))
	for _, mapping := range v.ModelMappings {
		model := strings.TrimSpace(mapping.PublicModel)
		if !publicModelID.MatchString(model) {
			return fmt.Errorf("公开模型 ID 只能包含字母、数字、点、下划线或连字符，且长度为 1-128")
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("公开模型 ID %q 重复", model)
		}
		seen[key] = struct{}{}
		if !validUpstreamTone(strings.TrimSpace(mapping.UpstreamTone)) {
			return fmt.Errorf("上游 tone %q 不受支持", mapping.UpstreamTone)
		}
		if strings.TrimSpace(mapping.DisplayName) == "" {
			return fmt.Errorf("公开模型 %q 缺少显示名称", model)
		}
		if _, err := normalizeReasoningEffort(mapping.DefaultReasoningLevel); err != nil || strings.TrimSpace(mapping.DefaultReasoningLevel) == "" {
			return fmt.Errorf("公开模型 %q 的默认推理级别无效", model)
		}
	}
	return nil
}
func (s *settingsStore) get() runtimeSettings { s.mu.RLock(); defer s.mu.RUnlock(); return s.v }
func (s *settingsStore) save(v runtimeSettings) error {
	if e := validateSettings(v); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if e := os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		return e
	}
	if e := writeFileAtomic(s.path, b, 0600); e != nil {
		return e
	}
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
	return nil
}
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// The proxy pool carries userinfo, so the response must not echo it raw:
		// this endpoint is what put the Azure proxy password into the dashboard
		// payload. Redact on the way out only - the stored value stays dialable,
		// and PUT merges field by field, so an untouched proxyPool is never
		// overwritten with the "***" placeholder.
		view := s.settings.get()
		view.ProxyPool = outbound.RedactProxyURLs(view.ProxyPool)
		view.OutboundProxy = outbound.RedactProxyURL(view.OutboundProxy)
		jsonOut(w, map[string]any{"settings": view, "codexModels": configurableCodexModels, "upstreamTones": knownUpstreamTones(), "restartRequiredFields": settingsRestartRequiredFields})
	case http.MethodPut:
		// 前端可能只修改一个字段（如监听地址），其余字段以零值提交。
		// 逐字段合并到当前设置再校验，避免"改一个字段弄丢其他配置"。
		cur := s.settings.get()
		base, _ := json.Marshal(cur)
		var merged map[string]any
		if json.Unmarshal(base, &merged) != nil {
			writeOpenAIError(w, 500, "internal_error", "marshal settings")
			return
		}
		var patch map[string]any
		if json.NewDecoder(r.Body).Decode(&patch) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		for k, v := range patch {
			merged[k] = v
		}
		mergedJSON, _ := json.Marshal(merged)
		var v runtimeSettings
		if json.Unmarshal(mergedJSON, &v) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		if e := s.settings.save(v); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		if e := outbound.ConfigurePool(v.ProxyPool); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		chathub.SetClientProfile(v.ClientProfile)
		chathub.EnableWireCapture(v.CaptureRouterFrames)
		jsonOut(w, map[string]any{"ok": true, "settings": v})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}
func configuredToolCallLimit(s *settingsStore) int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_CALLS_PER_TURN"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= 1 && n <= 64 {
			return n
		}
		return 1
	}
	return s.get().MaxToolCallsPerTurn
}

// adaptiveToolCallLimit permits parallel calls only when every call is a
// read-only, independently addressable operation. Any write, execution,
// mutation, or ambiguous tool is serialized conservatively.
func adaptiveToolCallLimit(c []detectedToolCall, configured int) int {
	if len(c) < 2 || configured < 2 {
		return 1
	}
	for _, call := range c {
		name := strings.ToLower(strings.TrimSpace(call.Name))
		if name == "" || toolLooksMutating(name) || !toolLooksReadOnly(name) {
			return 1
		}
	}
	return configured
}

func toolLooksMutating(name string) bool {
	for _, word := range []string{"exec", "shell", "command", "write", "edit", "update", "delete", "remove", "move", "rename", "create", "patch", "apply", "install", "run"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func toolLooksReadOnly(name string) bool {
	for _, word := range []string{"read", "list", "search", "find", "get", "fetch", "browser", "lookup", "inspect", "stat", "status", "describe", "info"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func limitToolCalls(c []detectedToolCall, n int) []detectedToolCall {
	if n < 1 {
		n = 1
	}
	if len(c) > n {
		return c[:n]
	}
	return c
}

func currentSettings() runtimeSettings { return openSettingsStore().get() }

// ApplyStartupSettingsEnv loads persisted restart-required fields before the
// rest of the application initializes. Explicit process environment variables
// always win over values saved from the web console.
func ApplyStartupSettingsEnv() {
	s := openSettingsStore().get()
	values := map[string]string{"M365_LISTEN": s.ListenAddress, "M365_CONFIG": s.ConfigPath, "M365_TOKEN_CACHE": s.TokenCachePath, "M365_SESSION_CACHE": s.SessionCachePath, outbound.EnvProxy: s.OutboundProxy, "M365_PROXY_POOL": strings.Join(s.ProxyPool, "\n"), "M365_CLIENT_ID": s.ClientID, "M365_AUTHORITY": s.Authority, "M365_REDIRECT_URI": s.RedirectURI, "M365_SCOPE": s.Scope, "M365_DEBUG_LOG": s.DebugLogPath}
	// 新增设置项 → 环境变量。只注入非零值，保持「零值 = 沿用默认」。
	for key, value := range settingsEnvOverrides(s) {
		values[key] = value
	}
	for k, v := range values {
		if _, exists := os.LookupEnv(k); !exists && strings.TrimSpace(v) != "" {
			_ = os.Setenv(k, v)
		}
	}
}

// settingsEnvOverrides 把界面上的运行参数翻译成进程环境变量。
//
// 这些开关此前只能通过环境变量设置，界面上无从调整。翻译集中在一处，避免
// 每个消费方各自去读设置文件而与环境变量语义分叉。
func settingsEnvOverrides(s runtimeSettings) map[string]string {
	out := map[string]string{}
	put := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	putInt := func(key string, value int) {
		if value > 0 {
			out[key] = strconv.Itoa(value)
		}
	}
	putBool := func(key string, value bool) {
		if value {
			out[key] = "1"
		}
	}

	put(outbound.EnvProxyGuardInterval, s.ProxyGuardInterval)
	putInt(outbound.EnvProxyGuardConcurrency, s.ProxyGuardConcurrency)
	putBool(outbound.EnvProxyGuardDisabled, s.ProxyGuardDisabled)
	putBool(EnvAllowFakeIPSource, s.AllowFakeIPSource)

	putInt("M365_MAX_CONCURRENT_CHATS", s.MaxConcurrentChats)
	// 变量名以 account_concurrency.go:29 的实际读取为准，不能凭直觉命名。
	putInt("M365_ACCOUNT_DEFAULT_CONCURRENCY", s.AccountConcurrency)

	putInt("M365_AUTO_CLEANUP_INTERVAL_MINUTES", s.AutoCleanupIntervalMinutes)
	putInt("M365_AUTO_CLEANUP_MAX_AGE_HOURS", s.AutoCleanupMaxAgeHours)
	putInt("M365_AUTO_CLEANUP_KEEP_N", s.AutoCleanupKeepN)
	if s.AutoCleanupDisabled {
		// 该开关读的是「非空即禁用」，用 0 表达关闭最直观。
		out["M365_AUTO_CLEANUP"] = "0"
	}

	put("M365_PERSIST_INTERVAL", s.PersistInterval)
	putBool("M365_HTTP_TRACE_VERBOSE", s.HTTPTraceVerbose)
	return out
}

// settingsRestartRequiredFields 列出只在进程启动时读取的设置项。前端据此在保存
// 前提示「需要重启」。质量守护、并发、清理、诊断这几组都通过
// ApplyStartupSettingsEnv 注入环境变量，因此同样需要重启才生效 —— 之前它们
// 只能改环境变量，界面上无从调整，也就没有这个提示。
var settingsRestartRequiredFields = []string{
	"listenAddress", "configPath", "tokenCachePath", "sessionCachePath",
	"outboundProxy", "proxyPool", "clientId", "authority", "redirectUri",
	"scope", "debugLogPath",
	"proxyGuardInterval", "proxyGuardConcurrency", "proxyGuardDisabled",
	"allowFakeIpSource", "maxConcurrentChats", "accountConcurrency",
	"autoCleanupIntervalMinutes", "autoCleanupMaxAgeHours", "autoCleanupKeepN",
	"autoCleanupDisabled", "persistInterval", "httpTraceVerbose",
}
