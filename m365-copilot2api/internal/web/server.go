package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/mcp"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type pendingPKCE struct {
	Verifier string
	Created  time.Time
	// Attempt is a monotonically increasing, server-local generation. State is
	// still the PKCE/CSRF binding; Attempt prevents an in-flight old callback
	// from committing after a later start/reset has replaced the state map.
	Attempt uint64
	Status  string
	Account any
	Error   string
	// Exchanging guards the authorization code exchange so that a double
	// callback (browser retry, refreshed callback tab, or a second paste of the
	// same URL) cannot redeem the same code twice.
	Exchanging bool
	// Consumed marks a state whose code was already redeemed. The entry is kept
	// briefly so /api/auth/status can still report success, but any further
	// callback on it is rejected instead of re-entering the login flow.
	Consumed bool
}

// pkceStateTTL bounds how long an unused authorization request stays valid.
const pkceStateTTL = 10 * time.Minute

// pkceConsumedTTL keeps a finished state around long enough for the UI poller
// to observe the terminal status, then drops it so nothing can be replayed.
const pkceConsumedTTL = 2 * time.Minute

const pkceInactiveMessage = "authorization attempt is no longer active; start a new authorization"

// prunePKCELocked removes expired and finished authorization attempts. Stale
// entries are what made the UI jump back into a previous callback: the poller
// kept seeing an old terminal state and the browser session was never cleared.
// Callers must hold s.mu.
func (s *Server) prunePKCELocked() {
	if s.pkce == nil {
		s.pkce = map[string]pendingPKCE{}
		return
	}
	now := time.Now()
	for state, p := range s.pkce {
		age := now.Sub(p.Created)
		switch {
		case age > pkceStateTTL:
			delete(s.pkce, state)
		case p.Consumed && age > pkceConsumedTTL:
			delete(s.pkce, state)
		case p.Status == "error" && age > pkceConsumedTTL:
			delete(s.pkce, state)
		}
	}
}

// nextPKCEAttemptLocked advances the local generation used to isolate browser
// authorization attempts. Callers must hold s.mu.
func (s *Server) nextPKCEAttemptLocked() uint64 {
	s.pkceAttempt++
	// A wrap is impractical, but zero is reserved for old in-memory entries
	// created before this field existed.
	if s.pkceAttempt == 0 {
		s.pkceAttempt++
	}
	return s.pkceAttempt
}

// failPKCEAttempt records an exchange failure only while this exact attempt is
// still active. A reset/new start removes the entry, so a late network result
// cannot resurrect an old callback state.
func (s *Server) failPKCEAttempt(state string, attempt uint64, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pkce[state]
	if !ok || p.Attempt != attempt || !p.Exchanging {
		return false
	}
	p.Exchanging = false
	p.Consumed = true
	p.Status = "error"
	p.Error = message
	s.pkce[state] = p
	return true
}

// finishPKCEAttempt is the commit point for an authorization callback. It
// keeps the PKCE mutex while persisting the account so reset/start and a late
// callback have a clear order: whichever acquires the mutex first wins.
func (s *Server) finishPKCEAttempt(state string, attempt uint64, tok auth.TokenSet) (auth.AccountToken, bool, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pkce[state]
	if !ok || p.Attempt != attempt || !p.Exchanging {
		return auth.AccountToken{}, false, nil, false
	}
	if s.tokens == nil {
		err := errors.New("account store unavailable")
		p.Exchanging = false
		p.Consumed = true
		p.Status = "error"
		p.Error = err.Error()
		s.pkce[state] = p
		return auth.AccountToken{}, false, err, true
	}

	priorID := tok.HomeOID
	if priorID == "" {
		priorID = tok.Email
	}
	_, alreadyLinked := s.tokens.Get(priorID)
	acc, err := s.tokens.Upsert(tok)
	if err != nil {
		p.Exchanging = false
		p.Consumed = true
		p.Status = "error"
		p.Error = err.Error()
		s.pkce[state] = p
		return auth.AccountToken{}, false, err, true
	}

	p.Exchanging = false
	p.Consumed = true
	p.Status = "authenticated"
	p.Account = map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "oid": acc.OID, "tid": acc.TID,
		"duplicate": alreadyLinked,
	}
	s.pkce[state] = p
	return acc, alreadyLinked, nil, true
}

// rateLimitCooldown is how long a rate-limited account stays out of rotation.
const rateLimitCooldown = 3 * time.Minute

const rateLimitProbePrompt = "Reply with exactly: OK"

// confirmRateLimitNotice verifies a text-channel rate-limit notice with a
// separate, fresh ChatHub conversation. A single notice is not enough to cool
// down an account because the upstream can occasionally emit a false positive.
func (s *Server) confirmRateLimitNotice(ctx context.Context, acc auth.AccountToken, noticeErr error) (bool, error) {
	if !errors.Is(noticeErr, chathub.ErrRateLimitNotice) {
		return IsRateLimited(noticeErr), noticeErr
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, probeErr := s.chatWithAccount(probeCtx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:    rateLimitProbePrompt,
		Tone:    "magic",
		Started: true,
	})
	if probeErr == nil {
		return false, nil
	}
	if errors.Is(probeErr, chathub.ErrRateLimitNotice) || IsRateLimited(probeErr) {
		return true, &UpstreamHTTPError{
			Status:     http.StatusTooManyRequests,
			RetryAfter: int(rateLimitCooldown.Seconds()),
		}
	}
	return false, probeErr
}

type Server struct {
	mu                 sync.Mutex
	tokens             *auth.Store
	accountPool        *accountHealth
	upstreamCooldown   *accountCooldown
	accountConcurrency *accountConcurrency
	resourceScheduler  *resourceScheduler
	// 批量授权的出口轮换计数。归服务端所有，因为恢复流程是十几次请求累计几百个
	// 账号，而单批上限只有 64：计数若随请求重置，每 100 个换一次出口就永远不会触发。
	oauthExitProcessed atomic.Int64
	oauthExitTurn      atomic.Int64
	pkce               map[string]pendingPKCE
	pkceAttempt        uint64
	// exchangePKCECode is nil in production and falls back to auth.ExchangeCode.
	// Keeping it on Server lets httptest exercise callback races without making a
	// real OAuth request.
	exchangePKCECode    func(code, verifier, redirectURI string) (auth.TokenSet, error)
	chat                *chathub.Client
	sessions            *sessionStore
	userSessions        *userSessionStore
	sessionResolver     *sessionResolver
	historyArchive      *historyArchiveStore
	conversationManager *conversationManager
	adminPassword       string
	adminSessions       map[string]time.Time
	mustChangePassword  bool
	loginAttempts       map[string]loginAttempt
	apiKeys             *apiKeyStore
	keyLimits           *keyConcurrency
	keyRates            *keyRateLimiter
	debug               *debugStore
	settings            *settingsStore
	responseMu          sync.Mutex
	responseMessages    map[string]map[string]respHistory
	usage               *usageLog
	benchmark           *benchmarkStore
	generatedImages     map[string]generatedImage
	// regJob 是批量注册长跑任务。归服务端所有，因为它必须活得比发起它的 HTTP 请求长 ——
	// 目标是几千个号，而单次注册接口上限 20 个。
	regJob   *registerJob
	regJobMu sync.Mutex
}

// registerJob 惰性取批量注册任务的持有者。
//
// 惰性是必要的：Server 在测试里常用 &Server{} 直接构造，构造函数里初始化的字段在那些
// 用例里是零值。
func (s *Server) registerJob() *registerJob {
	s.regJobMu.Lock()
	defer s.regJobMu.Unlock()
	if s.regJob == nil {
		s.regJob = &registerJob{}
	}
	return s.regJob
}

const maxResponsesPerTenant = 256

type respHistory struct {
	At       time.Time
	Messages []oaiMsg
}

func New() (*Server, error) {
	store, err := auth.OpenStore("")
	if err != nil {
		return nil, err
	}
	password, mustChange := loadAdminPassword()
	sessionTTL := 30 * time.Minute
	if v := os.Getenv("M365_USER_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			sessionTTL = d
		}
	}
	settings := openSettingsStore()
	configured := settings.get()
	chathub.SetClientProfile(configured.ClientProfile)
	chathub.EnableWireCapture(configured.CaptureRouterFrames)
	return &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		upstreamCooldown:   newAccountCooldown(),
		accountConcurrency: newAccountConcurrency(),
		resourceScheduler:  newResourceScheduler(),
		pkce:               map[string]pendingPKCE{},
		// One Client for the process lifetime is fine: it holds no snapshot of the
		// outbound configuration, so proxy-pool and client-profile edits are picked
		// up on the next request. See chathub.Client.
		chat: func() *chathub.Client {
			c := chathub.NewClient()
			c.Trace = func(meta map[string]any) { fmt.Printf("[multimodal-trace] %s\\n", mustJSON(meta)) }
			return c
		}(),
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(sessionTTL),
		sessionResolver:     openSessionResolver(),
		historyArchive:      openHistoryArchive(),
		conversationManager: openConversationManager(),
		adminPassword:       password,
		adminSessions:       map[string]time.Time{},
		mustChangePassword:  mustChange,
		loginAttempts:       map[string]loginAttempt{},
		apiKeys:             openAPIKeys(),
		keyLimits:           newKeyConcurrency(),
		keyRates:            newKeyRateLimiter(),
		debug:               openDebugStore(),
		settings:            settings,
		responseMessages:    map[string]map[string]respHistory{},
		usage:               openUsageLog(),
		benchmark:           &benchmarkStore{run: benchmarkRun{State: "idle"}},
		generatedImages:     map[string]generatedImage{},
	}, nil
}

func (s *Server) InitM365CloudClient() {
	accounts := s.tokens.List()
	if len(accounts) == 0 {
		return
	}
	acc := accounts[0]
	clientID := os.Getenv("M365_CLIENT_ID")
	if clientID == "" {
		clientID = acc.ClientID
	}
	if clientID == "" {
		clientID = auth.DefaultClientID
	}
	InitM365CloudClient(clientID, acc.TID, acc.RefreshToken)
	log.Printf("[m365-cloud] client initialized for account %s", acc.Email)
}

