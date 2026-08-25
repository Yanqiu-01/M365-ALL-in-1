package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 自定义口令的核心契约：调用方提交的明文原封不动成为可用 key，
// 而磁盘上只留 sha256 摘要。
func TestCreateWithSecretUsesPlaintextAndPersistsOnlyHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	store := newAPIKeyStore(path)
	const secret = "my-own-gateway-secret-2026"

	record, raw, err := store.createWithSecret("custom", secret)
	if err != nil {
		t.Fatalf("createWithSecret: %v", err)
	}
	if raw != secret {
		t.Fatalf("returned key=%q, want the submitted secret verbatim", raw)
	}
	if !store.valid(secret) {
		t.Fatal("the submitted secret must authenticate")
	}
	if store.valid(secret + "x") {
		t.Fatal("a near-miss secret must not authenticate")
	}
	if record.Prefix != secret[:12] {
		t.Fatalf("prefix=%q want %q", record.Prefix, secret[:12])
	}
	// ID 必须独立随机，不能是明文的可逆派生（否则公开字段泄漏前 8 字节）。
	if strings.HasPrefix(secret, record.ID) || strings.Contains(secret, record.ID) {
		t.Fatalf("id=%q is derived from the secret", record.ID)
	}
	if len(record.ID) != 16 {
		t.Fatalf("id=%q want 16 hex chars (8 random bytes)", record.ID)
	}
	if record.Hash != "" || record.Raw != "" {
		t.Fatalf("returned record leaks hash=%q raw=%q", record.Hash, record.Raw)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), secret) {
		t.Fatal("the key file contains the secret in plaintext")
	}
	if !strings.Contains(string(onDisk), keyHash(secret)) {
		t.Fatal("the key file is missing the secret hash")
	}
}

// 两个独立创建的自定义 key 必须拿到不同的 ID，证明 ID 来自 crypto/rand
// 而不是明文或时间戳。
func TestCreateWithSecretGeneratesIndependentIDs(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	first, _, err := store.createWithSecret("a", "secret-number-one-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.createWithSecret("b", "secret-number-two-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("both keys share id %q", first.ID)
	}
}

func TestCreateWithSecretRejectsBadShapes(t *testing.T) {
	cases := map[string]string{
		"too short":        "short-secret",
		"exactly 15 bytes": strings.Repeat("a", 15),
		"contains space":   "has a space in it here",
		"contains tab":     "has\ta-tab-in-it-here",
		"contains newline": "has\na-newline-here-x",
		"control char":     "has\x01a-control-char",
		"non ascii":        "密钥密钥密钥密钥密钥密钥密钥密钥",
		"too long":         strings.Repeat("a", 129),
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
			_, _, err := store.createWithSecret("bad", secret)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if got := keySecretHTTPStatus(err); got != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 (err=%v)", got, err)
			}
			if len(store.Keys) != 0 {
				t.Fatalf("rejected secret still stored: %+v", store.Keys)
			}
		})
	}
}

// 边界必须是可用的，不是差一位地被拒。
func TestCreateWithSecretAcceptsLengthBoundaries(t *testing.T) {
	for _, n := range []int{customKeySecretMinLen, customKeySecretMaxLen} {
		store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
		secret := strings.Repeat("a", n)
		if _, raw, err := store.createWithSecret("edge", secret); err != nil || raw != secret {
			t.Fatalf("len=%d raw=%q err=%v", n, raw, err)
		}
	}
}

func TestCreateWithSecretRejectsDuplicate(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	const secret = "duplicate-secret-value"
	if _, _, err := store.createWithSecret("first", secret); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.createWithSecret("second", secret)
	if err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if got := keySecretHTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("status=%d want 409 (err=%v)", got, err)
	}
	if len(store.Keys) != 1 {
		t.Fatalf("duplicate was stored: %+v", store.Keys)
	}
	// 随机 key 与已存在的自定义 key 冲突几乎不可能，但随机路径不应被唯一性
	// 检查影响到行为。
	if _, _, err := store.create("random"); err != nil {
		t.Fatalf("random create after duplicate rejection: %v", err)
	}
}

