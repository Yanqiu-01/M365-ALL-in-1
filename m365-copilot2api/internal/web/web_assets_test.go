package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// web/ 下 APK 原有的三个页面逐字节取自 APK assets/web/。
// APK 中不存在 conversation.html —— rodata 只有 "web/index.html"，
// 且 rootPage 的路径判定只覆盖 / 与 /login。
// workbench.html 是 2026-08-26 按用户要求新增的「仅聊天」工作台，
// 属有意的功能扩展，不属于 APK 复原集，因此单独校验而非并入 want。
func TestWebAssetsMatchAPKSet(t *testing.T) {
	entries, err := os.ReadDir("../../web")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)

	want := []string{"debug.html", "index.html", "login.html"}
	extraHTML := []string{"panel.html", "workbench.html"} // 有意新增，见函数注释
	extraAssets := []string{"favicon.ico"}                // 有意新增：站点图标（壁纸编号05生成）
	allowed := append(append([]string{}, want...), extraHTML...)
	allowed = append(allowed, extraAssets...)
	sort.Strings(allowed)
	if strings.Join(got, ",") != strings.Join(allowed, ",") {
		t.Errorf("web/ 内容为 %v，期望 %v（APK 三件 + 有意新增的 workbench/favicon）", got, allowed)
	}

	// 逐个确认非空且是 HTML 文档。
	htmlDocs := append(append([]string{}, want...), extraHTML...)
	for _, name := range htmlDocs {
		body, err := os.ReadFile(filepath.Join("../../web", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Errorf("%s 为空", name)
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(body))), "<!doctype html") {
			t.Errorf("%s 不是 HTML 文档", name)
		}
	}
}

// index.html 是中文界面：APK 里没有任何 locale 切换机制
// （不存在 localeSelectionKey / preferredLocale 等标识符），
// 语言直接由 <html lang> 声明。
func TestWebIndexIsChineseWithoutLocaleSwitcher(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	if !strings.Contains(page, `<html lang="zh-CN">`) {
		t.Error(`index.html 应声明 <html lang="zh-CN">`)
	}
	// 这些标识符曾被断言存在，实测 APK 中一个都没有。
	for _, absent := range []string{
		"localeSelectionKey",
		"preferredLocale",
		"m365_locale_selected",
	} {
		if strings.Contains(page, absent) {
			t.Errorf("index.html 出现了 APK 中不存在的 locale 机制标识符 %q", absent)
		}
	}
}

// 前端调用的每个 /api 端点都必须有服务端路由，否则页面功能会静默失效。
// 这是比「检查某个字符串存在」更有价值的契约断言。
func TestWebIndexAPIEndpointsAreRouted(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}

	pattern := regexp.MustCompile(`'(/api/[A-Za-z0-9/_-]+)'`)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
		seen[match[1]] = true
	}
	if len(seen) < 15 {
		t.Fatalf("只解析出 %d 个端点，正则可能失效", len(seen))
	}

	// Routes() 返回 http.Handler，无法直接查询注册表；改为发请求观察
	// 是否命中 404。未注册的路径由 ServeMux 返回 404 且响应体为
	// "404 page not found"，据此与业务型 404 区分。
	server := &Server{}
	routes := server.Routes()
	endpoints := make([]string, 0, len(seen))
	for endpoint := range seen {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)

	for _, endpoint := range endpoints {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, endpoint, nil))
		if recorder.Code == http.StatusNotFound &&
			strings.Contains(recorder.Body.String(), "404 page not found") {
			t.Errorf("前端调用 %s，但服务端未注册该路由", endpoint)
		}
	}
}

// panel.html 与 index.html 受同一条契约约束。它此前只进了资源白名单和「是不是
// HTML 文档」这两项检查，7 个 /api 调用一个都没被覆盖 —— 而它恰恰是驱动账号注册
// 与 OAuth 导入的页面，任一路由缺失都会让功能静默失效。
func TestWebPanelAPIEndpointsAreRouted(t *testing.T) {
	body, err := os.ReadFile("../../web/panel.html")
	if err != nil {
		t.Fatal(err)
	}

	pattern := regexp.MustCompile(`'(/api/[A-Za-z0-9/_-]+)'`)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
		seen[match[1]] = true
	}
	// panel.html 实测调用 7 个端点。下界取 5 是为了在页面增删调用时不误报，同时
	// 仍能在正则失效（匹配数骤降为 0）时立刻失败。
	if len(seen) < 5 {
		t.Fatalf("只解析出 %d 个端点，正则可能失效", len(seen))
	}

	server := &Server{}
	routes := server.Routes()
	endpoints := make([]string, 0, len(seen))
	for endpoint := range seen {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)

	for _, endpoint := range endpoints {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, endpoint, nil))
		if recorder.Code == http.StatusNotFound &&
			strings.Contains(recorder.Body.String(), "404 page not found") {
			t.Errorf("panel.html 调用 %s，但服务端未注册该路由", endpoint)
		}
	}
}

// rootPage 只服务 / 与 /login；/conversation 必须 404。
func TestRootPageServesOnlyAPKPages(t *testing.T) {
	server := &Server{}

	for _, path := range []string{"/", "/login"} {
		recorder := httptest.NewRecorder()
		server.rootPage(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		// 工作目录在 internal/web，读不到 web/*.html，会返回 500；
		// 关键是不能 404 —— 说明路径被接受了。
		if recorder.Code == http.StatusNotFound {
			t.Errorf("%s 应被 rootPage 接受，却返回 404", path)
		}
	}

	// APK 中不存在 conversation.html，该路径必须 404。
	recorder := httptest.NewRecorder()
	server.rootPage(recorder, httptest.NewRequest(http.MethodGet, "/conversation", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("/conversation 应返回 404（APK 无此页面），实际 %d", recorder.Code)
	}

	// 方法限制：APK rootPage 只判 GET 与 HEAD。
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		recorder := httptest.NewRecorder()
		server.rootPage(recorder, httptest.NewRequest(method, "/", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / 应返回 405，实际 %d", method, recorder.Code)
		}
	}
}
