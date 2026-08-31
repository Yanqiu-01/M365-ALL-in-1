package auth

import (
	"errors"
	"path/filepath"
	"testing"
)

// openTestVault 打开一个完全隔离在临时目录里的凭据库。
//
// 必须先 isolateStoreKey：OpenCredentialVault 会调 loadOrCreateFernetKey，而它在
// M365_STORE_KEY_FILE 未设置时会去写真实家目录下的 m365-store.key。
func openTestVault(t *testing.T) *CredentialVault {
	t.Helper()
	store := isolateStoreKey(t)
	v, err := OpenCredentialVault(filepath.Join(filepath.Dir(store), "credential-vault.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepathInside(v.Path(), filepath.Dir(store)) {
		t.Fatalf("凭据库路径 %q 逃出了临时目录 %q", v.Path(), filepath.Dir(store))
	}
	return v
}

func filepathInside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && rel[0] != '.'
}

// Put 必须认得 Get 认得的每一个地址。
//
// Get 一直接受账号 ID 或邮箱两种地址（调用方手里拿到哪个就用哪个），而 Put 只按
// 账号 ID 匹配。于是同一个地址在两个操作里指向不同的东西：用 Get 能解析到的邮箱去
// Put，不会替换那条记录，而是追加了第二条；Get 仍然返回先找到的旧记录，更新就这样
// 静默消失了。
func TestVaultPutResolvesTheSameAddressAsGet(t *testing.T) {
	v := openTestVault(t)

	if err := v.Put("oid-1", "a@example.com", "first"); err != nil {
		t.Fatal(err)
	}
	// Get 用邮箱能解析到它 —— 这就是「Get 认得的地址」。
	if got, err := v.Get("a@example.com"); err != nil || got != "first" {
		t.Fatalf("Get(邮箱)=%q err=%v want first", got, err)
	}

	// 用同一个地址更新密码。
	if err := v.Put("a@example.com", "", "second"); err != nil {
		t.Fatal(err)
	}

	if got, err := v.Get("a@example.com"); err != nil || got != "second" {
		t.Errorf("Get(邮箱)=%q err=%v want second："+
			"用 Get 认得的地址做的更新对 Get 不可见", got, err)
	}
	if got, err := v.Get("oid-1"); err != nil || got != "second" {
		t.Errorf("Get(账号ID)=%q err=%v want second：同一条记录的两个地址读出了不同的密码", got, err)
	}
	if list := v.List(); len(list) != 1 {
		t.Errorf("记录数=%d want 1，Put 追加了重复记录：%+v", len(list), list)
	}
}

// Delete 同样必须认得 Get 认得的地址。
func TestVaultDeleteResolvesEmailAlias(t *testing.T) {
	v := openTestVault(t)
	if err := v.Put("oid-2", "b@example.com", "pw"); err != nil {
		t.Fatal(err)
	}

	removed, err := v.Delete("b@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Delete(邮箱) 报告没有可删除的记录，而 Get(邮箱) 能看到它")
	}
	if _, err := v.Get("b@example.com"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("删除后 Get(邮箱) err=%v want ErrCredentialNotFound", err)
	}
	if _, err := v.Get("oid-2"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("删除后 Get(账号ID) err=%v want ErrCredentialNotFound", err)
	}
	if list := v.List(); len(list) != 0 {
		t.Errorf("记录数=%d want 0：%+v", len(list), list)
	}
}

// 三个操作对同一个地址的解析结果必须一致，落盘后重新打开也一样。
func TestVaultAddressResolutionSurvivesReopen(t *testing.T) {
	store := isolateStoreKey(t)
	path := filepath.Join(filepath.Dir(store), "credential-vault.json")

	v, err := OpenCredentialVault(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Put("oid-3", "c@example.com", "first"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("c@example.com", "", "second"); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCredentialVault(path)
	if err != nil {
		t.Fatal(err)
	}
	if list := reopened.List(); len(list) != 1 {
		t.Fatalf("重新打开后记录数=%d want 1：%+v", len(list), list)
	}
	for _, address := range []string{"oid-3", "c@example.com"} {
		if got, err := reopened.Get(address); err != nil || got != "second" {
			t.Errorf("重新打开后 Get(%q)=%q err=%v want second", address, got, err)
		}
	}
	// 邮箱字段不能因为「用邮箱当 accountID 更新」而丢掉。
	if meta := reopened.List(); meta[0].AccountID != "oid-3" || meta[0].Email != "c@example.com" {
		t.Errorf("记录标识被改写：%+v", meta[0])
	}
}

// 账号 ID 精确匹配优先于邮箱别名，结果不依赖记录顺序。
func TestVaultPrefersExactAccountIDOverEmailAlias(t *testing.T) {
	v := openTestVault(t)
	// 第一条记录的邮箱恰好等于第二条记录的账号 ID —— 两种地址在这里冲突。
	if err := v.Put("oid-a", "shared-key", "belongs-to-a"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("shared-key", "", "belongs-to-b"); err != nil {
		t.Fatal(err)
	}

	// "shared-key" 既是第一条的邮箱、又是第二条的账号 ID：必须解析到账号 ID 那条。
	// 注意第一次 Put 之后 "shared-key" 只匹配到第一条，所以第二次 Put 更新的是它 ——
	// 这正是别名语义要求的结果，两条记录不会分裂。
	if list := v.List(); len(list) != 1 {
		t.Fatalf("记录数=%d want 1：%+v", len(list), list)
	}
	got, err := v.Get("shared-key")
	if err != nil {
		t.Fatal(err)
	}
	if got != "belongs-to-b" {
		t.Errorf("Get(shared-key)=%q want belongs-to-b", got)
	}
	if got, err := v.Get("oid-a"); err != nil || got != "belongs-to-b" {
		t.Errorf("Get(oid-a)=%q err=%v want belongs-to-b（同一条记录）", got, err)
	}
}

// findCredentialLocked 在两条记录分别按 ID 和邮箱命中同一个 key 时，必须选 ID 那条。
func TestFindCredentialPrefersAccountIDMatch(t *testing.T) {
	v := openTestVault(t)
	// 直接构造冲突：第一条的邮箱 == 第二条的账号 ID。
	v.body.Credentials = []credentialEntry{
		{AccountID: "oid-x", Email: "collide"},
		{AccountID: "collide", Email: "y@example.com"},
	}
	if got := v.findCredentialLocked("collide"); got != 1 {
		t.Errorf("findCredentialLocked=%d want 1（账号 ID 精确匹配应优先于邮箱别名）", got)
	}
	if got := v.findCredentialLocked("y@example.com"); got != 1 {
		t.Errorf("findCredentialLocked(邮箱)=%d want 1", got)
	}
	if got := v.findCredentialLocked("no-such"); got != -1 {
		t.Errorf("findCredentialLocked(不存在)=%d want -1", got)
	}
}