func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/admin/login", s.adminLogin)
	m.HandleFunc("/api/admin/logout", s.adminLogout)
	m.HandleFunc("/api/admin/session", s.adminSession)
	m.HandleFunc("/api/admin/change-password", s.adminChangePassword)
	m.HandleFunc("/api/admin/keys", s.adminKeys)
	m.HandleFunc("/api/admin/keys/reveal", s.adminKeyReveal)
	m.HandleFunc("/api/admin/live-metrics", s.handleLiveMetrics)
	m.HandleFunc("/api/accounts/credentials/sync", s.handleCredentialSync)
	m.HandleFunc("/api/admin/models", s.adminModels)
	m.HandleFunc("/api/admin/models/test", s.adminModelTest)
	m.HandleFunc("/api/admin/settings", s.adminSettings)
	m.HandleFunc("/api/admin/proxy-pool", s.proxyPool)
	m.HandleFunc("/api/admin/deployments", s.deployments)
	m.HandleFunc("/api/admin/deployment", s.deploymentAction)
	m.HandleFunc("/api/admin/deployment/check", s.deploymentCheck)
	m.HandleFunc("/api/admin/debug/logs", s.debugList)
	m.HandleFunc("/api/admin/debug/detail", s.debugDetail)
	m.HandleFunc("/api/admin/debug/wire", s.handleWireFrames)
	m.HandleFunc("/api/admin/debug/wire/toggle", s.handleWireFramesToggle)
	m.HandleFunc("/api/admin/debug/router-frames", s.handleRouterFrames)
	m.HandleFunc("/api/admin/debug/router-frames/toggle", s.handleRouterFramesToggle)
	m.HandleFunc("/api/admin/account-health", s.adminAccountHealth)
	m.HandleFunc("/api/admin/benchmark", s.adminBenchmark)
	m.HandleFunc("/api/admin/benchmark/run", s.adminBenchmarkRun)
	m.HandleFunc("/api/admin/benchmark/stop", s.adminBenchmarkStop)
	m.HandleFunc("/api/live", s.handleLiveness)
	m.HandleFunc("/api/stages", s.handleStageLog)
	m.HandleFunc("/api/health", s.health)
	m.HandleFunc("/api/version", s.version)
	m.HandleFunc("/api/update", s.update)
	m.HandleFunc("/api/accounts", s.accounts)
	m.HandleFunc("/api/accounts/refresh", s.refreshAccount)
	m.HandleFunc("/api/accounts/refresh-all", s.refreshAllAccounts)
	m.HandleFunc("/api/accounts/delete", s.deleteAccount)
	m.HandleFunc("/api/accounts/provision", s.provisionAccount)
	m.HandleFunc("/api/admin/accounts/reassign", s.reassignAccount)
	// Registration and OAuth are native 4141 handlers; no local 8555 panel proxy is used.
	s.RegisterNativePanelRoutes(m)
	m.HandleFunc("/api/accounts/credentials", s.accountCredentials)
	m.HandleFunc("/api/accounts/web/run-scripts", s.accountRunScripts)
	m.HandleFunc("/api/auth/start", s.startPKCE)
	m.HandleFunc("/api/auth/status", s.pkceStatus)
	m.HandleFunc("/api/auth/callback", s.callbackPKCE)
	m.HandleFunc("/api/auth/reset", s.resetPKCE)
	m.HandleFunc("/api/chat", s.chatOnce)
	m.HandleFunc("/api/chat/stream", s.chatStream)
	m.HandleFunc("/api/conversations", s.conversations)
	m.HandleFunc("/api/conversations/capture", s.captureConversations)
	m.HandleFunc("/api/conversations/detail", s.conversationDetail)
	m.HandleFunc("/api/conversations/delete", s.deleteConversation)
	m.HandleFunc("/api/conversations/cleanup", s.conversationCleanup)
	m.HandleFunc("/api/conversations/whitelist", s.conversationWhitelist)
	m.HandleFunc("/v1/sessions", s.handleSessions)
	m.HandleFunc("/v1/sessions/", s.handleSessionDelete)
	m.HandleFunc("/api/m365/conversations", s.handleM365Conversations)
	m.HandleFunc("/api/m365/conversations/delete", s.handleM365Delete)
	m.HandleFunc("/api/m365/conversations/cleanup", s.handleM365Cleanup)
	m.HandleFunc("/api/stats", s.handleCacheStats)
	m.HandleFunc("/api/stats/reset", s.handleCacheStatsReset)
	m.HandleFunc("/api/usage", s.adminUsage)
	m.HandleFunc("/api/usage/logs", s.adminUsageLogs)
	m.HandleFunc("/api/resources/status", s.resourceStatus)
	m.HandleFunc("/api/contributions/ledger", s.contributionLedger)
	m.HandleFunc("/v1/mcp", mcp.HandleStreamable)
	m.HandleFunc("/v1/mcp/sse", mcp.HandleSSE)
	m.HandleFunc("/v1/mcp/message", mcp.HandleMessage)
	m.HandleFunc("/v1/mcp/tools", mcp.HandleToolsList)
	m.HandleFunc("/v1/models", s.openaiModels)
	m.HandleFunc("/v1/chat/completions", s.openaiChat)
	m.HandleFunc("/v1/responses", s.responses)
	// count_tokens must be registered before /v1/messages is matched: ServeMux
	// treats a pattern without a trailing slash as an exact path, so
	// /v1/messages never covered the sub-path and it fell through to "/".
	m.HandleFunc("/v1/messages/count_tokens", s.anthropicCountTokens)
	m.HandleFunc("/v1/messages", s.anthropicMessages)
	m.HandleFunc("/v1/images/generations", s.imageGenerations)
	m.HandleFunc("/v1/images/edits", s.imageEdits)
	m.HandleFunc("/v1/images/files/", s.generatedImageFile)
	m.HandleFunc("/", s.rootPage)
	return recoverPanics(requestID(httpTrace(securityHeaders(s.adminMiddleware(s.debugMiddleware(m))))))
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/images/files/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" || r.URL.Path == "/api/admin/logout" || r.URL.Path == "/api/auth/start" || r.URL.Path == "/api/auth/status" || r.URL.Path == "/api/auth/callback" || r.URL.Path == "/api/auth/reset" || r.URL.Path == "/api/live" || r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/workbench" || r.URL.Path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			if !s.validAPIKey(r) {
				http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// 本机只读诊断豁免：原生 DiagActivity 固定访问 http://127.0.0.1:4141，
		// 而它把会话 cookie 存在实例字段里，页面一关就丢，导致每次进诊断页都
		// 要重新登录、退出再进就什么都看不到。这些端点只读且不含凭据（帧内容
		// 已脱敏），仅对回环地址放行，对外访问仍需管理员会话。
		if isLoopbackRequest(r) && isReadOnlyDiagnosticPath(r.URL.Path) && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		// 捕获开关也需本机豁免：否则诊断页登录态一丢就再也开不了捕获，
		// 只读放行也就失去意义。它只切换一个布尔量，不返回任何凭据。
		if isLoopbackRequest(r) && isCaptureToggle(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.adminPassword == "" {
			http.Error(w, `{"error":{"message":"administrator password is not configured","type":"configuration_error"}}`, http.StatusServiceUnavailable)
			return
		}
		if !s.validAdminSession(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
			return
		}
		s.mu.Lock()
		mustChange := s.mustChangePassword
		s.mu.Unlock()
		if mustChange && r.URL.Path != "/api/admin/change-password" && r.URL.Path != "/api/admin/logout" {
			writeOpenAIError(w, http.StatusForbidden, "password_change_required", "administrator password must be changed before using the console")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackRequest 判断请求是否来自本机回环地址。
func isLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host).IsLoopback()
}

// isCaptureToggle 是仅切换帧捕获开关的端点。它不读写凭据、不返回敏感数据，
// 因此与只读诊断端点一并对本机放行。
func isCaptureToggle(path string) bool {
	switch path {
	case "/api/admin/debug/router-frames/toggle",
		"/api/admin/debug/wire/toggle":
		return true
	}
	return false
}

// isReadOnlyDiagnosticPath 列出可对本机免密开放的只读诊断端点。
// 只包含 GET 语义、内容已脱敏、且不返回令牌或密码的路径。
func isReadOnlyDiagnosticPath(path string) bool {
	switch path {
	case "/api/stages",
		"/api/admin/debug/router-frames",
		"/api/admin/debug/wire",
		"/api/admin/account-health":
		return true
	}
	return false
}

func secureAdminCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Only trust X-Forwarded-Proto from a loopback reverse proxy.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return net.ParseIP(host).IsLoopback() && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) validAdminSession(r *http.Request) bool {
	c, err := r.Cookie("m365_admin_session")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.adminSessions[c.Value]
	if !ok || time.Now().After(expires) {
		delete(s.adminSessions, c.Value)
		return false
	}
	return true
}

const maxAdminSessions = 4096

// pruneAdminSessions drops expired entries; callers must hold s.mu.
func pruneAdminSessions(m map[string]time.Time, now time.Time) {
	for k, exp := range m {
		if now.After(exp) {
			delete(m, k)
		}
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ip, now := clientIP(r), time.Now()
	policy := loginPolicyFor(isLoopbackRequest(r))
	var body struct {
		Password string `json:"password"`
	}
	decodeErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	s.mu.Lock()
	password := s.adminPassword
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	// Verify the credential before consulting the lockout table. A correct
	// password must always be able to get in, otherwise a local client stuck
	// replaying a stale password can keep the real administrator out forever.
	// The comparison stays constant time and runs on every request, whether or
	// not this source is locked, so the reply timing does not reveal which of
	// the two conditions rejected the attempt.
	valid := subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) == 1
	if decodeErr != nil || body.Password == "" || !valid {
		// Only a failing attempt is subject to the lockout. While locked the
		// answer is 429 rather than 401 so it stays indistinguishable from the
		// reply a locked-out correct password would have produced.
		locked, wait := s.recordLoginFailure(ip, now, policy)
		if locked {
			seconds := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", fmt.Sprint(seconds))
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many failed login attempts; try again later")
			return
		}
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid administrator password")
		return
	}
	// A successful login resets the budget for this source immediately, which
	// is what lets a legitimate administrator break an in-progress lockout.
	s.clearLoginFailures(ip)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeOpenAIError(w, 500, "internal_error", "session failure")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	pruneAdminSessions(s.adminSessions, now)
	if len(s.adminSessions) >= maxAdminSessions {
		// Evict the oldest entry to keep the map bounded.
		var oldest string
		var oldestExp time.Time
		for k, exp := range s.adminSessions {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(s.adminSessions, oldest)
	}
	s.adminSessions[token] = now.Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Value: token, Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	jsonOut(w, map[string]any{"status": "authenticated", "must_change_password": mustChange})
}
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("m365_admin_session"); e == nil {
		s.mu.Lock()
		delete(s.adminSessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]string{"status": "logged_out"})
}
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	authenticated := s.validAdminSession(r)
	s.mu.Lock()
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	jsonOut(w, map[string]bool{"authenticated": authenticated, "must_change_password": authenticated && mustChange})
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"keys": s.apiKeys.listViews()})
	case http.MethodPost:
		// secret 可选。为空则照旧随机生成 key；非空则把它当作完整 key 明文，
		// 校验形状与唯一性后只落 sha256 摘要。明文既不入日志也不入错误消息。
		var b struct {
			Name   string `json:"name"`
			Secret string `json:"secret"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if strings.TrimSpace(b.Name) == "" {
			b.Name = "API key"
		}
		// secret 不做 TrimSpace：空白本身就是非法字符，静默修剪会让调用方
		// 拿到一个与它提交的字符串不同的 key。
		rec, raw, e := s.apiKeys.createWithSecret(b.Name, b.Secret)
		if status := keySecretHTTPStatus(e); status != 0 {
			typ := "invalid_request_error"
			if status == http.StatusConflict {
				typ = "duplicate_key_error"
			}
			writeOpenAIError(w, status, typ, e.Error())
			return
		}
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		// 保存明文以支持后续回显。失败不回滚 key 本身（它已经可用），只在响应里
		// 说明这一个 key 无法回显，避免用户以为回显功能整体坏了。
		retained := true
		var retainNote string
		if err := s.retainAPIKeySecret(rec.ID, rec.Name, raw); err != nil {
			retained = false
			retainNote = "密钥已创建，但明文保存失败，之后无法回显：" + err.Error()
		}
		jsonOut(w, map[string]any{"key": raw, "record": newAPIKeyView(rec), "retained": retained, "retainNote": retainNote})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		deleted, e := s.apiKeys.delete(id)
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "key not found", 404)
			return
		}
		// 同步清掉 vault 里的明文，不留孤儿密钥。
		s.forgetAPIKeySecret(id)
		jsonOut(w, map[string]string{"status": "deleted"})
	// PUT 与 PATCH 共用同一部分更新语义：只有请求体里出现的字段被写入，
	// 缺省字段保持原值。PUT 保留是为了不破坏既有前端调用。
	case http.MethodPut, http.MethodPatch:
		var b struct {
			ID string `json:"id"`
			apiKeyPatch
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.ID) == "" {
			http.Error(w, "bad json", 400)
			return
		}
		record, updated, e := s.apiKeys.update(strings.TrimSpace(b.ID), b.apiKeyPatch)
		if errors.Is(e, errConflictingKeyState) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", e.Error())
			return
		}
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		if !updated {
			http.Error(w, "key not found", 404)
			return
		}
		jsonOut(w, map[string]any{"status": "updated", "record": newAPIKeyView(record)})
	default:
		http.Error(w, "method not allowed", 405)
	}
}
func (s *Server) validAPIKey(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if raw == "" {
		v := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			raw = strings.TrimSpace(v[7:])
		}
	}
	if raw != "" && s.apiKeys.valid(raw) {
		return true
	}
	// 允许直接用网关自己持有的 M365 access token 作为凭据，但必须逐字匹配。
	//
	// 这里原本是 strings.HasPrefix(raw, "eyJ") —— "eyJ" 只是 `{"` 的 base64 前缀，
	// 于是「Authorization: Bearer eyJ」这三个字符就能通过认证：不验签名、不验过期、
	// 不验签发者。实测活网关对 `Bearer eyJ` 返回 200。任何第三方租户的合法 JWT，
	// 甚至任何以 eyJ 开头的垃圾串，都是有效凭据。
	//
	// 一个网关既没签发也无法验签的 JWT 不能证明任何事，所以形状检查（三段点分）也
	// 不够。改为与本地账号的 access token 常量时间比对：这是唯一可核实的判据。
	return raw != "" && s.matchesKnownAccessToken(raw)
}

// matchesKnownAccessToken 报告 raw 是否等于某个已授权账号的 access token。
//
// 用 subtle.ConstantTimeCompare 逐个比，避免用比较耗时泄漏前缀信息。长度先比是安全
// 的：token 长度本身不是秘密，且能避免对明显不匹配的项做无谓的全长比较。
func (s *Server) matchesKnownAccessToken(raw string) bool {
	if s == nil || s.tokens == nil {
		return false
	}
	candidate := []byte(raw)
	for _, account := range s.tokens.List() {
		known := []byte(strings.TrimSpace(account.AccessToken))
		if len(known) == 0 || len(known) != len(candidate) {
			continue
		}
		if subtle.ConstantTimeCompare(known, candidate) == 1 {
			return true
		}
	}
	return false
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	list := s.tokens.List()
	// 原版 /api/health 不含 accountConcurrency。
	jsonOut(w, map[string]any{
		"status":       "ok",
		"auth":         []string{"pkce"},
		"chat":         "chathub",
		"clientId":     auth.ClientID(),
		"scope":        auth.Scope(),
		"tokenCache":   s.tokens.Path(),
		"accountCount": len(list),
	})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list := s.tokens.List()
	type view struct {
		ID          string    `json:"id"`
		Email       string    `json:"email"`
		DisplayName string    `json:"displayName,omitempty"`
		Status      string    `json:"status"`
		OID         string    `json:"oid,omitempty"`
		TID         string    `json:"tid,omitempty"`
		ExpiresAt   time.Time `json:"expiresAt,omitempty"`
		UpdatedAt   time.Time `json:"updatedAt,omitempty"`
	}
	out := make([]view, 0, len(list))
	for _, a := range list {
		out = append(out, view{
			ID: a.ID, Email: a.Email, DisplayName: a.DisplayName,
			Status: a.Status, OID: a.OID, TID: a.TID,
			ExpiresAt: a.ExpiresAt, UpdatedAt: a.UpdatedAt,
		})
	}
	jsonOut(w, map[string]any{"accounts": out})
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	acc, err := s.tokens.ForceRefresh(strings.TrimSpace(body.ID))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "refreshed", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt, "updatedAt": acc.UpdatedAt,
	}})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.tokens.Delete(body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) provisionAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}
	set, err := auth.ROPC(body.Email, body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "ropc_error", err.Error())
		return
	}
	acc, err := s.tokens.Upsert(set)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "upsert_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "provisioned", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt,
	}})
}

