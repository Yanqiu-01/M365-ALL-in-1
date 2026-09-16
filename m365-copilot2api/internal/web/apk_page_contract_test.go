package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 原 APK 实测（QEMU 中运行 base.apk 提取的 libm365.so）：
//
//	GET /            -> 200, index.html, 102579 bytes
//	GET /login       -> 200, 与 / 逐字节相同
//	GET /conversation-> 404
//	GET /debug       -> 404
//
// 登录态由前端 JS 依据 /api/admin/session 切换，没有独立登录页路由。
// 上游 rootPage 里的 name = "login.html" 分支在二开版中已被删除；恢复过程
// 曾把它带回，使 /login 返回 10611 字节的空壳页，前端上下文随之丢失。
func TestRootPageServesIndexForLoginAsOriginalAPK(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte("<!doctype html><html><body>index</body></html>")
	if err := os.WriteFile(filepath.Join(dir, "web", "index.html"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	// 若实现回退到服务 login.html，内容不同即可被检出。
	if err := os.WriteFile(filepath.Join(dir, "web", "login.html"), []byte("<!doctype html><html><body>LOGIN-SHELL</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	s := &Server{}
	body := map[string]string{}
	for _, path := range []string{"/", "/login"} {
		rec := httptest.NewRecorder()
		s.rootPage(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, rec.Code)
		}
		body[path] = rec.Body.String()
	}
	if body["/"] != body["/login"] {
		t.Errorf("/login must serve the same index.html as / (original APK behaviour)\n  / =%q\n  /login=%q", body["/"], body["/login"])
	}
	if body["/login"] != string(index) {
		t.Errorf("/login served %q, want index.html content", body["/login"])
	}

	for _, path := range []string{"/conversation", "/debug", "/nope"} {
		rec := httptest.NewRecorder()
		s.rootPage(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status=%d want 404 (absent from the original APK routes)", path, rec.Code)
		}
	}
}

func TestWebAssetPathPrefersExecutableDirectoryOverProcessCwd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	webDir := filepath.Join(filepath.Dir(exe), "web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("from-exe")
	path := filepath.Join(webDir, "index.html")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "web", "index.html"), []byte("from-cwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	got := webAssetPath("index.html")
	if got != path {
		t.Fatalf("webAssetPath= %q, want executable-adjacent %q", got, path)
	}
}
