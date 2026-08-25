package main

import (
	"context"
	"errors"
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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