// maxPendingPKCE 限制并发进行中的授权数，避免这张表无界增长。
//
// 它必须严格大于单批授权的账号数上限，否则一批还没走完就开始淘汰自己的
// state，被淘汰的那些账号回调时拿到 "invalid or expired state"，正是
// fix/concurrent-pkce-batch-oauth 要消灭的现象。
//
// 因此这里从 panelOAuthBatchMax 推导，而不是各写一个字面量。原先的注释说
// 「批量授权的并发上限是 16，留出余量」，两个数都对不上代码：批量上限是
// panelOAuthBatchMax = 64（那条注释写的 16 是被删掉的 Python worker 时代的值），
// 而表上限也正好是 64 —— 等于一点余量都没有，满批 64 个账号时第 64 次登记就会
// 淘汰第 1 个。
// 留 2 倍余量：一批占满之后，交互式授权和另一批仍有位置。
// TestPendingPKCETableLeavesHeadroomOverBatchCap 守着这个不变量。
const maxPendingPKCE = 2 * panelOAuthBatchMax

// registerPKCEState 登记一个进行中的授权，并返回它的 generation。
//
// keepOthers 区分两种调用者，二者的正确行为相反：
//
//   - false（浏览器交互式授权）：整表覆盖，只留当前这一个。旧的终态条目残留
//     会让 UI 反复进入上一次的回调。
//   - true（批量并发授权）：必须保留其它进行中的 state。批量脚本会先连着调
//     N 次 /api/auth/start 再逐个回调；若每次都整表覆盖，前 N-1 个账号回调时
//     都会拿到 "invalid or expired state" —— 实测 3 并发时 2 个 http400，
//     只有最后启动的那个成功。
//
// 并发模式下用 Attempt 做隔离已经足够：callback 兑换前会重新校验 Attempt，
// 过期条目由 prunePKCELocked 按 TTL 回收。
func (s *Server) registerPKCEState(state, verifier string, keepOthers bool) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.nextPKCEAttemptLocked()
	if !keepOthers {
		// 浏览器交互式授权：保持「一次只有一个进行中的授权」。旧的终态条目
		// 残留会让 UI 反复进入上一次的回调（见 TestStartPKCEClearsStaleAttempts）。
		s.pkce = map[string]pendingPKCE{
			state: {Verifier: verifier, Created: time.Now(), Attempt: attempt, Status: "pending"},
		}
		return attempt
	}
	s.prunePKCELocked()
	if s.pkce == nil {
		s.pkce = map[string]pendingPKCE{}
	}
	// 满了就淘汰最旧的一条，而不是整表清空。
	for len(s.pkce) >= maxPendingPKCE {
		oldestState, oldest := "", time.Time{}
		for st, p := range s.pkce {
			if oldestState == "" || p.Created.Before(oldest) {
				oldestState, oldest = st, p.Created
			}
		}
		if oldestState == "" {
			break
		}
		delete(s.pkce, oldestState)
	}
	s.pkce[state] = pendingPKCE{Verifier: verifier, Created: time.Now(), Attempt: attempt, Status: "pending"}
	return attempt
}

// beginPKCEAuthorization 生成一次新的 PKCE 授权，返回 state、授权 URL、
// 代数与 redirect URI。startPKCE 与「账密一键回调」的交互式回退路径共用
// 它，因此仓库里只有一套 PKCE 实现。
//
// keepOthers 的含义与 registerPKCEState 完全一致，而且必须由调用方给出：
// 这里原先写死 false，于是每个走这条路的调用者都拿到「整表覆盖」语义。
// 对交互式单账号授权那是对的，对批量授权则是灾难 —— runOAuthBatch 与
// runRegister 都在循环里逐个账号调它，前 N-1 个 state 被后来者抹掉，那些账号
// 回调时只会得到 "invalid or expired state"。这正是
// fix/concurrent-pkce-batch-oauth 修过的缺陷，它当时只修到 /api/auth/start
// 那条 HTTP 路径，Go 内置的批量路径走的是这个函数，所以又原样复现了一遍。
func (s *Server) beginPKCEAuthorization(prompt string, keepOthers bool) (string, string, uint64, string, error) {
	v, err := auth.Verifier()
	if err != nil {
		return "", "", 0, "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", 0, "", err
	}
	state := hex.EncodeToString(b)
	redirectURI := auth.RedirectURI()

	attempt := s.registerPKCEState(state, v, keepOthers)

	url := auth.AuthorizationURLWithPrompt(
		auth.AuthorizeEndpoint(),
		auth.ClientID(),
		redirectURI,
		state,
		auth.Challenge(v),
		auth.Scope(),
		prompt,
	)
	return state, url, attempt, redirectURI, nil
}

func (s *Server) startPKCE(w http.ResponseWriter, r *http.Request) {
	v, err := auth.Verifier()
	if err != nil {
		http.Error(w, "pkce failure", http.StatusInternalServerError)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "state failure", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(b)
	redirectURI := auth.RedirectURI()

	// 批量并发授权必须显式声明，才允许多个 state 并存。默认仍是独占语义，
	// 保证浏览器交互式授权不会被旧条目干扰。
	keepOthers := false
	if r != nil {
		switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("concurrent"))) {
		case "1", "true", "yes":
			keepOthers = true
		}
	}
	attempt := s.registerPKCEState(state, v, keepOthers)

	// Default prompt is login (auth.Prompt). select_account still auto-continues
	// a single signed-in session, so it is only used when the caller asks for it.
	prompt := auth.Prompt()
	if r != nil {
		switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("prompt"))) {
		case "login":
			prompt = "login"
		case "select_account":
			prompt = "select_account"
		case "consent":
			prompt = "consent"
		}
		if truthy(r.URL.Query().Get("forceLogin")) {
			prompt = "login"
		}
	}

	jsonOut(w, map[string]any{
		"status":  "pkce_ready",
		"state":   state,
		"attempt": attempt,
		"url": auth.AuthorizationURLWithPrompt(
			auth.AuthorizeEndpoint(),
			auth.ClientID(),
			redirectURI,
			state,
			auth.Challenge(v),
			auth.Scope(),
			prompt,
		),
		"redirectUri": redirectURI,
		// The UI opens this first so Microsoft drops its browser session;
		// otherwise the next authorization silently reuses the signed-in
		// account and the callback page just repeats the existing identity.
		"logoutUrl": auth.LogoutURL(),
		"prompt":    prompt,
		"note":      "If redirect is nativeclient, paste the final URL/code into /api/auth/callback after login.",
	})
}

// truthy accepts the usual affirmative spellings used by the admin UI.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// resetPKCE clears every pending authorization and tells the UI where to send
// the browser to drop the Microsoft session. This is the explicit escape hatch
// for "the callback page keeps coming back with the old account".
func (s *Server) resetPKCE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	s.mu.Lock()
	cleared := len(s.pkce)
	s.nextPKCEAttemptLocked()
	s.pkce = map[string]pendingPKCE{}
	s.mu.Unlock()
	jsonOut(w, map[string]any{
		"status":    "reset",
		"cleared":   cleared,
		"logoutUrl": auth.LogoutURL(),
	})
}

func (s *Server) pkceStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.prunePKCELocked()
	p, ok := s.pkce[state]
	s.mu.Unlock()
	if !ok {
		jsonOut(w, map[string]any{"status": "expired", "terminal": true})
		return
	}
	out := map[string]any{
		"status":   p.Status,
		"attempt":  p.Attempt,
		"terminal": p.Consumed || p.Status == "authenticated" || p.Status == "error",
	}
	if p.Account != nil {
		out["account"] = p.Account
	}
	if p.Error != "" {
		out["error"] = p.Error
	}
	jsonOut(w, out)
}

