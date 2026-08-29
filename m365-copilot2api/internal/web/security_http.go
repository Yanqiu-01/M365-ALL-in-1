package web

import (
	"net/http"
	"os"
)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; script-src 'self' 'unsafe-inline' https://unpkg.com https://cdn.jsdelivr.net")
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// rootPage 只提供两个页面：/ 与 /login。
//
// APK 证据（tools/apktool，security_http.go:22-45，848 字节）：
//   - +0x0054 CMP #47 判单字符 '/'，+0x0064 CMP #6 配整数化比较 "/login"；
//   - +0x00d4 CMP #3 与 +0x00f8 CMP #4 分别判方法 "GET" / "HEAD"；
//   - rodata 中只存在 "web/index.html"（@0x4d4181），
//     不存在 "web/conversation.html" 或 "conversation.html"。
//
// 此前本地多出的 /conversation 分支属虚构：APK 的 assets/web 仅有
// index.html / login.html / debug.html 三个文件。
//
// /workbench 是唯一的例外，且不是复原产物：见下方 case 处的说明。
func (s *Server) rootPage(w http.ResponseWriter, r *http.Request) {
	// /favicon.ico 由用户指定的壁纸（琉璃神社壁纸包 2025年11月号 编号05）生成，
	// 与 APK 原始行为无关，属有意扩展。Content-Type 必须显式声明，否则浏览器
	// 不会把响应识别为图标。
	if r.URL.Path == "/favicon.ico" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		f, err := os.Open("web/favicon.ico")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/x-icon")
		http.ServeContent(w, r, "favicon.ico", st.ModTime(), f)
		return
	}
	var name string
	switch r.URL.Path {
	case "/", "/login":
		// 原 APK 实测 /login 与 / 返回同一份 index.html（逐字节相同，102579
		// 字节）：登录态由前端 JS 依据 /api/admin/session 切换，没有独立的
		// 登录页路由。上游那句 name = "login.html" 的分支在二开版里已被删除，
		// 恢复时误将其带回，导致 /login 只返回 10611 字节的空壳页面。
		name = "web/index.html"
	case "/workbench":
		// 本次按用户明确要求新增的「仅聊天」前端工作台,与 APK 原始行为无关:
		// APK rodata 只有 "web/index.html",不存在 workbench.html,原版 GET
		// /workbench 应为 404。此分支属于有意的功能扩展,不是上文所述那类凭空
		// 复原出的虚构路由,请勿按「APK 无此路径」为由直接删除。
		name = "web/workbench.html"
	case "/panel":
		// 一体化控制面板入口。账号注册、OAuth 授权导入、网关状态和任务日志
		// 共用 4141 服务及 /api/admin/panel/* 原生接口,不再依赖独立的 8555 端口。
		name = "web/panel.html"
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	f, err := os.Open(name)
	if err != nil {
		http.Error(w, "web interface unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "web interface unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