// 不传 secret 时行为必须与改动前完全一致：随机、带 m365_ 前缀、长度不变。
func TestCreateWithoutSecretKeepsRandomShape(t *testing.T) {
	store := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	first, raw, err := store.create("random")
	if err != nil {
		t.Fatal(err)
	}
	// "m365_" + 32 字节 hex = 5 + 64。
	if len(raw) != 69 || !strings.HasPrefix(raw, "m365_") {
		t.Fatalf("raw=%q len=%d, want m365_ + 64 hex chars", raw, len(raw))
	}
	if first.Prefix != raw[:12] {
		t.Fatalf("prefix=%q want %q", first.Prefix, raw[:12])
	}
	if !store.valid(raw) {
		t.Fatal("randomly generated key must authenticate")
	}
	second, otherRaw, err := store.create("random")
	if err != nil {
		t.Fatal(err)
	}
	if raw == otherRaw || first.ID == second.ID {
		t.Fatal("two random keys must differ")
	}
	// createWithSecret("") 与 create() 必须等价。
	_, delegated, err := store.createWithSecret("random", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(delegated) != 69 || !strings.HasPrefix(delegated, "m365_") {
		t.Fatalf("delegated raw=%q, want the same random shape", delegated)
	}
}

func TestKeyPrefixNeverPanicsOnShortInput(t *testing.T) {
	for _, in := range []string{"", "a", "abcdefghijk", "abcdefghijkl", "abcdefghijklm"} {
		got := keyPrefix(in)
		if len(in) < 12 && got != in {
			t.Fatalf("keyPrefix(%q)=%q want %q", in, got, in)
		}
		if len(in) >= 12 && got != in[:12] {
			t.Fatalf("keyPrefix(%q)=%q want %q", in, got, in[:12])
		}
	}
}

// HTTP 契约：POST /api/admin/keys 的 secret 字段。
func TestAdminKeysPostAcceptsCustomSecret(t *testing.T) {
	s := &Server{apiKeys: newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))}
	const secret = "console-supplied-secret-01"

	rec := httptest.NewRecorder()
	s.adminKeys(rec, httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"name":"custom","secret":"`+secret+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Key    string `json:"key"`
		Record struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Prefix  string `json:"prefix"`
			Enabled bool   `json:"enabled"`
			Hash    string `json:"hash"`
		} `json:"record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Key != secret {
		t.Fatalf("key=%q want the submitted secret", payload.Key)
	}
	if payload.Record.Prefix != secret[:12] || payload.Record.Name != "custom" || !payload.Record.Enabled {
		t.Fatalf("record=%+v", payload.Record)
	}
	if payload.Record.Hash != "" {
		t.Fatal("response leaks the key hash")
	}
	if !s.apiKeys.valid(secret) {
		t.Fatal("the created key must authenticate")
	}
}

func TestAdminKeysPostRejectsInvalidSecret(t *testing.T) {
	s := &Server{apiKeys: newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))}

	tooShort := httptest.NewRecorder()
	s.adminKeys(tooShort, httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"name":"x","secret":"tiny"}`)))
	if tooShort.Code != http.StatusBadRequest {
		t.Fatalf("short secret status=%d want 400; body=%s", tooShort.Code, tooShort.Body.String())
	}

	spaced := httptest.NewRecorder()
	s.adminKeys(spaced, httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"name":"x","secret":"has a space in it here"}`)))
	if spaced.Code != http.StatusBadRequest {
		t.Fatalf("spaced secret status=%d want 400; body=%s", spaced.Code, spaced.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(spaced.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", spaced.Body.String(), err)
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Errorf("error.type=%q", payload.Error.Type)
	}
	// 错误消息不得回显调用方提交的明文。
	if strings.Contains(spaced.Body.String(), "has a space in it here") {
		t.Errorf("error body echoes the submitted secret: %s", spaced.Body.String())
	}

	const dup = "already-taken-secret-x"
	first := httptest.NewRecorder()
	s.adminKeys(first, httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"name":"one","secret":"`+dup+`"}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	s.adminKeys(second, httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"name":"two","secret":"`+dup+`"}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d want 409; body=%s", second.Code, second.Body.String())
	}
	if strings.Contains(second.Body.String(), dup) {
		t.Errorf("conflict body echoes the submitted secret: %s", second.Body.String())
	}
	if len(s.apiKeys.list()) != 1 {
		t.Fatalf("unexpected key count: %+v", s.apiKeys.list())
	}
}

// 不带 secret 的 POST 行为不变：仍返回随机 key。
func TestAdminKeysPostWithoutSecretStaysRandom(t *testing.T) {
	s := &Server{apiKeys: newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))}
	rec := httptest.NewRecorder()
	s.adminKeys(rec, httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(`{"name":"plain"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Key) != 69 || !strings.HasPrefix(payload.Key, "m365_") {
		t.Fatalf("key=%q, want the unchanged random shape", payload.Key)
	}
}