func (s *Server) callbackPKCE(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	// also accept pasted full callback URL
	if code == "" {
		if u := r.URL.Query().Get("url"); u != "" {
			if parsed, err := http.NewRequest(http.MethodGet, u, nil); err == nil {
				code = parsed.URL.Query().Get("code")
				if state == "" {
					state = parsed.URL.Query().Get("state")
				}
			}
		}
	}
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}

	// Claim the state before touching the network. An authorization code is
	// single-use upstream, so a replayed callback must be rejected here rather
	// than failing at Microsoft and leaving the UI in the callback loop.
	s.mu.Lock()
	s.prunePKCELocked()
	p, ok := s.pkce[state]
	switch {
	case !ok || time.Since(p.Created) > pkceStateTTL:
		s.mu.Unlock()
		http.Error(w, "invalid or expired state; start a new authorization", http.StatusBadRequest)
		return
	case p.Consumed:
		s.mu.Unlock()
		http.Error(w, "this authorization was already completed; start a new authorization to add another account", http.StatusConflict)
		return
	case p.Exchanging:
		s.mu.Unlock()
		http.Error(w, "this authorization is already being processed", http.StatusConflict)
		return
	}
	p.Exchanging = true
	p.Status = "exchanging"
	s.pkce[state] = p
	verifier := p.Verifier
	attempt := p.Attempt
	s.mu.Unlock()

	exchangeCode := s.exchangePKCECode
	if exchangeCode == nil {
		exchangeCode = auth.ExchangeCode
	}
	tok, err := exchangeCode(code, verifier, auth.RedirectURI())
	if err != nil {
		if !s.failPKCEAttempt(state, attempt, err.Error()) {
			http.Error(w, pkceInactiveMessage, http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	acc, alreadyLinked, err, active := s.finishPKCEAttempt(state, attempt, tok)
	if !active {
		http.Error(w, pkceInactiveMessage, http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Browser loopback callbacks should finish in a friendly page instead of
	// displaying a raw JSON response. Keep JSON for the manual/API flow.
	if strings.HasPrefix(auth.RedirectURI(), "http://127.0.0.1:") || strings.HasPrefix(auth.RedirectURI(), "http://localhost:") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		headline, detail := "授权完成", "账号已经自动加入账号池，可以关闭此页面。"
		if alreadyLinked {
			headline = "账号已存在"
			detail = "本次登录的仍是账号池里已有的账号（浏览器沿用了上次的登录状态）。要添加新账号，请先在授权页面点“退出登录”，或使用“强制重新登录”重新授权。"
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>M365 Copilot2API 授权完成</title><style>body{font:16px system-ui;text-align:center;padding:15vh 20px;color:#242424}main{max-width:520px;margin:auto}h1{font-size:26px}</style><main><h1>%s</h1><p>%s</p><script>if(window.opener){window.opener.postMessage({type:"m365-auth-complete",duplicate:%t},window.location.origin);setTimeout(()=>window.close(),%d)}</script></main>`,
			headline, detail, alreadyLinked, map[bool]int{false: 300, true: 4000}[alreadyLinked])
		return
	}
	jsonOut(w, map[string]any{
		"status":    "authenticated",
		"duplicate": alreadyLinked,
		"account":   map[string]any{"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName, "status": acc.Status, "oid": acc.OID, "tid": acc.TID},
	})
}

func (s *Server) resolveAccount(accountID string) (auth.AccountToken, error) {
	if s == nil || s.tokens == nil {
		return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
	}
	if accountID == "" {
		accounts := s.tokens.List()
		if len(accounts) == 0 {
			return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
		}
		concurrency := s.accountConcurrency.Snapshot()
		inflight, _ := concurrency["inflight"].(map[string]int)
		// 调度器由 New() 构造（server.go:255）。这里原有一段惰性初始化，既到不了
		// （字段永不为 nil），又是在每个请求都会进入的路径上无锁写共享字段 ——
		// 两个并发请求可能各建一个调度器，其中一个的在途计数随即丢失。
		acc, ok := s.resourceScheduler.Select(accounts, s.accountAvailable, inflight)
		if !ok {
			// Select 内部已经用 accountAvailable 过滤，因此走到这里就是「没有一个
			// 可用」。原先这里直接返回普通 error，映射成 502；紧随其后那段 429 +
			// Retry-After 反而永远到不了 —— Select 成功时账号必然可用。
			//
			// 502 与 429 对客户端是两回事：Codex 会把 502 当上游故障立刻重试，把
			// 冷却期打穿；429 带 Retry-After 才会让它等。所以这里要自己判断冷却
			// 并给出重试时间。
			if s.accountPool != nil {
				for _, account := range accounts {
					if account.ID == "" || s.accountPool.Available(account.ID) {
						continue
					}
					retry := int(time.Until(s.accountPool.EarliestRecovery()).Seconds())
					if retry < 5 {
						retry = 5
					}
					return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: retry, Body: "all accounts are cooling down; try again later"}
				}
			}
			// 账号健康但被上游按邮箱限流：取最晚的解禁时间，早于它重试仍会被拒。
			if s.upstreamCooldown != nil {
				snapshot := s.upstreamCooldown.snapshot()
				retry := 0
				for _, account := range accounts {
					until, blocked := snapshot[account.Email]
					if !blocked {
						continue
					}
					if seconds := int(time.Until(until).Seconds()); seconds > retry {
						retry = seconds
					}
				}
				if retry > 0 {
					return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: retry, Body: "all accounts are temporarily unavailable; try again shortly"}
				}
			}
			return auth.AccountToken{}, fmt.Errorf("no online authorized account available; complete OAuth callback first")
		}
		accountID = acc.ID
	}
	return s.tokens.EnsureValid(accountID)
}

// nextHealthyAccount returns the next round-robin account that is still
// healthy, skipping the given id first, and validates its token. Used by the
// failover path after a rate-limited or auth-failed attempt.
func (s *Server) nextHealthyAccount(avoidID string) (auth.AccountToken, error) {
	probeLimit := len(s.tokens.List())
	for i := 0; i < probeLimit; i++ {
		acc, ok := s.tokens.Next()
		if !ok {
			return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
		}
		if avoidID != "" && acc.ID == avoidID {
			continue
		}
		if !s.accountAvailable(acc.ID) {
			continue
		}
		return s.tokens.EnsureValid(acc.ID)
	}
	return auth.AccountToken{}, fmt.Errorf("no healthy account available for failover")
}

type chatBody struct {
	AccountID      string               `json:"accountId"`
	Message        string               `json:"message"`
	Prompt         string               `json:"prompt"`
	Tone           string               `json:"tone"`
	ConversationID string               `json:"conversationId"`
	SessionID      string               `json:"sessionId"`
	SessionKey     string               `json:"sessionKey"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
	Tools          []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat   `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

func modelTone(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.2":
		return "Gpt_5_2_Chat"
	case "gpt-5.2-reasoning":
		return "Gpt_5_2_Reasoning"
	case "gpt-5.3":
		return "Gpt_5_3_Chat"
	case "gpt-5.3-reasoning":
		return "Gpt_5_3_Reasoning"
	case "gpt-5.4":
		return "Gpt_5_4_Chat"
	case "gpt-5.4-reasoning":
		return "Gpt_5_4_Reasoning"
	case "gpt-5.5":
		return "Gpt_5_5_Chat"
	case "gpt-5.5-reasoning":
		return "Gpt_5_5_Reasoning"
	case "gpt-5.6-reasoning":
		return "Gpt_5_6_Reasoning"
	case "claude", "claude-sonnet":
		return "Claude_Sonnet"
	case "claude-sonnet-reasoning":
		return "Claude_Sonnet_Reasoning"
	case "gpt-5.4-quick":
		return "Gpt_5_4_Chat"
	case "gpt-5.3-think-deeper":
		return "Gpt_5_3_Chat"
	default:
		return "magic"
	}
}

func sseRaw(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

func (s *Server) chatOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" && len(body.Attachments) == 0 {
		http.Error(w, "message or attachment required", http.StatusBadRequest)
		return
	}
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	acc, err := s.resolveAccount(body.AccountID)
	if err != nil {
		if isAccountResolveFailure(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if claimsOID, claimsTID := extractOIDTID(acc.AccessToken); claimsOID != "" {
			acc.OID = claimsOID
			acc.TID = claimsTID
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid — re-login with PKCE browser client", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:           text,
		Tone:           body.Tone,
		ConversationID: body.ConversationID,
		SessionID:      body.SessionID,
		Attachments:    body.Attachments,
	})
	if err != nil {
		// Failover: a rate-limited or auth-failed account must not take down the
		// request when the pool has other healthy accounts. Only auto-selected
		// requests fail over; an explicitly chosen account is respected, and a
		// conversation-bound chat stays on its account.
		if body.AccountID == "" && body.ConversationID == "" && (IsRateLimited(err) || IsAuthFailure(err)) {
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, chathub.Request{
					Text:           text,
					Tone:           body.Tone,
					ConversationID: body.ConversationID,
					SessionID:      body.SessionID,
					Attachments:    body.Attachments,
				})
				if err2 == nil {
					s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
					s.accountPool.MarkSuccess(next.ID)
					acc = next
					res = res2
					err = nil
				} else {
					err = err2
				}
			}
		}
		s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
		writeUpstreamError(w, err)
		return
	}
	s.accountPool.MarkSuccess(acc.ID)
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	jsonOut(w, map[string]any{
		"status":         "ok",
		"text":           res.Text,
		"conversationId": res.ConversationID,
		"sessionId":      res.SessionID,
		"requestId":      res.RequestID,
		"throttling":     res.Throttling,
		"result":         res.RawResult,
		"events":         res.Events,
		"images":         res.Images,
		"account":        map[string]any{"id": acc.ID, "email": acc.Email},
	})
}

// dropTransientConversation 异步删除 router/repair 轮创建的一次性云端对话，
// 避免每请求都往 M365 对话列表塞一条记录。删除失败不阻塞请求，留给 auto_cleanup 兜底。
func (s *Server) dropTransientConversation(conversationID string) {
	if conversationID == "" || m365CloudClient == nil {
		return
	}
	id := conversationID
	safeGo("transientConversation.delete", func() {
		if err := m365CloudClient.DeleteConversation(id); err != nil {
			log.Printf("[transient-conv] delete failed id=%s err=%v", id, err)
		}
	})
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, map[string]any{"object": "list", "data": s.catalogModels()})
}

