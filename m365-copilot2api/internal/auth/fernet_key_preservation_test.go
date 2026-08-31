package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadOrCreateFernetKey 曾经是 if err == nil { return key }，失败就往下走生成并覆写。
//
// 于是「首次运行」和「我读不到已有密钥」被当成同一件事：权限被改、瞬时 I/O 错误、内容
// 损坏、长度不对，都会用一把新密钥覆盖旧的。凡是用旧密钥加密过的凭据从那一刻起永久
// 不可解，而且过程是静默的 —— 调用方只看到「成功拿到密钥」。

// isolateStoreKey 把密钥文件钉在临时目录里。
//
// 必须显式设置 M365_STORE_KEY_FILE：storeKeyPath 在 os.UserHomeDir() 成功时会完全
// 忽略传入的 storePath，固定返回 ~/.config/m365-store.key。只传 t.TempDir() 的
// storePath 看着像隔离，其实不是 —— 我就是这样用测试覆写了真实机器上的密钥文件，
// 让一份 5.4 MB 的加密账号库失去了唯一的钥匙。参数只是兜底，环境变量才是判据。
func isolateStoreKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "m365-store.key"))
	store := filepath.Join(dir, "accounts.json")
	// 自证隔离生效：解析出的路径必须落在这个临时目录里。
	if got := storeKeyPath(store); !strings.HasPrefix(got, dir) {
		t.Fatalf("key path %q escaped the temp dir %q — the test would write to a real key file", got, dir)
	}
	return store
}

func TestFirstRunCreatesAKey(t *testing.T) {
	store := isolateStoreKey(t)
	key, err := loadOrCreateFernetKey(store)
	if err != nil {
		t.Fatalf("first run must be able to create a key: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
	// 再调一次必须拿到同一把，而不是又生成一把新的。
	again, err := loadOrCreateFernetKey(store)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if string(again) != string(key) {
		t.Error("a second call generated a different key; existing credentials would be unreadable")
	}
}

func TestCorruptKeyFileIsNeverOverwritten(t *testing.T) {
	store := isolateStoreKey(t)
	keyPath := storeKeyPath(store)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := "this-is-not-a-valid-fernet-key"
	if err := os.WriteFile(keyPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadOrCreateFernetKey(store)
	if err == nil {
		t.Fatal("a corrupt key file must be fatal, not silently replaced")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error should say it refused to overwrite, got: %v", err)
	}

	// 决定性断言：文件内容必须一字未动。
	after, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("the key file disappeared: %v", readErr)
	}
	if strings.TrimSpace(string(after)) != corrupt {
		t.Fatalf("the key file was rewritten. before=%q after=%q — every credential "+
			"encrypted with the original key is now unrecoverable", corrupt, strings.TrimSpace(string(after)))
	}
}

// 长度不对的 base64 同样是「读不到已有密钥」，不是首次运行。
func TestWrongLengthKeyFileIsNeverOverwritten(t *testing.T) {
	store := isolateStoreKey(t)
	keyPath := storeKeyPath(store)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// 合法 base64，但解出来只有 8 字节。
	short := "AAAAAAAAAAA="
	if err := os.WriteFile(keyPath, []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateFernetKey(store); err == nil {
		t.Fatal("a short key must be fatal, not replaced")
	}
	after, _ := os.ReadFile(keyPath)
	if strings.TrimSpace(string(after)) != short {
		t.Error("the key file was rewritten despite being present")
	}
}
