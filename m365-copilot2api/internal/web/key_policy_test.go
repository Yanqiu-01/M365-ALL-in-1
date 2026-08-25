package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newPolicyServer 造一个只带 key 策略所需部件的 Server。
func newPolicyServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		apiKeys:   newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json")),
		keyLimits: newKeyConcurrency(),
		keyRates:  newKeyRateLimiter(),
	}
}

func mustCreateKey(t *testing.T, s *Server, name string) (apiKeyRecord, string) {
	t.Helper()
	record, raw, err := s.apiKeys.create(name)
	if err != nil {
		t.Fatal(err)
	}
	return record, raw
}

func keyedRequest(raw, model string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	_ = model
	return req
}

// 旧版 api-keys.json 缺少新字段时必须按默认值读入（不限模型、不限并发），
// 而不是解析失败或把 key 变成不可用。
func TestAPIKeyStoreReadsLegacyFileWithoutNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	legacy := `{"keys":[{"id":"abc","name":"legacy","prefix":"m365_00000","hash":"` +
		keyHash("m365_legacyraw") + `","createdAt":"2026-01-01T00:00:00Z","revoked":false}]}`
	if err := writeFileAtomic(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)
	store := openAPIKeys()
	if len(store.Keys) != 1 {
		t.Fatalf("loaded %d keys", len(store.Keys))
	}
	record := store.Keys[0]
	if record.AllowedModelIDs != nil {
		t.Errorf("legacy key gained a model whitelist: %v", record.AllowedModelIDs)
	}
	if record.MaxConcurrent != 0 || record.RPMLimit != 0 {
		t.Errorf("legacy key gained limits: concurrent=%d rpm=%d", record.MaxConcurrent, record.RPMLimit)
	}
	if !record.modelAllowed("gpt-5") {
		t.Error("legacy key must not restrict models")
	}
	if !store.valid("m365_legacyraw") {
		t.Error("legacy key stopped authenticating")
	}
}

// update 的指针语义：未提供的字段保持原值，提供零值则真的清零。
func TestAPIKeyUpdatePartialSemantics(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	record, _, err := store.create("original")
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"gpt-5", "gpt-5-mini"}
	concurrent := 3
	rpm := 60
	if _, ok, err := store.update(record.ID, apiKeyPatch{
		AllowedModelIDs: &models, MaxConcurrent: &concurrent, RPMLimit: &rpm,
	}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	// name 未提供，必须保持原值。
	name := "renamed"
	updated, ok, err := store.update(record.ID, apiKeyPatch{Name: &name})
	if err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name=%q", updated.Name)
	}
	if len(updated.AllowedModelIDs) != 2 || updated.MaxConcurrent != 3 || updated.RPMLimit != 60 {
		t.Errorf("unprovided fields were clobbered: %+v", updated)
	}

	// 显式零值必须真的清空，而不是被当成「未提供」。
	empty := []string{}
	zero := 0
	updated, ok, err = store.update(record.ID, apiKeyPatch{AllowedModelIDs: &empty, MaxConcurrent: &zero})
	if err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	if len(updated.AllowedModelIDs) != 0 {
		t.Errorf("whitelist not cleared: %v", updated.AllowedModelIDs)
	}
	if updated.MaxConcurrent != 0 {
		t.Errorf("maxConcurrent not cleared: %d", updated.MaxConcurrent)
	}
	if updated.RPMLimit != 60 {
		t.Errorf("rpmLimit should have survived: %d", updated.RPMLimit)
	}
}