// adminModelTest 由控制台模型测试调用，通过管理员会话鉴权，不依赖明文 API Key
// （密钥加固后 list 不再返回 raw，前端无法再自行携带 key 调用 /v1 端点）。
func (s *Server) adminModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		Model string `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Model) == "" {
		http.Error(w, "bad json: model required", http.StatusBadRequest)
		return
	}
	acc, err := s.resolveAccount("")
	if err != nil {
		if isAccountResolveFailure(err) {
			writeAccountResolveError(w, err, "account_error")
			return
		}
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "account missing oid/tid")
		return
	}
	tone, _ := s.requestedTone(b.Model, "")
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{
		Text: `Say "OK" in one word.`,
		Tone: tone,
	})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", upstreamError(err))
		return
	}
	jsonOut(w, map[string]any{"ok": true, "model": b.Model, "reply": sanitizePublicAssistantTextForModel(res.Text, b.Model), "latency_ms": ms})
}

func (s *Server) openaiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.catalogModels()
	created := time.Now().Unix()
	for _, model := range data {
		model["created"] = created
	}
	// Codex v0.144.5 requires `models`, while OpenAI-compatible clients use
	// `data`. Keep both aliases backed by the same catalog.
	jsonOut(w, map[string]any{"object": "list", "data": data, "models": data})
}

type oaiMsg struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []map[string]any `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type oaiReq struct {
	Model             string          `json:"model"`
	ResponseFormat    *responseFormat `json:"response_format,omitempty"`
	Messages          []oaiMsg        `json:"messages"`
	Stream            bool            `json:"stream"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	// optional account routing
	User           string `json:"user"`
	AccountID      string `json:"accountId"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	SessionKey     string `json:"session_key"`
	// CamelCase aliases mirroring the response metadata fields; clients echo
	// m365.conversationId / m365.sessionId back verbatim.
	ConversationIDC string               `json:"conversationId,omitempty"`
	SessionIDC      string               `json:"sessionId,omitempty"`
	Attachments     []chathub.Attachment `json:"attachments,omitempty"`
	Tools           []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func contentToString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" || t == "input_text" || t == "output_text" {
					if s, _ := m["text"].(string); s != "" {
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	case nil:
		// nil 是「没有内容」，不是内容。
		//
		// 早先它走下面的 fmt.Sprint 兜底，得到字符串 "<nil>" —— Go 的调试格式冒充
		// 成了正文。那五个字符会一路进到 prompt 里让模型当结果读（实测模型收到
		// "<nil>" 后既无法确认成功也无法确认失败，只能含糊其辞），还会虚增 token
		// 估算、污染会话相似度哈希、在对话界面上显示出来。tool_state.go 那个
		// 「有没有内容」的判断更会因此对 nil 答「有」。
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func normalizeLegacyTools(body *oaiReq) {
	if len(body.Tools) == 0 && len(body.Functions) > 0 {
		body.Tools = make([]chathub.Tool, 0, len(body.Functions))
		for _, f := range body.Functions {
			body.Tools = append(body.Tools, chathub.Tool{Type: "function", Function: f})
		}
	}
	if body.ToolChoice == nil && body.FunctionCall != nil {
		body.ToolChoice = body.FunctionCall
	}
}

func buildAnswerRequest(answerPrompt, tone string, body oaiReq, ledger agentLedger, planningMode string) chathub.Request {
	if len(ledger.Completed) > 0 || len(ledger.Pending) > 0 {
		answerPrompt += "\n" + ledger.RouterContext()
	}
	if len(ledger.Completed) > 0 {
		answerPrompt += "\nFINAL ANSWER RULE: Report only actions supported by completed tool results. If the goal is not fully verified, state exactly what remains unconfirmed."
	}
	req := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments}
	if planningMode == "native" {
		req.Tools = body.Tools
		req.ToolChoice = body.ToolChoice
	}
	// Router mode reaches this turn with Tools empty by design, but the caller did
	// declare tools -- the router simply did not select one. Saying so keeps the
	// environment prompt from offering an absent-tool escape hatch on the very
	// turn that produces the visible answer. Measured on a clean Claude CLI run:
	// with 36 tools declared, the answer turn came back "I don't have a dedicated
	// file-read tool wired up in this session".
	req.ToolsDeclared = len(body.Tools) > 0 && fmt.Sprint(body.ToolChoice) != "none"
	return req
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	startedAt := time.Now()
	beginRequest(requestID, r)
	stage(requestID, "http_start", map[string]any{"stream": r.URL.Query().Get("stream") == "true"})
	log.Printf("[req-trace] id=%s stage=http_start stream=%t", requestID, r.URL.Query().Get("stream") == "true")
	defer func() {
		stage(requestID, "http_return", map[string]any{"total_ms": time.Since(startedAt).Milliseconds()})
		endRequest(requestID, nil)
		log.Printf("[req-trace] id=%s stage=http_return total_ms=%d", requestID, time.Since(startedAt).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const maxChatRequestBody = 10 << 20
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var body oaiReq
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if reason := oversizeReason(len(body.Messages), len(raw)); reason != "" {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", reason)
		return
	}
	// API key 级策略（模型白名单 / RPM / 并发）在此统一强制执行。
	// /v1/responses 与 /v1/messages 都经由 runOpenAIAdapter 与
	// streamResponsesAdapter 委派回本函数，因此这一处即覆盖三种协议入口，
	// 且不会重复占用并发槽位。
	keyRelease, keyAllowed := s.enforceKeyPolicy(w, r, body.Model, writeOpenAIError)
	if !keyAllowed {
		return
	}
	defer keyRelease()
	responseFormat := body.ResponseFormat
	effort := body.ReasoningEffort
	if body.Reasoning != nil && strings.TrimSpace(body.Reasoning.Effort) != "" {
		effort = body.Reasoning.Effort
	}
	tone, toneErr := s.requestedTone(body.Model, effort)
	if toneErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	normalizeLegacyTools(&body)
	body.ConversationID = firstNonEmpty(body.ConversationID, body.ConversationIDC)
	body.SessionID = firstNonEmpty(body.SessionID, body.SessionIDC)
	log.Printf("[req-trace] id=%s stage=body_parsed messages=%d tools=%d choice=%s raw_bytes=%d", requestID, len(body.Messages), len(body.Tools), normalizedToolChoiceMode(body.ToolChoice), len(raw))
	stage(requestID, "body_parsed", map[string]any{"messages": len(body.Messages), "tools": len(body.Tools), "raw_bytes": len(raw)})
	// 空请求必须在注入之前判定。下面注入的环境说明是网关自己添的内容，一旦先
	// 注入，扁平化后的 prompt 就永远非空，messages:[] 这类请求会绕过后面那道
	// 400 直接打到上游。这里用同一个扁平化函数和同一句错误文案，判定标准与注入
	// 前保持一致。
	if callerPrompt, _ := flattenPromptMessages(body.Messages, nil); strings.TrimSpace(callerPrompt) == "" {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}
	body.Messages = ensureRuntimeWorkspaceInstruction(body.Messages)
	if cleaned, notes := sanitizeToolConversation(body.Messages); len(notes) > 0 {
		body.Messages = cleaned
		log.Printf("[req-trace] id=%s stage=tool_history_sanitized drops=%d detail=%v", requestID, len(notes), notes)
		stage(requestID, "tool_history_sanitized", map[string]any{"drops": len(notes), "detail": notes})
	}
	if err := validateToolConversation(body.Messages); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	trimmedMessages, trimErr := trimMessagesToContext(body.Messages, body.Tools, body.ToolChoice, body.Model)
	if trimErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "context_length_exceeded", trimErr.Error())
		return
	}
	if len(trimmedMessages) != len(body.Messages) {
		log.Printf("[req-trace] id=%s stage=context_trim messages=%d->%d budget=%d", requestID, len(body.Messages), len(trimmedMessages), configuredContextBudget())
		stage(requestID, "context_trim", map[string]any{"messages_before": len(body.Messages), "messages_after": len(trimmedMessages), "budget": configuredContextBudget()})
		body.Messages = trimmedMessages
	}
	// Rebuild a protocol-neutral evidence ledger from actual tool calls/results.
	// Round limits apply only to the current user turn; full history still informs evidence.
	ledger := buildAgentLedger(body.Messages)
	activeLedger := buildAgentLedger(activeMessages(body.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "tool_round_limit", "message": err.Error(), "completed_calls": len(activeLedger.Completed)}})
		return
	}
	// Preserve role boundaries when adapting OpenAI messages to ChatHub's
	// single message.text field. This keeps system/developer instructions,
	// history, and the current user turn distinguishable.
	var prompt string
	prompt, body.Attachments = flattenPromptMessages(body.Messages, body.Attachments)
	log.Printf("[req-trace] id=%s stage=prompt_flattened prompt_len=%d attachments=%d", requestID, len(prompt), len(body.Attachments))
	fmt.Printf("[multimodal-entry] messages=%d attachments=%d prompt_len=%d\n", len(body.Messages), len(body.Attachments), len(prompt))
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}
	if answer, ok := publicIdentityAnswer(body.Messages, body.Model); ok && responseFormat == nil {
		s.writePublicIdentityChatResponse(w, r, &body, prompt, answer, startedAt)
		return
	}

	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	if body.User != "" && body.ConversationID == "" {
		if us, ok := s.userSessions.Get(body.User); ok {
			body.AccountID = firstNonEmpty(body.AccountID, us.AccountID)
			body.ConversationID = us.ConversationID
			body.SessionID = us.SessionID
			log.Printf("[user-session] hit user=%s conversation=%s session=%s", body.User, us.ConversationID, us.SessionID)
		}
	}
	// 内容键会话复用：命中后云端对话已存全量历史，只需把客户端新增的
	// 消息拼成增量 prompt 发送（对齐 DeepSeek 上下文缓存语义）。
	answerPrompt := prompt
	resolvedConversationID := ""
	// Incremental prompt: only send messages beyond what the cloud conversation
	// already has. Two paths reach this:
	//   - body.ConversationID was empty → content-key resolver finds the match;
	//   - body.ConversationID was set by SessionKey/User above → look up that
	//     conversation's history directly. Without this second path, every turn
	//     on a long conversation re-flattens and re-sends the entire history.
	historyLen := 0
	if body.ConversationID != "" {
		for _, sess := range s.sessionResolver.ListSessions() {
			if sess.ConversationID == body.ConversationID && sess.SessionID != "" {
				historyLen = len(sess.ContextHistory)
				break
			}
		}
	}
	if body.ConversationID == "" && len(body.Messages) > 0 {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			resolvedConversationID = resolved.ConversationID
			body.ConversationID = resolved.ConversationID
			body.SessionID = resolved.SessionID
			body.AccountID = firstNonEmpty(body.AccountID, resolved.AccountID)
			log.Printf("[session-resolver] matched=%s conversation=%s history=%d total=%d", resolved.MatchedBy, resolved.ConversationID, resolved.HistoryLen, len(body.Messages))
			historyLen = resolved.HistoryLen
		}
	}
	if historyLen > 0 && historyLen < len(body.Messages) {
		// Only the increment goes upstream. The full prompt above is still built
		// because the router, identity answer, tool-intent heuristics and token
		// accounting read it, but it must not be what we send: re-sending a
		// 148-message history every turn is what saturated the CPU.
		incPrompt, incAtt := flattenPromptMessages(body.Messages[historyLen:], nil)
		incPrompt = strings.TrimSpace(incPrompt)
		if incPrompt != "" {
			// Bind stored the injected runtime system at index 0, so HistoryLen
			// walks past it. Re-attach only that marker block — not the caller's
			// full harness — or mid-conversation identity drops to the cloud sandbox.
			answerPrompt = attachRuntimeIdentityToIncrement(incPrompt)
			body.Attachments = incAtt
			log.Printf("[session-resolver] incremental prompt_len=%d (full was %d)", len(answerPrompt), len(prompt))
		}
	}
	accountID := body.AccountID
	acc, err := s.resolveAccount(accountID)
	if err != nil {
		log.Printf("[account-route] resolve failed requested=%q err=%v", accountID, err)
		if isAccountResolveFailure(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeUpstreamError(w, err)
		return
	}
	log.Printf("[account-route] selected id=%q email=%q token_present=%t oid_present=%t tid_present=%t", acc.ID, acc.Email, acc.AccessToken != "", acc.OID != "", acc.TID != "")
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	// Normalize tools once. Selection is always made by the upstream model;
	// the gateway only validates its structured decision and converts protocols.
	toolMaps := make([]map[string]any, 0, len(body.Tools))
	for _, tool := range body.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	if body.ToolChoice == nil && len(toolMaps) > 0 {
		body.ToolChoice = "auto"
	}
	validateCalls := func(stage string, calls []detectedToolCall) ([]detectedToolCall, int) {
		valid, rejected := validateDetectedToolCalls(calls, toolMaps, body.ToolChoice)
		// 客户端显式关闭并行时降为单调用。这里是 11 处决策解析的共同收口，放在
		// 别处会漏掉分支。
		valid, serialized := enforceParallelToolCalls(valid, body.ParallelToolCalls)
		rejected = append(rejected, serialized...)
		for _, call := range rejected {
			log.Printf("[tool-validation] id=%s stage=%s rejected_name=%q reason=%q", requestID, stage, call.Name, call.Reason)
		}
		return valid, len(rejected)
	}
	planningMode := s.settings.get().ToolPlanningMode
	var calls []detectedToolCall
	var parsed bool
	// routerIntent is the raw heuristic: did the user ask for a concrete action?
	// routerRetry is that answer minus the turns already answerable from tool
	// evidence, and is what gates the constrained retry. Both are declared here
	// because the streaming intent-retry gate sits outside the router block.
	routerIntent := toolIntentLikely(latestUserIntent(body.Messages, prompt), toolMaps)
	routerRetry := routerIntent && !ledgerAnswersIntent(latestUserIntent(body.Messages, prompt), ledger)

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
	// The stream is opened by the actual response path below. Do not emit a
	// tool preamble here: a request may contain tools in its schema while still
	// being an ordinary text question.
	// Streaming requests must not wait for the synchronous tool router. This
	// path forwards ordinary upstream text deltas immediately; tool routing for
	// non-streaming requests remains below until the event-level tool protocol
	// is available end-to-end.
	if planningMode == "router" && body.Stream && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routerOutcome := newRouterOutcome(requestID, "stream-router", len(toolMaps), body.ToolChoice, routerIntent)
		// Preserve the existing validated tool router for streaming tool turns.
		// Only fall through to text streaming when the router explicitly selects
		// no tool; this prevents a natural-language preamble from becoming a
		// completed assistant turn with the actual call lost.
		routePrompt := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		log.Printf("[req-trace] id=%s stage=router_start prompt_len=%d", requestID, len(routePrompt))
		routeRes, routedAccount, routeErr := s.routerChatWithFailover(ctx, "stream-router", acc, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
		if routedAccount.ID != "" {
			acc = routedAccount
			account = chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
		}
		recordRouterFrames(routerFrameInput{RequestID: requestID, Stage: "stream-router", Prompt: routePrompt, Text: routeRes.Text, Reasoning: routeRes.Reasoning, Events: routeRes.Events, Err: routeErr})
		log.Printf("[req-trace] id=%s stage=router_return elapsed_ms=%d err=%t", requestID, time.Since(startedAt).Milliseconds(), routeErr != nil)
		// Router turns run in a throwaway cloud conversation that is never
		// reused by the answer turn; delete it so the conversation list does
		// not accumulate one entry per routed request.
		if routeErr == nil && routeRes.ConversationID != "" {
			s.dropTransientConversation(routeRes.ConversationID)
		}
		if routeErr != nil {
			routerOutcome.record("initial", "router_error")
			switch classifyRouterFailure(ctx, routeErr, body.ToolChoice) {
			case routerFailureAbandon:
				return
			case routerFailureFatal:
				writeRouterFatal(w, "router", routeErr)
				return
			case routerFailureAnswer:
				log.Printf("[req-trace] id=%s stage=router_degraded reason=answer_fallback", requestID)
			}
		}
		calls, parsed = parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		routerOutcome.observeParsed(parsed, len(calls))
		calls = filterCompletedCalls(calls, ledger)
		postLedger := len(calls)
		calls, rejected := validateCalls("router", calls)
		routerOutcome.observeValidated(postLedger, len(calls), rejected)
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
			if repairErr == nil && repairRes.ConversationID != "" {
				s.dropTransientConversation(repairRes.ConversationID)
			}
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				routerOutcome.observeParsed(parsed, len(calls))
				calls = filterCompletedCalls(calls, ledger)
				postLedger = len(calls)
				calls, rejected = validateCalls("router", calls)
				routerOutcome.observeValidated(postLedger, len(calls), rejected)
			}
		}
		if parsed && len(calls) > 0 {
			routerOutcome.record("validated", "emitted_tool_calls")
			scope := fmt.Sprintf("%d:%v:stream", len(body.Messages), completedCallIDs(ledger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), true, calls, routeRes)
			// 工具轮提前返回：本地轻量登记会话（空 ConversationID），不把
			// 一次性 router 对话写入 sessionResolver。routeRes 的云端对话
			// 已由上方的 dropTransientConversation 删除。
			bindRes := routeRes
			bindRes.ConversationID = ""
			bindRes.SessionID = ""
			s.bindConversation(acc, &body, r, bindRes, answerPrompt, startedAt)
			return
		}
		if normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerRetry {
			routerOutcome.record("validated", "intent_retry")
		} else if normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerIntent {
			routerOutcome.record("validated", "intent_answered_from_ledger")
		} else if !toolChoiceRequiresToolCall(body.ToolChoice) {
			routerOutcome.record("validated", "ordinary_answer_fallback")
		}
	}
	if body.Stream {
		if parsed && len(calls) == 0 && normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerRetry {
			retryPrompt := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, "required") + "\nINTENT RETRY: Select at least one declared tool for this concrete action request. Do not answer with prose or NO_TOOL_NEEDED."
			retryRes, retryErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: retryPrompt, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
			recordRouterFrames(routerFrameInput{RequestID: requestID, Stage: "stream-router-intent-retry", Prompt: retryPrompt, Text: retryRes.Text, Reasoning: retryRes.Reasoning, Events: retryRes.Events, Err: retryErr})
			if retryErr == nil && retryRes.ConversationID != "" {
				s.dropTransientConversation(retryRes.ConversationID)
			}
			if retryErr == nil {
				retryCalls, retryParsed := parseModelToolDecision(retryRes.Text, toolMaps, "required")
				retryCalls = filterCompletedCalls(retryCalls, ledger)
				retryCalls, _ = validateCalls("stream-router-intent-retry", retryCalls)
				if retryParsed && len(retryCalls) > 0 {
					scope := fmt.Sprintf("%d:%v:stream-intent-retry", len(body.Messages), completedCallIDs(ledger))
					for i := range retryCalls {
						retryCalls[i].ID = scopedCallID(retryCalls[i].Name, string(retryCalls[i].Arguments), i, scope)
					}
					retryCalls = limitToolCalls(retryCalls, adaptiveToolCallLimit(retryCalls, configuredToolCallLimit(s.settings)))
					_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), true, retryCalls, retryRes)
					// 工具轮提前返回：本地轻量登记会话（空 ConversationID），
					// 不把一次性 router 对话写入 sessionResolver。retryRes
					// 的云端对话已由上方的 dropTransientConversation 删除。
					bindRes := retryRes
					bindRes.ConversationID = ""
					bindRes.SessionID = ""
					s.bindConversation(acc, &body, r, bindRes, answerPrompt, startedAt)
					return
				}
			}
		}
		answerReq := buildAnswerRequest(answerPrompt, tone, body, ledger, planningMode)
		answerPrompt = answerReq.Text
		log.Printf("[req-trace] id=%s stage=answer_start prompt_len=%d native_tools=%d", requestID, len(answerPrompt), len(answerReq.Tools))
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		if err := sseRaw(r.Context(), w, flusher, ": connected\n\n"); err != nil {
			return
		}
		var text strings.Builder
		var pending strings.Builder
		var streamedTools []detectedToolCall
		progress := newStreamProgress(startedAt)
		first := true
		identityFilter := newPublicIdentityStreamFilter(model)
		emitText := func(part string) error {
			if part == "" {
				return nil
			}
			part = identityFilter.Push(part)
			if part == "" {
				return nil
			}
			progress.addText(part)
			if err := r.Context().Err(); err != nil {
				return err
			}
			delta := map[string]any{"content": part}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", mustJSON(chunk)); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}
		// When the client declared tools, prose cannot be flushed as it arrives:
		// see deferredStreamSink. Tool-bearing turns accumulate and are released
		// once the finished answer has been classified, at the cost of
		// time-to-first-token on those turns; the Anthropic path always paid it.
		sink, deferred, deferOutput := deferredStreamSink(toolMaps, emitText)
		handleStreamText := func(fragment string) error {
			text.WriteString(fragment)
			return streamTextWithToolLookahead(&pending, fragment, toolMaps, body.ToolChoice, sink)
		}
		streamEvent := func(ev chathub.StreamEvent) error {
			if ev.Kind == "tool" && ev.ToolName != "" && len(ev.Arguments) > 0 {
				streamedTools = append(streamedTools, detectedToolCall{ID: "call_" + uuid.NewString(), Name: ev.ToolName, Arguments: ev.Arguments})
				return nil
			}
			if ev.Kind != "text" || ev.Text == "" {
				return nil
			}
			return handleStreamText(ev.Text)
		}
		res, usedAccount, err := s.streamChatWithRecovery(ctx, acc, answerReq, streamEvent)
		if usedAccount.ID != "" {
			acc = usedAccount
			account = chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
		}
		if err != nil {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			msg := upstreamError(err)
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
			}
			msg = sanitizePublicInternalText(msg)
			// A deferred turn has delivered nothing yet. Release whatever did
			// arrive before reporting the break, or the partial answer the live
			// path would have shown is lost outright.
			if deferOutput && deferred.Len() > 0 {
				_ = emitText(deferred.String())
			}
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(streamTruncatedChunk(id, model, progress))+"\n\n")
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": "rate_limit"}})+"\n\n")
			_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
			return
		}
		s.accountPool.MarkSuccess(acc.ID)
		if text.Len() == 0 && strings.TrimSpace(res.Text) != "" {
			text.WriteString(res.Text)
			pending.WriteString(res.Text)
		}
		rawCalls := streamedTools
		if len(rawCalls) == 0 {
			rawCalls = fencedToolCalls(text.String(), toolMaps, body.ToolChoice)
		}
		calls, rejected := validateCalls("stream", rawCalls)
		toolResult := chathub.Result{Text: text.String()}
		if len(calls) == 0 && rejected > 0 {
			// A native ChatHub event can contain a fabricated or empty tool name.
			// Do not leak it to the local runner: ask the model to remap the intent
			// to exactly one of the tools the client actually declared.
			repairPrompt := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, "required") +
				"\nREPAIR RULE: The previous upstream event selected an undeclared tool. Select one declared tool that performs the intended operation. Never return unknown_tool."
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: repairPrompt, Tone: tone, Attachments: body.Attachments})
			if repairErr == nil {
				repaired, parsed := parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				if parsed {
					calls, _ = validateCalls("stream-repair", repaired)
					if len(calls) > 0 {
						toolResult = repairRes
					}
				}
			}
			if len(calls) == 0 {
				log.Printf("[tool-validation] id=%s stage=stream-repair failed", requestID)
				_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream selected an undeclared tool and repair failed", "code": "invalid_tool_call"}})+"\n\n")
				_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
				return
			}
		}
		if len(calls) > 0 {
			log.Printf("[req-trace] id=%s stage=tool_calls_detected count=%d names=%v", requestID, len(calls), func() []string {
				var n []string
				for _, c := range calls {
					n = append(n, c.Name)
				}
				return n
			}())
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponse(w, id, model, true, calls, toolResult)
			if body.User != "" && res.ConversationID != "" {
				s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
			}
			s.bindConversation(acc, &body, r, res, answerPrompt, startedAt)
			return
		}
		if err := flushStreamText(&pending, toolMaps, body.ToolChoice, true, sink); err != nil {
			log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
			return
		}
		if deferOutput {
			// The answer is complete and carried no tool call. Classify it here,
			// while nothing has been flushed: a denial can still be replaced by a
			// corrected answer, and the corrected answer may itself be the tool
			// call this turn was supposed to produce.
			if corrected, ok := s.correctSandboxDrift(ctx, ejectRequest{
				AccountID:      acc.ID,
				Account:        account,
				Tools:          body.Tools,
				ToolChoice:     body.ToolChoice,
				Text:           text.String(),
				UserRequest:    answerPrompt,
				Tone:           tone,
				Attachments:    body.Attachments,
				ConversationID: firstNonEmpty(body.ConversationID, res.ConversationID),
				SessionID:      firstNonEmpty(body.SessionID, res.SessionID),
			}); ok {
				res = corrected
				ejected := fencedToolCalls(corrected.Text, toolMaps, body.ToolChoice)
				if len(ejected) == 0 {
					ejected = nativeToolCalls(corrected.Events, body.Tools)
				}
				ejectedCalls, ejectedRejected := validateCalls("stream-eject", ejected)
				if len(ejectedCalls) > 0 {
					ejectedCalls = limitToolCalls(ejectedCalls, adaptiveToolCallLimit(ejectedCalls, configuredToolCallLimit(s.settings)))
					_ = writeToolResponse(w, id, model, true, ejectedCalls, corrected)
					if body.User != "" && res.ConversationID != "" {
						s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
					}
					s.bindConversation(acc, &body, r, res, answerPrompt, startedAt)
					return
				}
				// 扣住的那句拒绝作废，换成纠正轮的正文。ejectDelivery 保证
				// corrected.Text 非空时发出去的东西也非空 —— 发了调用但没过 schema
				// 的那种情况不能再走围栏剥离，否则整段被剥空，客户端拿到一个空轮。
				deferred.Reset()
				pending.Reset()
				if ejectedRejected > 0 {
					log.Printf("[req-trace] id=%s stage=stream_eject_rejected rejected=%d keeping_text=1", requestID, ejectedRejected)
				}
				if err := sink(ejectDelivery(corrected.Text, ejectedRejected, toolMaps, body.ToolChoice)); err != nil {
					log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
					return
				}
			}
			if err := emitText(deferred.String()); err != nil {
				log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
				return
			}
		}
		finishChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
		_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(finishChunk)+"\n\n")
		_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		if body.User != "" && res.ConversationID != "" {
			s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
		}
		s.bindConversation(acc, &body, r, res, answerPrompt, startedAt)
		return
	}
	// Ask the upstream model to select and validate the next tool. The gateway
	// remains tool-agnostic; it only validates and serializes the decision.
	if planningMode == "router" && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routerOutcome := newRouterOutcome(requestID, "router", len(toolMaps), body.ToolChoice, routerIntent)
		routePrompt := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		// routerChatWithFailover already retries a connect-stage transport
		// failure on a different healthy account and a different outbound exit.
		// A rate-limited or auth-failed attempt is swapped the same way, so the
		// previous single-shot 429/401 failover is folded into it.
		routeRes, routedAccount, routeErr := s.routerChatWithFailover(ctx, "router", acc, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
		if routedAccount.ID != "" {
			acc = routedAccount
			account = chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
		}
		// 失败帧必须先记录再返回：用户开了「捕获路由原始帧」复现失败后，
		// 诊断里必须留下证据 —— 恰恰是最需要证据的场景不能丢证据。
		// 账号健康度由 routerChatWithFailover 内部按尝试逐次记账。
		recordRouterFrames(routerFrameInput{RequestID: requestID, Stage: "router", Prompt: routePrompt, Text: routeRes.Text, Reasoning: routeRes.Reasoning, Events: routeRes.Events, Err: routeErr})
		if routeErr != nil {
			routerOutcome.record("initial", "router_error")
			switch classifyRouterFailure(ctx, routeErr, body.ToolChoice) {
			case routerFailureAbandon:
				return
			case routerFailureFatal:
				writeRouterFatal(w, "router", routeErr)
				return
			case routerFailureAnswer:
				log.Printf("[req-trace] id=%s stage=router_degraded reason=answer_fallback", requestID)
			}
		}
		calls, parsed = parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		routerOutcome.observeParsed(parsed, len(calls))
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Do not invent calls; use {"calls":[]} if unrecoverable. OUTPUT:
` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				routerOutcome.observeParsed(parsed, len(calls))
			}
			if !parsed {
				// 路由器没能给出可解析的决策，这不等于请求失败。
				// 绝大多数情况是模型直接用自然语言回答了问题（"I will read
				// the file for you." / "抱歉，我无法……"），此时唯一正确的动作
				// 是当作「本轮不调用工具」继续走下面的回答链路。
				//
				// 之前这里直接 502，导致 Claude CLI 侧看到
				// "model returned an invalid tool routing decision"：
				// 一次散文回复就把整条对话打死。仅当客户端明确要求必须调用
				// 工具时，无法给出决策才是真正的失败。
				if toolChoiceRequiresToolCall(body.ToolChoice) {
					routerOutcome.record("repair", "parser_failure")
					log.Printf("[req-trace] id=%s stage=router_undecidable choice=required", requestID)
					writeOpenAIError(w, http.StatusBadGateway, "router_error", "model did not return a parsable tool decision while tool_choice required a call")
					return
				}
				log.Printf("[req-trace] id=%s stage=router_undecidable fallback=answer text_len=%d", requestID, len(routeRes.Text))
				routerOutcome.record("repair", "parser_failure")
				calls, parsed = nil, true
				routerOutcome.observeParsed(true, 0)
			}
		}
		calls = filterCompletedCalls(calls, ledger)
		postLedger := len(calls)
		calls, rejected := validateCalls("router", calls)
		routerOutcome.observeValidated(postLedger, len(calls), rejected)
		if len(calls) > 0 {
			routerOutcome.record("validated", "emitted_tool_calls")
			scope := fmt.Sprintf("%d:%v", len(body.Messages), completedCallIDs(ledger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, calls, routeRes)
			// 非流式工具轮提前返回：本地轻量登记会话（空 ConversationID），
			// 不把一次性 router 对话写入 sessionResolver。routeRes 的云端
			// 对话是 router 合成 prompt 走的一次性对话，这里对称地调用
			// dropTransientConversation 删除它，避免每个非流式工具轮在云端
			// 遗留一条一次性对话（流式路径在 :1738-1740 已做同样处理）。
			if routeRes.ConversationID != "" {
				s.dropTransientConversation(routeRes.ConversationID)
			}
			bindRes := routeRes
			bindRes.ConversationID = ""
			bindRes.SessionID = ""
			s.bindConversation(acc, &body, r, bindRes, prompt, startedAt)
			return
		}
		if len(calls) == 0 && normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerRetry {
			routerOutcome.record("validated", "intent_retry")
			// A concrete action request deserves one constrained retry even in auto
			// mode. This is the narrow repair path that avoids forcing tools for
			// ordinary informational questions.
			retryText := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, "required") + "\nINTENT RETRY: Select at least one declared tool for this concrete action request. Do not answer with prose or NO_TOOL_NEEDED."
			retryRes, retryErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: retryText, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
			recordRouterFrames(routerFrameInput{RequestID: requestID, Stage: "router-intent-retry", Prompt: retryText, Text: retryRes.Text, Reasoning: retryRes.Reasoning, Events: retryRes.Events, Err: retryErr})
			if retryErr == nil && retryRes.ConversationID != "" {
				s.dropTransientConversation(retryRes.ConversationID)
			}
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, toolMaps, "required")
				routerOutcome.observeParsed(parsed, len(calls))
				calls = filterCompletedCalls(calls, ledger)
				postLedger := len(calls)
				calls, rejected := validateCalls("router-intent-retry", calls)
				routerOutcome.observeValidated(postLedger, len(calls), rejected)
				if parsed && len(calls) > 0 {
					routerOutcome.record("intent_retry", "emitted_tool_calls")
					scope := fmt.Sprintf("%d:%v:intent-retry", len(body.Messages), completedCallIDs(ledger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
					_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, calls, retryRes)
					// 非流式 intent-retry 工具轮提前返回：本地轻量登记
					// 会话（空 ConversationID），不把一次性 router 对话写入
					// sessionResolver。retryRes 的云端对话已由上方的
					// dropTransientConversation 删除。
					bindRes := retryRes
					bindRes.ConversationID = ""
					bindRes.SessionID = ""
					s.bindConversation(acc, &body, r, bindRes, prompt, startedAt)
					return
				}
			}
		}
		if normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerRetry {
			routerOutcome.record("intent_retry", "retry_exhausted")
		} else if normalizedToolChoiceMode(body.ToolChoice) == "auto" && routerIntent {
			routerOutcome.record("validated", "intent_answered_from_ledger")
		} else if !toolChoiceRequiresToolCall(body.ToolChoice) {
			routerOutcome.record("validated", "ordinary_answer_fallback")
		}
		if fmt.Sprint(body.ToolChoice) == "required" {
			defs, _ := json.Marshal(toolMaps)
			retryText := `Select at least one required next tool call from FUNCTION_DEFINITIONS. Validate every argument against its schema. Return JSON only as {"calls":[{"name":"function_name","arguments":{}}]}.
APPLICATION_REQUEST_AND_EVIDENCE:
` + prompt + "\n" + ledger.RouterContext() + "\nFUNCTION_DEFINITIONS:\n" + string(defs)
			retryRes, retryErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: retryText, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, toolMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, ledger)
				calls, _ = validateCalls("router", calls)
				if parsed && len(calls) > 0 {
					scope := fmt.Sprintf("%d:%v:required-retry", len(body.Messages), completedCallIDs(ledger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
					_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, calls, retryRes)
					// 非流式 required-retry 工具轮提前返回：本地轻量登记
					// 会话（空 ConversationID），不把一次性 router 对话写入
					// sessionResolver。required-retry 路径上方没有对称的
					// dropTransientConversation 调用，这里补上，避免云端
					// 遗留一次性对话。
					if retryRes.ConversationID != "" {
						s.dropTransientConversation(retryRes.ConversationID)
					}
					bindRes := retryRes
					bindRes.ConversationID = ""
					bindRes.SessionID = ""
					s.bindConversation(acc, &body, r, bindRes, prompt, startedAt)
					return
				}
			}
			// 强制模式下重试仍未选出工具：过去直接 502。但此时模型通常已经
			// 给出了可用的文字答案，直接失败会把整轮对话丢掉。降级为普通回答，
			// 让客户端自行决定是否重试，比返回 502 更接近 OpenAI 的语义。
			log.Printf("[req-trace] id=%s stage=router_required_exhausted fallback=answer", requestID)
			routerOutcome.record("required_retry", "retry_exhausted")
		}
	}
	answerReq := buildAnswerRequest(answerPrompt, tone, body, ledger, planningMode)
	answerPrompt = answerReq.Text
	var res chathub.Result
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		firstDelta := true
		progress := newStreamProgress(startedAt)
		writeChunk := func(delta map[string]any) error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			// The first SSE chunk must carry the assistant role; subsequent
			// chunks carry content or reasoning deltas.
			if firstDelta {
				firstDelta = false
				withRole := map[string]any{"role": "assistant", "content": nil}
				for k, v := range delta {
					withRole[k] = v
				}
				delta = withRole
			}
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": delta}}}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", mustJSON(chunk)); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}
		contentFilter := newPublicIdentityStreamFilter(firstNonEmpty(body.Model, defaultPublicModelName))
		reasoningFilter := newPublicReasoningStreamFilter()
		onDelta := func(content string) error {
			content = contentFilter.Push(content)
			if content != "" {
				progress.addText(content)
				return writeChunk(map[string]any{"content": content})
			}
			return nil
		}
		onReasoning := func(reasoning string) error {
			reasoning = reasoningFilter.Push(reasoning)
			if reasoning != "" {
				progress.addReasoning(reasoning)
				return writeChunk(map[string]any{"reasoning_content": reasoning})
			}
			return nil
		}
		if err := sseRaw(r.Context(), w, flusher, ": connected\n\n"); err != nil {
			return
		}
		res, err = s.chatWithAccountReasoning(ctx, acc.ID, account, answerReq, onDelta, onReasoning)
		if err != nil && body.AccountID == "" && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err)) {
			// Retry a throttled stream on the next healthy account; the client
			// has only seen the ": connected" preamble so far, so the retry is
			// indistinguishable from a fresh request.
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				failoverReq := answerReq
				if body.ConversationID == resolvedConversationID {
					failoverReq.ConversationID = ""
					failoverReq.SessionID = ""
				}
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				if res2, err2 := s.chatWithAccountReasoning(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq, onDelta, onReasoning); err2 == nil {
					res = res2
					acc = next
					err = nil
				} else {
					err = err2
					s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown)
				}
			}
		}
		if err == nil {
			if content := contentFilter.Flush(); content != "" {
				if writeErr := writeChunk(map[string]any{"content": content}); writeErr != nil {
					return
				}
			}
			if reasoning := reasoningFilter.Flush(); reasoning != "" {
				if writeErr := writeChunk(map[string]any{"reasoning_content": reasoning}); writeErr != nil {
					return
				}
			}
			res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
			res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
			s.accountPool.MarkSuccess(acc.ID)
		} else {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			msg := upstreamError(err)
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
			}
			msg = sanitizePublicInternalText(msg)
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(streamTruncatedChunk(id, model, progress))+"\n\n")
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": "rate_limit"}})+"\n\n")
		}
		pt := EstimateTokens(prompt)
		ct := EstimateTokens(res.Text)
		log.Printf("[usage] stream id=%s pt=%d ct=%d res.Text=%d", id, pt, ct, len(res.Text))
		if err == nil && ct == 0 {
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream returned empty completion; the requested model may be unavailable for this tenant", "code": "upstream_error"}})+"\n\n")
		}
		if err != nil {
			_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		} else {
			usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
			_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
			_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		}
	} else {
		res, err = s.chatWithAccount(ctx, acc.ID, account, answerReq)
		if IsEmptyCompletion(err) && tone != "magic" {
			log.Printf("[tone-fallback] tone=%q returned empty, retrying with magic", tone)
			magicReq := answerReq
			magicReq.Tone = "magic"
			if res2, err2 := s.chatWithAccount(ctx, acc.ID, account, magicReq); err2 == nil && res2.Text != "" {
				res = res2
				err = nil
			}
		}
		if err != nil && body.AccountID == "" && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err)) {
			// Failover only when nothing pins the request to a conversation or
			// account; a fresh chat can safely retry on the next healthy account.
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				failoverReq := answerReq
				if body.ConversationID == resolvedConversationID {
					failoverReq.ConversationID = ""
					failoverReq.SessionID = ""
				}
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq)
				if err2 == nil {
					res = res2
					acc = next
					err = nil
					s.accountPool.MarkSuccess(next.ID)
				} else {
					err = err2
				}
			}
		}
	}
	if err != nil {
		s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
		writeUpstreamError(w, err)
		return
	}
	s.accountPool.MarkSuccess(acc.ID)
	if body.Stream {
		if body.User != "" && res.ConversationID != "" {
			s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
		}
		s.bindConversation(acc, &body, r, res, prompt, startedAt)
		return
	}

	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: prompt})
	}
	if body.User != "" && res.ConversationID != "" {
		s.userSessions.Put(body.User, res.ConversationID, res.SessionID, acc.ID)
		log.Printf("[user-session] put user=%s conversation=%s session=%s", body.User, res.ConversationID, res.SessionID)
	}
	if res.ConversationID != "" {
		s.bindConversation(acc, &body, r, res, prompt, startedAt)
	}
	if res.ConversationID != "" {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			w.Header().Set(sessionHeaderName, resolved.SessionID)
		}
	}
	model := body.Model
	if model == "" {
		model = "m365-copilot"
	}
	id := "chatcmpl-" + uuid.NewString()
	// Both drift corrections live in sandbox_eject.go so the streaming path can
	// run the identical classification before it releases any output. What is
	// re-asked here is answerPrompt, not the full flattened history: on a session
	// hit answerPrompt is the increment that actually went upstream, and it is
	// the copy that carries the re-attached runtime identity block.
	if corrected, ok := s.correctSandboxDrift(ctx, ejectRequest{
		AccountID:   acc.ID,
		Account:     account,
		Tools:       body.Tools,
		ToolChoice:  body.ToolChoice,
		Text:        res.Text,
		UserRequest: answerPrompt,
		Tone:        tone,
		Attachments: body.Attachments,
		// The body carries no IDs on a first turn; the result always does.
		// Falling back to the answer's own IDs keeps the correction inside the
		// conversation that holds the request instead of opening a blank one.
		ConversationID: firstNonEmpty(body.ConversationID, res.ConversationID),
		SessionID:      firstNonEmpty(body.SessionID, res.SessionID),
	}); ok {
		res = corrected
	}
	invalidDetectedTool := false
	if rawCalls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice); len(rawCalls) > 0 {
		calls, rejected := validateCalls("fenced", rawCalls)
		invalidDetectedTool = rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponse(w, id, model, body.Stream, calls, res)
			return
		}
	}
	if rawCalls := nativeToolCalls(res.Events, body.Tools); len(rawCalls) > 0 {
		calls, rejected := validateCalls("native", rawCalls)
		invalidDetectedTool = invalidDetectedTool || rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponse(w, id, model, body.Stream, calls, res)
			return
		}
	}
	// Recover natural-language tool intent in native mode, and repair any
	// structured event that failed the declared-name/schema boundary.
	if (planningMode == "native" || invalidDetectedTool) && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routePrompt := modelToolRouterPrompt(routerPromptMessages(body.Messages)+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		routeRes, routeErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, ToolsDeclared: true, SchemasInText: true})
		recordRouterFrames(routerFrameInput{RequestID: requestID, Stage: "native-recovery", Prompt: routePrompt, Text: routeRes.Text, Reasoning: routeRes.Reasoning, Events: routeRes.Events, Err: routeErr})
		if routeErr == nil {
			calls, parsed = parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
			if !parsed {
				repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
				if repairErr == nil {
					calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				}
			}
			calls, _ = validateCalls("native-recovery", calls)
			if parsed && len(calls) > 0 {
				scope := fmt.Sprintf("%d:%v:native-recovery", len(body.Messages), completedCallIDs(ledger))
				for i := range calls {
					calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
				}
				calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
				_ = writeToolResponse(w, id, model, body.Stream, calls, routeRes)
				return
			}
		}
	}
	if !completionEvidenceAllows(res.Text, ledger) {
		res.Text = "I cannot confirm completion because no matching tool results were returned. No external action has been verified."
	}
	res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	log.Printf("[debug] res.Text bytes=%d content=%q", len(res.Text), res.Text)
	created := time.Now().Unix()

	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		// one-shot "stream" — emit full content then done
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": res.Text},
			}},
		}
		b, _ := json.Marshal(chunk)
		_ = sseRaw(r.Context(), w, flusher, "data: "+string(b)+"\n\n")
		pt := EstimateTokens(prompt)
		ct := EstimateTokens(res.Text)
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
		_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
		_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		return
	}

	if responseFormat != nil && (responseFormat.Type == "json_object" || responseFormat.Type == "json_schema") {
		res.Text = normalizeJSONText(res.Text)
	}
	content := any(res.Text)
	if len(res.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": res.Text}}
		for _, u := range res.Images {
			du, _ := downloadImageAsDataURIWithToken(u, acc.AccessToken)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": du}})
		}
		content = parts
	}
	assistant := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if res.Reasoning != "" {
		assistant["reasoning_content"] = res.Reasoning
	}
	// 上游 ChatHub 不返回 token 计数，按请求/回复文本本地估算填充
	// OpenAI 要求的 usage 字段。
	pt := EstimateTokens(prompt)
	ct := EstimateTokens(res.Text)
	jsonOut(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       assistant,
			"finish_reason": "stop",
		}},
		"m365": compatM365Metadata(res),
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      pt + ct,
		},
	})
}

func (s *Server) writePublicIdentityChatResponse(w http.ResponseWriter, r *http.Request, body *oaiReq, prompt, answer string, startedAt time.Time) {
	model := firstNonEmpty(body.Model, defaultPublicModelName)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	inputTokens := EstimateTokens(prompt)
	outputTokens := EstimateTokens(answer)
	usage := map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens}
	if s.usage != nil {
		s.usage.record(UsageRecord{
			Time:         time.Now(),
			APIKeyPrefix: extractAPIKey(r),
			Model:        model,
			Endpoint:     "/v1/chat/completions",
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			DurationMs:   time.Since(startedAt).Milliseconds(),
			Status:       http.StatusOK,
		})
	}
	if !body.Stream {
		jsonOut(w, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": answer},
			"finish_reason": nil,
		}},
	}
	_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(chunk)+"\n\n")
	finish := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage}
	_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(finish)+"\n\n")
	_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
}

const defaultPublicModelName = "m365-copilot"

const sessionHeaderName = "X-M365-Session-Id"

// bindConversation 在请求完成后登记会话解析器索引与缓存统计，流式与非流式
// 路径共用。会话为内容键，云端的对话由 auto_cleanup 按 2h 闲置窗口回收，
// 这里不再做"用完即删"，否则复用永远不可能命中。
//
// 工具轮提前返回点也复用本函数登记本地会话。此时上游对话是一次性 router
// 对话（dropTransientConversation 会删掉它），ConversationID 为空：绑定的
// 意义不是复用云端对话，而是让下一轮 Resolve 的内容前缀匹配有据可查、并
// 维持账号粘性与 cacheStats 口径不变。因此空 ConversationID 时仍执行
// cacheStats / sink 回填，但跳过 sessionResolver.Bind 与 conversationManager
// 等任何依赖上游会话 ID 的动作 —— 没有上游会话可绑，写进去反而会让显式路径
// 或 auto_cleanup 误把一次性 router 对话当成长效会话。
func (s *Server) bindConversation(acc auth.AccountToken, body *oaiReq, r *http.Request, res chathub.Result, prompt string, startedAt time.Time) {
	emptyConversation := res.ConversationID == ""
	historyBody := *body
	historyBody.Messages = append(cloneMessages(body.Messages), oaiMsg{
		Role:             "assistant",
		Content:          res.Text,
		ReasoningContent: res.Reasoning,
	})
	if !emptyConversation {
		compression := s.sessionResolver.Bind(res.SessionID, res.ConversationID, acc.ID, &historyBody, "", r)
		if compression != nil && s.historyArchive != nil {
			accountEmail := acc.Email
			if accountEmail == "" && s.tokens != nil {
				if account, ok := s.tokens.Get(acc.ID); ok {
					accountEmail = account.Email
				}
			}
			if path, added, err := s.historyArchive.recordCompression(compression, accountEmail); err != nil {
				log.Printf("[history-archive] compression capture failed conversation=%s path=%s err=%v", compression.ConversationID, path, err)
			} else if added {
				log.Printf("[history-archive] compression captured conversation=%s path=%s before_bytes=%d after_bytes=%d", compression.ConversationID, path, compression.BeforeContextBytes, compression.AfterContextBytes)
			}
		}
		s.conversationManager.Record(res.ConversationID, acc.ID, prompt)
		if s.conversationManager.ShouldCleanup() {
			if cleaned := s.conversationManager.Cleanup(); len(cleaned) > 0 {
				log.Printf("[conversation-manager] auto-cleaned %d conversations", len(cleaned))
			}
		}
	} else {
		// 轻量登记：工具轮的上游对话是一次性 router 对话（已由
		// dropTransientConversation 删除），不写入 ConversationID。生成
		// 独立 UUID 作为 SessionID，绕过 Bind 内 explicitID 覆盖分支
		// （explicitID 仅在 sessionID=="" 时才覆盖），避免与客户端显式
		// X-M365-Session-Id 串扰。ContextHistory 含 router 决策文本，
		// 与下一轮回传的 tool_calls 助手消息不构成严格前缀，不会命中
		// 内容前缀匹配（那会向空对话只发增量、丢失上下文）；但可参与
		// 相似度兜底，命中时 HistoryLen=0，回答轮发全量到新对话。
		s.sessionResolver.Bind(uuid.NewString(), "", acc.ID, &historyBody, "", r)
	}

	apiKey := extractAPIKey(r)
	// 内部自调用（如评测经 callOwnChatCompletions 走本处理器）由调用方
	// 自行记账，此处跳过，否则同一次调用会被计入两条用量记录。
	if isInternalCall(r) {
		return
	}
	historyTokens := int64(0)
	upper := len(body.Messages) - 1
	if upper < 0 {
		upper = 0
	}
	for _, msg := range body.Messages[:upper] {
		historyTokens += EstimateTokens(contentToString(msg.Content))
	}
	newTokens := EstimateTokens(prompt)
	sessions := s.sessionResolver.ListSessions()
	cacheStats.RecordRequest(apiKey, historyTokens > 0, newTokens, historyTokens, len(sessions))
	// 历史 token 只有这里算得出来（有 body.Messages 和 session 解析结果）。
	// 内部委派时回填给外层，否则外层记录里缓存永远是 0（面板上的「缓0」）。
	if sink := innerStatsSink(r); sink != nil {
		sink.CacheTokens = historyTokens
	}
	// 内部委派时只跳过用量记账，不跳过上面的 cacheStats：缓存命中是真实发生的，
	// 而外层 /v1/messages、/v1/responses 不记 cacheStats，在这里跳掉会让缓存
	// 命中率凭空变低。所以这里不能复用 internalCallHeader —— 那个标记在
	// cacheStats 之前就 return 了，语义是「整段统计都由调用方负责」。
	//
	// 不加这个判断的后果（面板实测）：一次 /v1/messages 请求产生两行用量，
	// 时间戳相同、端点一个 messages 一个 chat/completions，token 被算两遍。
	if isInnerAdapterCall(r) {
		return
	}
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: apiKey,
		AccountEmail: acc.Email,
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/chat/completions",
		Stream:       body.Stream,
		InputTokens:  newTokens,
		OutputTokens: EstimateTokens(res.Text),
		CacheTokens:  historyTokens,
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
}

func extractAPIKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key != "" {
		return key
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		key = strings.TrimSpace(auth[7:])
	}
	if len(key) > 8 {
		return key[:8] + "..."
	}
	return key
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func extractOIDTID(accessToken string) (oid, tid string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	if v, ok := m["oid"].(string); ok {
		oid = v
	}
	if v, ok := m["tid"].(string); ok {
		tid = v
	}
	return oid, tid
}

// internalCallHeader 标记进程内自调用。带此头的请求由调用方负责用量记账，
// bindConversation 会跳过统计，避免同一次调用被重复计入。
const internalCallHeader = "X-M365-Internal-Call"

// isInternalCall 判定请求是否来自进程内自调用。
func isInternalCall(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get(internalCallHeader)) != ""
}
