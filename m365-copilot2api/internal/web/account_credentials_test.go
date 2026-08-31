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

	"m365-copilot2api/internal/auth"
)

// 详情端点必须真的返回消息内容，而不只是计数。
func TestConversationDetailReturnsFullMessages(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, sessionResolver: openSessionResolver()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "what is the answer"},
		{Role: "assistant", Content: "the full answer body", ReasoningContent: "the reasoning trace"},
	}}
	s.sessionResolver.Bind("session-x", "conversation-x", "account-a", body, "", req)

	rec := httptest.NewRecorder()
	s.conversationDetail(rec, httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=conversation-x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object         string `json:"object"`
		ConversationID string `json:"conversationId"`
		SessionID      string `json:"sessionId"`
		AccountID      string `json:"accountId"`
		Title          string `json:"title"`
		MessageCount   int    `json:"messageCount"`
		Messages       []struct {
			Index            int    `json:"index"`
			Role             string `json:"role"`
			Text             string `json:"text"`
			ReasoningContent string `json:"reasoningContent"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "conversation.detail" {
		t.Errorf("object=%q", out.Object)
	}
	if out.ConversationID != "conversation-x" || out.SessionID != "session-x" || out.AccountID != "account-a" {
		t.Errorf("identity fields wrong: %+v", out)
	}
	if out.MessageCount != 2 || len(out.Messages) != 2 {
		t.Fatalf("messageCount=%d messages=%d", out.MessageCount, len(out.Messages))
	}
	// 关键：正文必须真的在响应里。
	if out.Messages[0].Role != "user" || out.Messages[0].Text != "what is the answer" {
		t.Errorf("first message=%+v", out.Messages[0])
	}
	if out.Messages[1].Text != "the full answer body" {
		t.Errorf("assistant text=%q", out.Messages[1].Text)
	}
	if out.Messages[1].ReasoningContent != "the reasoning trace" {
		t.Errorf("reasoning=%q", out.Messages[1].ReasoningContent)
	}
	if out.Title != "what is the answer" {
		t.Errorf("title=%q", out.Title)
	}

	// 按 sessionId 查询也要命中同一条会话。
	rec = httptest.NewRecorder()
	s.conversationDetail(rec, httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=session-x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup by sessionId status=%d", rec.Code)
	}
}

func TestConversationDetailErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	s := &Server{sessionResolver: openSessionResolver()}

	rec := httptest.NewRecorder()
	s.conversationDetail(rec, httptest.NewRequest(http.MethodGet, "/api/conversations/detail", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing id status=%d want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.conversationDetail(rec, httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id status=%d want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.conversationDetail(rec, httptest.NewRequest(http.MethodPost, "/api/conversations/detail?id=x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status=%d want 405", rec.Code)
	}
}

// 详情路由必须已注册（否则前端点开会静默 404）。
func TestConversationDetailRouteIsRegistered(t *testing.T) {
	routes := (&Server{}).Routes()
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=x", nil))
	if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatal("/api/conversations/detail is not routed")
	}
}

// 核心断言：改密绝不落明文。
func TestCredentialVaultNeverWritesPlaintext(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "credential-vault.json")
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))

	vault, err := auth.OpenCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "Sup3r-S3cret-Passw0rd-do-not-persist"
	if err := vault.Put("account-a", "user@example.com", secret); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the vault file contains the password in plaintext")
	}
	// 明文 JSON 字段名也不该出现，说明整个 body 都被加密了。
	if strings.Contains(string(raw), "\"password\"") {
		t.Error("vault file exposes an unencrypted password field")
	}
	if !strings.Contains(string(raw), "m365-credential-vault") {
		t.Errorf("unexpected vault envelope: %s", raw)
	}

	// 加密后仍要能正确取回，否则重新授权无从复用。
	got, err := vault.Get("account-a")
	if err != nil || got != secret {
		t.Fatalf("Get returned %q err=%v", got, err)
	}

	// 重新打开（模拟重启）后依然可解密。
	reopened, err := auth.OpenCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Get("account-a"); err != nil || got != secret {
		t.Fatalf("after reopen Get=%q err=%v", got, err)
	}
	// 列表只给元数据。
	metas := reopened.List()
	if len(metas) != 1 || metas[0].AccountID != "account-a" || metas[0].Email != "user@example.com" {
		t.Fatalf("metadata=%+v", metas)
	}
	blob, _ := json.Marshal(metas)
	if strings.Contains(string(blob), secret) {
		t.Error("credential metadata leaked the password")
	}
}

// 改密端点：写入成功、响应不回显口令、GET 不泄露口令。
func TestAccountCredentialsEndpointDoesNotEchoSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))
	credentialVaultPathOverride = filepath.Join(dir, "credential-vault.json")
	t.Cleanup(func() { credentialVaultPathOverride = "" })

	s := &Server{}
	const secret = "Rotate-Me-9911"
	body := `{"accountId":"account-a","email":"user@example.com","password":"` + secret + `"}`
	rec := httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/credentials", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response echoed the password: %s", rec.Body.String())
	}
	var out struct {
		Status    string `json:"status"`
		AccountID string `json:"accountId"`
		Encrypted bool   `json:"encrypted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "updated" || out.AccountID != "account-a" || !out.Encrypted {
		t.Errorf("out=%+v", out)
	}

	// 磁盘上仍不得有明文。
	raw, err := os.ReadFile(credentialVaultPathOverride)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("endpoint wrote the password in plaintext")
	}

	// GET 只返回元数据。
	rec = httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("GET leaked the password: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account-a") {
		t.Errorf("GET missing metadata: %s", rec.Body.String())
	}

	// DELETE 生效。
	rec = httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodDelete, "/api/accounts/credentials?accountId=account-a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodDelete, "/api/accounts/credentials?accountId=account-a", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE status=%d want 404", rec.Code)
	}
}

func TestAccountCredentialsRequiresFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))
	credentialVaultPathOverride = filepath.Join(dir, "credential-vault.json")
	t.Cleanup(func() { credentialVaultPathOverride = "" })

	s := &Server{}
	rec := httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/credentials", strings.NewReader(`{"accountId":"a"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// run-scripts 缺少账密时必须明确拒绝，而不是假装启动了流程。
func TestAccountRunScriptsRequiresCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))
	credentialVaultPathOverride = filepath.Join(dir, "credential-vault.json")
	t.Cleanup(func() { credentialVaultPathOverride = "" })

	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store}
	rec := httptest.NewRecorder()
	s.accountRunScripts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/web/run-scripts", strings.NewReader(`{"email":"user@example.com"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.accountRunScripts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/web/run-scripts", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status=%d want 405", rec.Code)
	}
}

// 新增路由都必须真的注册。
func TestAccountCredentialRoutesAreRegistered(t *testing.T) {
	routes := (&Server{}).Routes()
	for _, path := range []string{"/api/accounts/credentials", "/api/accounts/web/run-scripts"} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("%s is not routed", path)
		}
	}
}

// beginPKCEAuthorization 是 startPKCE 与一键回调共用的那一套 PKCE。
func TestBeginPKCEAuthorizationSharesPKCEState(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{}}
	state, url, attempt, redirect, err := s.beginPKCEAuthorization("login", false)
	if err != nil {
		t.Fatal(err)
	}
	if state == "" || attempt == 0 {
		t.Fatalf("state=%q attempt=%d", state, attempt)
	}
	if !strings.Contains(url, "code_challenge=") || !strings.Contains(url, "state="+state) {
		t.Errorf("authorization url missing PKCE parameters: %s", url)
	}
	if redirect == "" {
		t.Error("redirect uri empty")
	}
	// state 必须进入 server 的 pkce 表，这样 /api/auth/callback 能接得住。
	s.mu.Lock()
	_, ok := s.pkce[state]
	s.mu.Unlock()
	if !ok {
		t.Error("state was not registered in the shared pkce map")
	}
}

// 已配置管理员口令时，凭据端点必须拒绝无会话调用。
func TestCredentialEndpointsRequireAdminSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))
	credentialVaultPathOverride = filepath.Join(dir, "credential-vault.json")
	t.Cleanup(func() { credentialVaultPathOverride = "" })

	s := &Server{adminPassword: "configured", adminSessions: map[string]time.Time{}}

	rec := httptest.NewRecorder()
	s.accountCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/credentials",
		strings.NewReader(`{"accountId":"a","password":"p"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("credentials status=%d want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.accountRunScripts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/web/run-scripts",
		strings.NewReader(`{"email":"u@example.com","password":"p"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("run-scripts status=%d want 401", rec.Code)
	}
}