// enabled 与 revoked 必须是同一份真相：写 enabled=false 等于 revoked=true，
// 两者矛盾时报错而不是任选其一。
func TestAPIKeyEnabledAndRevokedShareOneSourceOfTruth(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	record, raw, err := store.create("k")
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	updated, ok, err := store.update(record.ID, apiKeyPatch{Enabled: &enabled})
	if err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	if !updated.Revoked {
		t.Error("enabled=false must set revoked=true")
	}
	if newAPIKeyView(updated).Enabled {
		t.Error("view enabled must be derived from revoked")
	}
	if store.valid(raw) {
		t.Error("disabled key still authenticates")
	}

	yes, no := true, true
	if _, _, err := store.update(record.ID, apiKeyPatch{Revoked: &yes, Enabled: &no}); err == nil {
		t.Error("contradicting revoked/enabled must be rejected")
	}
}

// 核心断言：白名单必须真正拦住越权模型，而不只是把字段存下来。
func TestKeyModelWhitelistBlocksUnlistedModel(t *testing.T) {
	s := newPolicyServer(t)
	record, raw := mustCreateKey(t, s, "restricted")
	models := []string{"gpt-5-mini"}
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{AllowedModelIDs: &models}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	// 越权模型：必须 403 且不放行。
	rec := httptest.NewRecorder()
	release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
	if allowed {
		release()
		t.Fatal("gpt-5 must be rejected for a key limited to gpt-5-mini")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "model_not_allowed" {
		t.Errorf("error type=%q body=%s", payload.Error.Type, rec.Body.String())
	}

	// 白名单内模型：必须放行。
	rec = httptest.NewRecorder()
	release, allowed = s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5-mini"), "gpt-5-mini", writeOpenAIError)
	if !allowed {
		t.Fatalf("gpt-5-mini must be allowed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	release()

	// 空白名单表示不限制。
	empty := []string{}
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{AllowedModelIDs: &empty}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	rec = httptest.NewRecorder()
	release, allowed = s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
	if !allowed {
		t.Fatalf("empty whitelist must not restrict, status=%d", rec.Code)
	}
	release()
}

// 省略 model 不能成为绕过白名单的后门。
func TestKeyModelWhitelistRejectsMissingModel(t *testing.T) {
	s := newPolicyServer(t)
	record, raw := mustCreateKey(t, s, "restricted")
	models := []string{"gpt-5-mini"}
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{AllowedModelIDs: &models}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	rec := httptest.NewRecorder()
	release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, ""), "", writeOpenAIError)
	if allowed {
		release()
		t.Fatal("an omitted model must not bypass the whitelist")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
}

// 核心断言：maxConcurrent 必须真的限流。
func TestKeyMaxConcurrentLimitsInflightRequests(t *testing.T) {
	s := newPolicyServer(t)
	record, raw := mustCreateKey(t, s, "limited")
	limit := 2
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{MaxConcurrent: &limit}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}

	releases := []func(){}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
		if !allowed {
			t.Fatalf("request %d must be admitted, status=%d", i, rec.Code)
		}
		releases = append(releases, release)
	}
	if got := s.keyLimits.Inflight(record.ID); got != 2 {
		t.Fatalf("inflight=%d want 2", got)
	}

	// 第 3 个并发请求必须被挡住。
	rec := httptest.NewRecorder()
	release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
	if allowed {
		release()
		t.Fatal("third concurrent request must be rejected when maxConcurrent=2")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "concurrency_limit_exceeded") {
		t.Errorf("body=%s", rec.Body.String())
	}

	// 释放一个槽位后必须重新放行，且计数归零可回收。
	releases[0]()
	rec = httptest.NewRecorder()
	release, allowed = s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
	if !allowed {
		t.Fatalf("a freed slot must admit the next request, status=%d", rec.Code)
	}
	release()
	releases[1]()
	if got := s.keyLimits.Inflight(record.ID); got != 0 {
		t.Errorf("inflight=%d want 0 after all releases", got)
	}
}

// maxConcurrent=0 表示沿用全局默认，不施加 key 级并发限制。
func TestKeyZeroMaxConcurrentIsUnlimited(t *testing.T) {
	s := newPolicyServer(t)
	_, raw := mustCreateKey(t, s, "unlimited")
	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
		if !allowed {
			t.Fatalf("request %d rejected with maxConcurrent=0, status=%d", i, rec.Code)
		}
		defer release()
	}
}

