package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openAPIKeys 曾经是 if e == nil && json.Unmarshal(b, s) == nil { ... }：读失败或解析
// 失败就跳过整块，返回一个空存储。之后任何路径调用 flush，磁盘上那份含全部 key 的文件
// 就被一个空对象覆盖 —— 静默且不可恢复。一次权限错误或半截写入的 JSON 足以清空所有
// API key。
//
// 另外 Path 字段没有 json tag，会被序列化进 api-keys.json 再读回来，等于让文件内容指定
// 自己的存放位置。

func TestCorruptAPIKeyFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-keys.json")
	corrupt := `{"keys":[{"hash":"abc",` // 半截 JSON，模拟写入中断
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)

	s := openAPIKeys()
	if s == nil {
		t.Fatal("openAPIKeys returned nil")
	}
	if s.loadErr == nil {
		t.Error("a corrupt file must be recorded as a load failure")
	}

	// 决定性断言：flush 必须拒绝，且文件内容一字未动。
	if err := s.flush(); err == nil {
		t.Error("flush must refuse to write a store that failed to load")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the key file disappeared: %v", err)
	}
	if string(after) != corrupt {
		t.Fatalf("the key file was overwritten. before=%q after=%q — every configured key is gone",
			corrupt, string(after))
	}
}

// 文件不存在是首次运行，空存储正确，且必须允许落盘 —— 否则永远建不出第一把 key。
func TestFirstRunAPIKeyStoreCanWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-keys.json")
	t.Setenv("M365_API_KEYS", path)

	s := openAPIKeys()
	if s.loadErr != nil {
		t.Fatalf("a missing file is first run, not a load failure: %v", s.loadErr)
	}
	if err := s.flush(); err != nil {
		t.Fatalf("first run must be able to persist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("flush reported success but no file exists: %v", err)
	}
}

// Path 不得出现在落盘内容里。
func TestAPIKeyStorePathIsNotSerialised(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-keys.json")
	t.Setenv("M365_API_KEYS", path)

	s := openAPIKeys()
	if err := s.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Path") || strings.Contains(string(raw), "api-keys.json\"") {
		t.Errorf("the store path was serialised into the file, letting its contents choose "+
			"where it lives:\n%s", string(raw))
	}
}
