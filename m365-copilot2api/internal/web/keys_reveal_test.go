package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newRevealTestServer 建一个用临时 vault 的 Server，避免碰用户真实数据目录。
func newRevealTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "store.key"))
	credentialVaultPathOverride = filepath.Join(dir, "credential-vault.json")
	t.Cleanup(func() { credentialVaultPathOverride = "" })
	return &Server{apiKeys: newAPIKeyStore(filepath.Join(dir, "api-keys.json"))}
}

// 随机生成的 key：创建后应能原样回显。
func TestRevealRandomKeyRoundTrip(t *testing.T) {
	s := newRevealTestServer(t)
	rec, raw, err := s.apiKeys.createWithSecret("random", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.retainAPIKeySecret(rec.ID, rec.Name, raw); err != nil {
		t.Fatalf("retain: %v", err)
	}
	got, err := s.revealAPIKeySecret(rec.ID)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if got != raw {
		t.Errorf("reveal = %q, want %q", got, raw)
	}
}

// 自定义口令的 key 同样要能回显。
func TestRevealCustomSecretKey(t *testing.T) {
	s := newRevealTestServer(t)
	const custom = "MyCustomKey-0123456789"
	rec, raw, err := s.apiKeys.createWithSecret("custom", custom)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if raw != custom {
		t.Fatalf("raw = %q, want the submitted secret", raw)
	}
	if err := s.retainAPIKeySecret(rec.ID, rec.Name, raw); err != nil {
		t.Fatalf("retain: %v", err)
	}
	got, err := s.revealAPIKeySecret(rec.ID)
	if err != nil || got != custom {
		t.Errorf("reveal = (%q,%v), want (%q,nil)", got, err, custom)
	}
}

// 启用回显之前创建的 key：vault 里没有条目，必须如实报错而不是回空串。
func TestRevealLegacyKeyReportsNotRetained(t *testing.T) {
	s := newRevealTestServer(t)
	rec, _, err := s.apiKeys.createWithSecret("legacy", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 故意不调用 retainAPIKeySecret，模拟老数据。
	got, err := s.revealAPIKeySecret(rec.ID)
	if got != "" {
		t.Errorf("legacy reveal returned a secret %q, must return none", got)
	}
	if err == nil || !strings.Contains(err.Error(), "无法回显") {
		t.Errorf("err = %v, want the not-retained explanation", err)
	}
}

// 删除 key 时明文必须一起消失，不留孤儿密钥。
func TestForgetAPIKeySecretOnDelete(t *testing.T) {
	s := newRevealTestServer(t)
	rec, raw, _ := s.apiKeys.createWithSecret("doomed", "")
	if err := s.retainAPIKeySecret(rec.ID, rec.Name, raw); err != nil {
		t.Fatalf("retain: %v", err)
	}
	if _, err := s.revealAPIKeySecret(rec.ID); err != nil {
		t.Fatalf("precondition: reveal should work, got %v", err)
	}
	s.forgetAPIKeySecret(rec.ID)
	if _, err := s.revealAPIKeySecret(rec.ID); err == nil {
		t.Error("secret still retrievable after forget")
	}
}

// 列表接口必须保持脱敏：明文绝不出现在轮询用的列表里。
func TestListViewsNeverCarryPlaintext(t *testing.T) {
	s := newRevealTestServer(t)
	rec, raw, _ := s.apiKeys.createWithSecret("listed", "")
	_ = s.retainAPIKeySecret(rec.ID, rec.Name, raw)
	blob, err := json.Marshal(map[string]any{"keys": s.apiKeys.listViews()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), raw) {
		t.Error("list response contains the key plaintext")
	}
	// 摘要也不该外泄。
	if strings.Contains(string(blob), keyHash(raw)) {
		t.Error("list response contains the key hash")
	}
}

// HTTP 层：未知 id 返回 404，缺 id 返回 400，老 key 返回 409。
func TestAdminKeyRevealHTTPStatuses(t *testing.T) {
	s := newRevealTestServer(t)
	rec, raw, _ := s.apiKeys.createWithSecret("http", "")

	rr := httptest.NewRecorder()
	s.adminKeyReveal(rr, httptest.NewRequest(http.MethodGet, "/api/admin/keys/reveal", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing id: status %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	s.adminKeyReveal(rr, httptest.NewRequest(http.MethodGet, "/api/admin/keys/reveal?id=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id: status %d, want 404", rr.Code)
	}

	// 未保留明文 → 409
	rr = httptest.NewRecorder()
	s.adminKeyReveal(rr, httptest.NewRequest(http.MethodGet, "/api/admin/keys/reveal?id="+rec.ID, nil))
	if rr.Code != http.StatusConflict {
		t.Errorf("not retained: status %d, want 409", rr.Code)
	}

	// 保留后 → 200 且回显明文
	if err := s.retainAPIKeySecret(rec.ID, rec.Name, raw); err != nil {
		t.Fatalf("retain: %v", err)
	}
	rr = httptest.NewRecorder()
	s.adminKeyReveal(rr, httptest.NewRequest(http.MethodGet, "/api/admin/keys/reveal?id="+rec.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("retained: status %d, want 200", rr.Code)
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Key != raw || out.ID != rec.ID {
		t.Errorf("reveal payload = %+v, want id=%s key=%s", out, rec.ID, raw)
	}

	// 非 GET 必须拒绝
	rr = httptest.NewRecorder()
	s.adminKeyReveal(rr, httptest.NewRequest(http.MethodPost, "/api/admin/keys/reveal?id="+rec.ID, nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rr.Code)
	}
}
