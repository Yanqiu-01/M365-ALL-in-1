package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// 端到端证据：越权模型必须在真实的 /v1/chat/completions 入口被拦下，
// 而不只是在 enforceKeyPolicy 的单元测试里。请求经过完整 Routes()
// （含 adminMiddleware 的 API key 校验），因此这条断言同时证明了
// 「key 通过了鉴权」与「白名单随后仍然拦住了它」两件事。
func TestChatCompletionsBlocksModelOutsideKeyWhitelist(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		apiKeys:   newAPIKeyStore(filepath.Join(dir, "api-keys.json")),
		keyLimits: newKeyConcurrency(),
		keyRates:  newKeyRateLimiter(),
		debug:     &debugStore{path: filepath.Join(dir, "debug-logs.jsonl")},
	}
	record, raw, err := s.apiKeys.create("restricted")
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"gpt-5-mini"}
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{AllowedModelIDs: &models}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	routes := s.Routes()
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403; body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Error.Type != "model_not_allowed" {
		t.Errorf("error.type=%q body=%s", payload.Error.Type, rec.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "gpt-5") {
		t.Errorf("error.message=%q", payload.Error.Message)
	}
}

// 端到端证据：maxConcurrent 用满后，真实入口返回 429 而不是继续往上游打。
func TestChatCompletionsEnforcesMaxConcurrentAtEntryPoint(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		apiKeys:   newAPIKeyStore(filepath.Join(dir, "api-keys.json")),
		keyLimits: newKeyConcurrency(),
		keyRates:  newKeyRateLimiter(),
		debug:     &debugStore{path: filepath.Join(dir, "debug-logs.jsonl")},
	}
	record, raw, err := s.apiKeys.create("limited")
	if err != nil {
		t.Fatal(err)
	}
	limit := 1
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{MaxConcurrent: &limit}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	// 占满该 key 的唯一槽位，模拟一个仍在处理中的请求。
	release, ok := s.keyLimits.TryAcquire(record.ID, limit)
	if !ok {
		t.Fatal("failed to occupy the only slot")
	}
	defer release()

	routes := s.Routes()
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "concurrency_limit_exceeded") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// 端到端证据：白名单同样覆盖 /v1/messages（Anthropic 形状），
// 证明三个协议入口共用同一处强制点。
func TestAnthropicMessagesInheritsKeyWhitelist(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		apiKeys:   newAPIKeyStore(filepath.Join(dir, "api-keys.json")),
		keyLimits: newKeyConcurrency(),
		keyRates:  newKeyRateLimiter(),
		debug:     &debugStore{path: filepath.Join(dir, "debug-logs.jsonl")},
	}
	record, raw, err := s.apiKeys.create("restricted")
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"gpt-5-mini"}
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{AllowedModelIDs: &models}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	routes := s.Routes()
	body := `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403; body=%s", rec.Code, rec.Body.String())
	}
	// /v1/messages 把内层错误重新包成 Anthropic 信封：状态码保留 403，
	// type 变成 api_error，拒绝原因保留在 message 里。
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Type != "error" {
		t.Errorf("envelope type=%q", payload.Type)
	}
	if !strings.Contains(payload.Error.Message, "not permitted for this API key") {
		t.Errorf("error.message=%q body=%s", payload.Error.Message, rec.Body.String())
	}
}
