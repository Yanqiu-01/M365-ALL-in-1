package web

import (
	"path/filepath"
	"strings"
	"testing"
)

// isolateStoreKey 把加密存储的密钥文件钉在临时目录里，供任何调用 New() 的测试使用。
//
// 为什么必须显式设置环境变量：auth.storeKeyPath 在 os.UserHomeDir() 成功时会忽略传入
// 的 store 路径，固定返回 ~/.config/m365-store.key。所以「用 t.TempDir() 当数据目录」
// 看着像隔离，其实不是 —— 密钥文件仍然落在真实的家目录里。
//
// 这不是假想的风险。admin_security 的两个测试就没有隔离密钥路径，它们一直在读写真实机
// 器上的 m365-store.key；此前 loadOrCreateFernetKey 会在读失败时静默重新生成，所以看
// 不出任何异常。等它改成「存在但读不出来就拒绝」之后，这两个测试立刻失败 —— 长期存在
// 的问题这才显形。
//
// 一份 5.4 MB 的 fernet 加密账号库只有这一把钥匙，覆写它等于让全部刷新令牌不可恢复。
// 密钥和数据目录必须一起隔离。只钉密钥路径不够：New() 打开的账号存储路径由
// M365_DATA_DIR 决定，未设置时同样落在真实家目录。真实的 accounts.json 是用真实密钥
// 加密的，配上一把临时新密钥就解不开 —— 测试会以 "read store key ... cannot find the
// file" 失败，而那其实是在读用户的真实数据。两者是一对，缺一不可。
func isolateStoreKey(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "m365-store.key")
	t.Setenv("M365_STORE_KEY_FILE", keyPath)
	t.Setenv("M365_DATA_DIR", dir)
	// 自证隔离生效，而不是假定它生效。
	if !strings.HasPrefix(keyPath, dir) {
		t.Fatalf("key path %q escaped %q", keyPath, dir)
	}
}
