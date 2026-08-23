package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"m365-copilot2api/internal/auth"
)

// refreshAllTestStore 建一个带 n 个账号的令牌存储。刷新令牌刻意取足够长且
// 唯一的字符串，好让「错误消息里不得出现刷新令牌」这条断言不会被短串误判。
func refreshAllTestStore(t *testing.T, n int) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("u-%d", i)
		tok := auth.TokenSet{
			HomeOID:      id,
			Email:        id + "@example.com",
			AccessToken:  "at-" + id,
			RefreshToken: "refresh-token-" + id + "-SECRET-VALUE",
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		if _, err := store.Upsert(tok); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	return store
}

// refreshAllTokenEndpoint 假装成 AAD 令牌端点。fail 决定哪些刷新令牌失败；
// 失败响应刻意把刷新令牌回显在 error_description 里，用来验证网关的脱敏。
func refreshAllTokenEndpoint(t *testing.T, fail map[string]bool) (maxConcurrent func() int) {
	t.Helper()
	var mu sync.Mutex
	inflight, peak := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inflight--
			mu.Unlock()
		}()
		time.Sleep(40 * time.Millisecond)
		_ = r.ParseForm()
		rt := r.FormValue("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		if fail[rt] {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_grant",
				"error_description": "AADSTS70008: the refresh token " + rt + " has expired. " +
					strings.Repeat("padding ", 60),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-for-" + rt,
			"refresh_token": "rotated-" + rt,
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(ts.Close)
	t.Setenv("M365_TOKEN_ENDPOINT", ts.URL)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

type refreshAllResponse struct {
	Total     int `json:"total"`
	Refreshed int `json:"refreshed"`
	Failed    int `json:"failed"`
	Results   []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"results"`
}

func postRefreshAll(t *testing.T, s *Server) (*httptest.ResponseRecorder, refreshAllResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.refreshAllAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil))
	var payload refreshAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec, payload
}

// 0 个账号是正常情况，不是错误。
func TestRefreshAllWithNoAccountsReturnsEmptySummary(t *testing.T) {
	s := &Server{tokens: refreshAllTestStore(t, 0)}
	rec, payload := postRefreshAll(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if payload.Total != 0 || payload.Refreshed != 0 || payload.Failed != 0 {
		t.Fatalf("summary=%+v", payload)
	}
	if payload.Results == nil {
		t.Fatal("results must be an empty array, not null")
	}
	if len(payload.Results) != 0 {
		t.Fatalf("results=%+v", payload.Results)
	}
	// 前端会直接读 results.length，所以 JSON 里必须是 []。
	if !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// 部分失败：汇总数字必须正确，且单个失败不影响其余账号。
func TestRefreshAllPartialFailureSummary(t *testing.T) {
	refreshAllTokenEndpoint(t, map[string]bool{
		"refresh-token-u-2-SECRET-VALUE": true,
	})
	s := &Server{tokens: refreshAllTestStore(t, 3)}

	rec, payload := postRefreshAll(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if payload.Total != 3 || payload.Refreshed != 2 || payload.Failed != 1 {
		t.Fatalf("summary total=%d refreshed=%d failed=%d body=%s",
			payload.Total, payload.Refreshed, payload.Failed, rec.Body.String())
	}
	if len(payload.Results) != 3 {
		t.Fatalf("results=%+v", payload.Results)
	}
	seen := map[string]bool{}
	for _, entry := range payload.Results {
		seen[entry.ID] = true
		if entry.Email != entry.ID+"@example.com" {
			t.Errorf("entry %+v has an unexpected email", entry)
		}
		switch entry.ID {
		case "u-2":
			if entry.OK {
				t.Errorf("u-2 should have failed: %+v", entry)
			}
			if entry.Error == "" {
				t.Error("failed entry carries no error message")
			}
			if len(entry.Error) > refreshAllErrorMaxLen {
				t.Errorf("error message is %d chars, want <= %d", len(entry.Error), refreshAllErrorMaxLen)
			}
		default:
			if !entry.OK || entry.Error != "" {
				t.Errorf("entry %+v should have succeeded", entry)
			}
		}
	}
	for _, id := range []string{"u-1", "u-2", "u-3"} {
		if !seen[id] {
			t.Errorf("missing account %s in results", id)
		}
	}
	// 上游把刷新令牌回显在 error_description 里；汇总响应绝不能带出去。
	if strings.Contains(rec.Body.String(), "refresh-token-u-2-SECRET-VALUE") {
		t.Fatalf("response leaks a refresh token: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "rotated-") || strings.Contains(rec.Body.String(), "new-access-for-") {
		t.Fatalf("response leaks token material: %s", rec.Body.String())
	}
}

// 全部失败也要走完，而不是在第一个错误处中断。
func TestRefreshAllContinuesWhenEveryAccountFails(t *testing.T) {
	refreshAllTokenEndpoint(t, map[string]bool{
		"refresh-token-u-1-SECRET-VALUE": true,
		"refresh-token-u-2-SECRET-VALUE": true,
		"refresh-token-u-3-SECRET-VALUE": true,
	})
	s := &Server{tokens: refreshAllTestStore(t, 3)}
	rec, payload := postRefreshAll(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if payload.Total != 3 || payload.Refreshed != 0 || payload.Failed != 3 {
		t.Fatalf("summary=%+v", payload)
	}
	if len(payload.Results) != 3 {
		t.Fatalf("results=%+v", payload.Results)
	}
}

// 并发有上限：同时打向令牌端点的请求数不得超过 refreshAllConcurrency，
// 但也必须真的并发（否则限流信号量就退化成串行）。
func TestRefreshAllCapsConcurrency(t *testing.T) {
	peak := refreshAllTokenEndpoint(t, nil)
	s := &Server{tokens: refreshAllTestStore(t, 12)}
	_, payload := postRefreshAll(t, s)
	if payload.Total != 12 || payload.Refreshed != 12 {
		t.Fatalf("summary=%+v", payload)
	}
	if got := peak(); got > refreshAllConcurrency {
		t.Fatalf("peak concurrency=%d, want <= %d", got, refreshAllConcurrency)
	}
	if got := peak(); got < 2 {
		t.Fatalf("peak concurrency=%d, refreshes did not run in parallel", got)
	}
}

func TestRefreshAllRejectsNonPost(t *testing.T) {
	s := &Server{tokens: refreshAllTestStore(t, 0)}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		s.refreshAllAccounts(rec, httptest.NewRequest(method, "/api/accounts/refresh-all", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status=%d want 405", method, rec.Code)
		}
	}
}

// 路由必须真的注册，并且落在 adminMiddleware 的受保护范围内。
func TestRefreshAllRouteIsRegisteredAndAdminOnly(t *testing.T) {
	s := &Server{
		tokens:        refreshAllTestStore(t, 0),
		adminPassword: "admin-password",
		adminSessions: map[string]time.Time{"session-token": time.Now().Add(time.Hour)},
	}
	routes := s.Routes()

	anonymous := httptest.NewRecorder()
	routes.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d want 401; body=%s", anonymous.Code, anonymous.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil)
	req.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: "session-token"})
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("body=%s", rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/accounts/refresh-all", nil)
	get.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: "session-token"})
	getRec := httptest.NewRecorder()
	routes.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d want 405", getRec.Code)
	}
}

func TestSanitizeRefreshErrorRedactsAndTruncates(t *testing.T) {
	if got := sanitizeRefreshError(nil, "rt"); got != "" {
		t.Fatalf("nil error produced %q", got)
	}
	const rt = "refresh-token-SECRET"
	err := fmt.Errorf("token endpoint HTTP 400: invalid_grant: %s is expired\nline two", rt)
	got := sanitizeRefreshError(err, rt)
	if strings.Contains(got, rt) {
		t.Fatalf("message still contains the refresh token: %q", got)
	}
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("message contains control whitespace: %q", got)
	}
	long := sanitizeRefreshError(fmt.Errorf("%s", strings.Repeat("x", 500)), "")
	if len(long) != refreshAllErrorMaxLen {
		t.Fatalf("truncated length=%d want %d", len(long), refreshAllErrorMaxLen)
	}
	// 多字节错误消息截断后仍必须是合法 UTF-8。
	multibyte := sanitizeRefreshError(fmt.Errorf("%s", strings.Repeat("刷新失败", 100)), "")
	if len(multibyte) > refreshAllErrorMaxLen {
		t.Fatalf("multibyte truncated length=%d", len(multibyte))
	}
	if !utf8.ValidString(multibyte) {
		t.Fatalf("truncation split a rune: %q", multibyte)
	}
}

func TestRefreshAllBudgetScalesWithAccountCount(t *testing.T) {
	if got := refreshAllBudget(0); got != 90*time.Second {
		t.Fatalf("budget(0)=%s want 90s", got)
	}
	if got := refreshAllBudget(10); got != 90*time.Second {
		t.Fatalf("budget(10)=%s want the 90s floor", got)
	}
	if got := refreshAllBudget(100); got != 200*time.Second {
		t.Fatalf("budget(100)=%s want 200s", got)
	}
	if got := refreshAllBudget(100000); got != 10*time.Minute {
		t.Fatalf("budget(100000)=%s want the 10m cap", got)
	}
}

// 慢账号不得挂死 handler：预算耗尽后返回已完成的部分结果，未完成的标记
// timeout。这里用请求上下文取消来驱动同一条 select 分支，因为真实预算
// （最低 90s）不适合放进单元测试。
func TestRefreshAllTimesOutInsteadOfHanging(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 一直卡住，直到测试放行，模拟对令牌端点的慢调用。
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	// 清理是 LIFO：必须先放行阻塞中的 handler，ts.Close 才不会等在它上面。
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("M365_TOKEN_ENDPOINT", ts.URL)

	s := &Server{tokens: refreshAllTestStore(t, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refreshAllAccounts(rec, req)
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler hung after the budget was exhausted")
	}

	var payload refreshAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Total != 2 || payload.Refreshed != 0 || payload.Failed != 2 {
		t.Fatalf("summary=%+v body=%s", payload, rec.Body.String())
	}
	for _, entry := range payload.Results {
		if entry.OK {
			t.Errorf("entry %+v should not be reported as refreshed", entry)
		}
		if entry.Error == "" {
			t.Errorf("entry %+v carries no error", entry)
		}
	}
	if !strings.Contains(rec.Body.String(), `"error":"timeout"`) {
		t.Fatalf("unfinished accounts must be marked timeout: %s", rec.Body.String())
	}
}