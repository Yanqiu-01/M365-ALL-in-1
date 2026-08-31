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

// isolateStoreKey 把密钥文件、账号库和凭据库一起钉在临时目录里，并返回账号库路径。
//
// 必须显式设置 M365_STORE_KEY_FILE：storeKeyPath 在 os.UserHomeDir() 成功时会完全
// 忽略传入的 storePath，固定返回 ~/.config/m365-store.key。只传 t.TempDir() 的
// storePath 看着像隔离，其实不是 —— 我就是这样用测试覆写了真实机器上的密钥文件，
// 让一份 5.4 MB 的加密账号库失去了唯一的钥匙。参数只是兜底，环境变量才是判据。
//
// 密钥路径和数据目录是一对，缺一不可。只钉密钥路径不够：CachePath() 在
// M365_DATA_DIR 未设置时同样落在真实家目录，而真实的 accounts.json 是用真实密钥
// 加密的，配上一把临时新密钥就解不开 —— 测试会以「读不到密钥」失败，而它其实正在
// 读用户的真实数据。CredentialVaultPath() 同理。
func isolateStoreKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(dir, "m365-store.key"))
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_CREDENTIAL_VAULT", filepath.Join(dir, "credential-vault.json"))
	store := filepath.Join(dir, "accounts.json")
	// 自证隔离生效：三条路径都必须落在这个临时目录里，而不是假定它们会。
	for name, got := range map[string]string{
		"storeKeyPath":        storeKeyPath(store),
		"CachePath":           CachePath(),
		"CredentialVaultPath": CredentialVaultPath(),
	} {
		if !strings.HasPrefix(got, dir) {
			t.Fatalf("%s 解析为 %q，逃出了临时目录 %q —— 测试会写到真实文件上", name, got, dir)
		}
	}
	return store
}

// 隔离助手自身的自检。它是本包每个碰到密钥的测试的唯一防线，必须自己也被测到。
func TestIsolateStoreKeyKeepsEveryPathInTempDir(t *testing.T) {
	store := isolateStoreKey(t)
	dir := filepath.Dir(store)
	for name, got := range map[string]string{
		"storeKeyPath":        storeKeyPath(store),
		"CachePath":           CachePath(),
		"CredentialVaultPath": CredentialVaultPath(),
	} {
		if !strings.HasPrefix(got, dir) {
			t.Errorf("%s=%q 不在 %q 内", name, got, dir)
		}
	}
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
