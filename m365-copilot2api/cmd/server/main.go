package main

import (
	"context"
	"errors"
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/turnstile"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	if f := openGatewayLog(); f != nil {
		defer f.Close()
		log.SetOutput(f)
	}
	web.ApplyStartupSettingsEnv()
	if err := outbound.ConfigureFromEnv(); err != nil {
		log.Fatalf("configure outbound proxy: %v", err)
	}
	s, e := web.New()
	if e != nil {
		log.Fatal(e)
	}
	s.InitM365CloudClient()
	s.StartAutoCleanup()
	// 启动时把面板账密清单补进加密 vault。账号由 Python 脚本导入时账密只落在
	// 文本清单里，vault 为空会让「账密一键回调」在留空密码时报「密码为空」。
	s.SyncCredentialsAtStartup()
	listen := "127.0.0.1:4141"
	if v := os.Getenv("M365_LISTEN"); v != "" {
		listen = v
	}
	log.Printf("m365-copilot2api listening on http://%s\\n", listen)
	server := &http.Server{
		Addr:              listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0, // streaming endpoints need an open-ended write window.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Background patrol over the egress pool. It owns no goroutine beyond ctx:
	// waitForProxyGuard blocks until the patrol has actually returned, so nothing
	// outlives main.
	waitForProxyGuard := outbound.StartProxyGuard(ctx)
	// 内建 FlareSolverr 自带一个 http.Server，不经过网关的任何中间件，也没有任何
	// 鉴权。因此监听地址只允许回环：把它绑到 0.0.0.0 会把一个无鉴权的求解接口
	// 直接摆到局域网上。非回环地址不静默接受，记一行日志后退回回环。
	// 内置求解器只在 Android 上有意义：它自己解不了 Turnstile，只是把任务通过
	// M365_DATA_DIR 下的协作目录转交给 App 内的 WebView。桌面端没有那个 WebView，
	// 它永远解不出 token，只会回一句「请打开修改版M365」——在 PC 上毫无意义。
	//
	// 更要紧的是 8191 正是真实 FlareSolverr（Docker/独立部署）的默认端口。让这个
	// 解不出结果的中继占着它，用户就再也起不了能用的求解器，而注册配置里的默认
	// 端点又恰好指向 8191。所以桌面端一律不启动它，把端口留给真家伙。
	turnstileDone := make(chan struct{})
	go func() {
		defer close(turnstileDone)
		if !turnstile.WebViewAvailable() {
			log.Printf("built-in FlareSolverr: not started on %s (it only relays to the Android in-app WebView); "+
				"leaving 127.0.0.1:8191 free for a real FlareSolverr", runtime.GOOS)
			return
		}
		addr := "127.0.0.1:8191"
		if v := strings.TrimSpace(os.Getenv("M365_FLARESOLVERR_LISTEN")); v != "" {
			if loopbackListenAddr(v) {
				addr = v
			} else {
				log.Printf("built-in FlareSolverr: refusing non-loopback listen %q (unauthenticated endpoint); using %s", v, addr)
			}
		}
		if err := turnstile.Listen(ctx, addr); err != nil {
			log.Printf("built-in FlareSolverr: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// The listener has returned, so stop the signal context and wait for the
	// patrol goroutine to finish before the process exits.
	stop()
	waitForProxyGuard()
	// 这个文件对其余后台工作者都保证「不会有东西比 main 活得更久」，内建
	// FlareSolverr 也要照此join，否则进程退出时它的监听器可能还没关，紧接着的
	// 重启会撞上「address already in use」。
	<-turnstileDone
	web.StopPersistLoop()
	log.Println("shutdown complete")
}

func openGatewayLog() *os.File {
	f, err := os.OpenFile(gatewayLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

func gatewayLogPath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(".", "m365-gateway.log")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "m365-gateway.log")
}