// 并发压力下计数不能漂移。
func TestKeyConcurrencyIsRaceFree(t *testing.T) {
	limiter := newKeyConcurrency()
	var wg sync.WaitGroup
	admitted := make(chan struct{}, 200)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := limiter.TryAcquire("key", 5); ok {
				admitted <- struct{}{}
				time.Sleep(time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()
	close(admitted)
	if got := limiter.Inflight("key"); got != 0 {
		t.Errorf("inflight=%d want 0", got)
	}
	if len(admitted) == 0 {
		t.Error("no request was admitted")
	}
}

// rpmLimit 必须真的限住每分钟请求数。
func TestKeyRPMLimitRejectsBurst(t *testing.T) {
	s := newPolicyServer(t)
	record, raw := mustCreateKey(t, s, "rpm")
	rpm := 3
	if _, ok, err := s.apiKeys.update(record.ID, apiKeyPatch{RPMLimit: &rpm}); err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
		if !allowed {
			t.Fatalf("request %d must be admitted, status=%d", i, rec.Code)
		}
		release()
	}
	rec := httptest.NewRecorder()
	release, allowed := s.enforceKeyPolicy(rec, keyedRequest(raw, "gpt-5"), "gpt-5", writeOpenAIError)
	if allowed {
		release()
		t.Fatal("the 4th request within a minute must be rejected when rpmLimit=3")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// 未登记的 key（例如直传 JWT）不应被 key 级策略拦住。
func TestUnknownKeyIsNotSubjectToKeyPolicy(t *testing.T) {
	s := newPolicyServer(t)
	rec := httptest.NewRecorder()
	release, allowed := s.enforceKeyPolicy(rec, keyedRequest("eyJhbGciOiJub25lIn0.e30.", "gpt-5"), "gpt-5", writeOpenAIError)
	if !allowed {
		t.Fatalf("unregistered credential must not hit key policy, status=%d", rec.Code)
	}
	release()
}

// PATCH /api/admin/keys 的请求/响应形状。
func TestAdminKeysPatchUpdatesMetadata(t *testing.T) {
	s := newPolicyServer(t)
	record, _ := mustCreateKey(t, s, "before")

	body := `{"id":"` + record.ID + `","name":"after","allowedModelIds":["gpt-5"],"maxConcurrent":4,"rpmLimit":30,"revoked":false}`
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.adminKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string     `json:"status"`
		Record apiKeyView `json:"record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "updated" {
		t.Errorf("status=%q", out.Status)
	}
	if out.Record.Name != "after" || out.Record.MaxConcurrent != 4 || out.Record.RPMLimit != 30 {
		t.Errorf("record=%+v", out.Record)
	}
	if len(out.Record.AllowedModelIDs) != 1 || out.Record.AllowedModelIDs[0] != "gpt-5" {
		t.Errorf("allowedModelIds=%v", out.Record.AllowedModelIDs)
	}
	if !out.Record.Enabled {
		t.Error("enabled should be true when revoked=false")
	}
	// 响应绝不能带出 hash 或明文。
	if strings.Contains(rec.Body.String(), "\"hash\"") || strings.Contains(rec.Body.String(), "\"raw\"") {
		t.Errorf("PATCH response leaked key material: %s", rec.Body.String())
	}

	// GET 必须返回同样的视图形状，且带 enabled。
	rec = httptest.NewRecorder()
	s.adminKeys(rec, httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "\"enabled\"") {
		t.Errorf("GET body missing enabled: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\"hash\":\"") {
		t.Errorf("GET leaked hash: %s", rec.Body.String())
	}
}

func TestAdminKeysPatchUnknownIDReturns404(t *testing.T) {
	s := newPolicyServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/keys", strings.NewReader(`{"id":"nope","name":"x"}`))
	rec := httptest.NewRecorder()
	s.adminKeys(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}
