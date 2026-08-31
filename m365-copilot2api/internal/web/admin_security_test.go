package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultPasswordForcesChangeAndRotatesSessions(t *testing.T) {
	// New() 会打开加密账号存储，而密钥路径默认落在真实家目录里，与本测试的 TempDir
	// 无关。不隔离就会读写机器上真实的 m365-store.key。
	isolateStoreKey(t)
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())

	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"admin123"}`)
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	var login map[string]any
	_ = json.NewDecoder(r.Body).Decode(&login)
	r.Body.Close()
	if login["must_change_password"] != true {
		t.Fatalf("login=%#v", login)
	}

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("protected status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"admin123","new_password":"a-new-password-123"}`)
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"a-new-password-123"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new login=%d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new session status=%d", r.StatusCode)
	}
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	// 同一类隔离，另一个来源：New() 打开的加密存储，其密钥路径与账号库路径默认都落在
	// 真实家目录，与本测试的 TempDir 无关。下面对管理口令的隔离已经很细致，却漏了这
	// 一项。
	//
	// 这里不能用 isolateStoreKey：它会设 M365_DATA_DIR，而本测试下面刻意把该变量清空
	// 以验证管理口令的取值优先级。所以只钉密钥与账号库这两条路径，让 M365_DATA_DIR
	// 保持由本测试自己支配。
	storeDir := t.TempDir()
	t.Setenv("M365_STORE_KEY_FILE", filepath.Join(storeDir, "m365-store.key"))
	// CachePath 的优先级是 M365_DATA_DIR → M365_CONFIG → M365_TOKEN_CACHE →
	// M365_TOKEN_FILE → 家目录。M365_DATA_DIR 归本测试自己支配，所以用次优先的
	// M365_CONFIG 把账号库钉进临时目录。
	t.Setenv("M365_CONFIG", filepath.Join(storeDir, "accounts.json"))
	// 环境隔离。loadAdminPassword 的取值优先级是
	// M365_DATA_DIR/admin-password → M365_ADMIN_PASSWORD_FILE →
	// M365_ADMIN_PASSWORD_BOOTSTRAP_FILE → M365_ADMIN_PASSWORD。此前本测试只设了
	// 最后一项，于是在任何存在真实 ~/.config/m365-copilot2api/admin-password 的机器
	// 上，它读到的都是那份持久化口令 —— "correct-password" 反而成了错误口令，断言
	// 测到的就不是它以为在测的东西（实测表现为最后一步拿到 401）。把优先级更高的三
	// 个来源全部指向空值或临时路径，本测试才真正只依赖 M365_ADMIN_PASSWORD。
	t.Setenv("M365_DATA_DIR", "")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD", "correct-password")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	for i := 0; i < 5; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("attempt %d=%d", i+1, r.StatusCode)
		}
	}
	// 以下是有意的行为变更。本测试此前断言"锁定期内连正确口令也得 429"，那条期望把
	// 一个真实缺陷固化成了规范：它使暴力破解防护变成拒绝服务放大器 —— 一个持续用
	// 错误口令重试的客户端（实测 640+ 次连续失败，服务重启清空内存计数后几秒内又被
	// 打满）可以让持有正确口令的合法管理员永久无法登录，因为请求在口令校验之前就被
	// 429 挡掉了。现在锁定只作用于失败的尝试。
	//
	// httptest 客户端来自 127.0.0.1，因此适用放宽后的回环预算
	// （loopbackLoginFailureThreshold 次 / loopbackLoginLockoutWindow），而不是非回环
	// 的 5 次 / 15 分钟；所以第 6 次失败仍是 401。回环虽然放宽但依然有上限，下面断言
	// 锁定恰好发生在该上限上。
	lockedAt, retryAfter := 0, ""
	for i := 6; i <= loopbackLoginFailureThreshold; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		status, retry := r.StatusCode, r.Header.Get("Retry-After")
		r.Body.Close()
		if status == 429 {
			lockedAt, retryAfter = i, retry
			break
		}
		if status != 401 {
			t.Fatalf("attempt %d=%d, want 401 before the loopback cap", i, status)
		}
	}
	// 锁定确实生效：暴力破解在回环上限处仍然被抑制。
	if lockedAt != loopbackLoginFailureThreshold {
		t.Fatalf("locked at attempt %d, want cap at %d", lockedAt, loopbackLoginFailureThreshold)
	}
	if retryAfter == "" {
		t.Fatal("locked response missing Retry-After")
	}
	// 合法管理员不被连带锁死：锁定期内正确口令仍然登录成功并拿到会话。
	good := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"correct-password"}`)
	goodStatus := good.StatusCode
	session := ""
	for _, ck := range good.Cookies() {
		if ck.Name == "m365_admin_session" {
			session = ck.Value
		}
	}
	good.Body.Close()
	if goodStatus != 200 {
		t.Fatalf("correct password during lockout=%d, want 200", goodStatus)
	}
	if session == "" {
		t.Fatal("successful login issued no session cookie")
	}
	// 成功登录立即清零该来源的失败计数：紧接着的错误口令回到 401 而不是 429。
	after := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
	afterStatus := after.StatusCode
	after.Body.Close()
	if afterStatus != 401 {
		t.Fatalf("wrong password after successful login=%d, want 401 (failure counter cleared)", afterStatus)
	}
}

func TestPersistedPasswordOverridesBootstrapEnv(t *testing.T) {
	path := t.TempDir() + "/admin-password"
	t.Setenv("M365_ADMIN_PASSWORD_FILE", path)
	t.Setenv("M365_ADMIN_PASSWORD", "old-bootstrap-password")
	if err := saveAdminPassword("persisted-new-password"); err != nil {
		t.Fatal(err)
	}
	got, mustChange := loadAdminPassword()
	if got != "persisted-new-password" || mustChange {
		t.Fatalf("got=%q mustChange=%v", got, mustChange)
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"x": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("x", time.Now()); !ok {
		t.Fatal("expired window remained locked")
	}
}
