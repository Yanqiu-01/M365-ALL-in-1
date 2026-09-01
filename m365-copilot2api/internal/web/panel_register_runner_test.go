package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/exitrotate"
)

// stubTunnel 让测试不去执行真实的 adb —— 测试机上不一定插着手机。
func stubTunnel(t *testing.T, ip string, err error) {
	t.Helper()
	old := ensureTunnel
	t.Cleanup(func() { ensureTunnel = old })
	ensureTunnel = func(context.Context, exitrotate.TunnelRequest) (exitrotate.TunnelResult, error) {
		if err != nil {
			return exitrotate.TunnelResult{Detail: err.Error()}, err
		}
		return exitrotate.TunnelResult{OK: true, IP: ip}, nil
	}
}

// writeRunnerPanelConfig 在 writePanelConfig 之上把求解器钉成 FlareSolverr，并指向一个
// 本地桩。
//
// 长跑任务不像单次注册那样能由调用方递一个 token 进来，它走的是完整的求解流程。默认
// （auto）在装了 Chrome/Edge 的机器上会真的开一个浏览器去访问站点地址 —— 测试会因此变慢、
// 变得依赖机器上装了什么，还会真的弹窗。
func writeRunnerPanelConfig(t *testing.T, root, siteURL string) {
	t.Helper()
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"solution": map[string]any{
				"response": `<input name="cf-turnstile-response" value="flare-solved-token-abcdefghij">`,
			},
		})
	}))
	t.Cleanup(flare.Close)

	writePanelConfig(t, root, siteURL)
	cfgPath := filepath.Join(root, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	reg := cfg["register"].(map[string]any)
	reg["solver"] = "flaresolverr"
	reg["flaresolverr_url"] = flare.URL
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForJob(t *testing.T, server *Server, want func(registerJobState) bool) registerJobState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state := server.registerJob().snapshot()
		if want(state) {
			return state
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("任务没有走到预期状态：%#v", server.registerJob().snapshot())
	return registerJobState{}
}

// 长跑任务要真的把整个号段走完，而不是只跑第一批就收工。
func TestRegisterJobWalksEveryBatchToTarget(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	var registered int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&registered, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)

	server := &Server{}
	state, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 5031, BatchSize: 2, SkipOAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running {
		t.Fatalf("启动后应当报告 running：%#v", state)
	}

	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	if final.Success != 6 {
		t.Fatalf("success = %d，想要 6（5026..5031）", final.Success)
	}
	if final.Batches != 3 {
		t.Fatalf("batches = %d，每批 2 个应当是 3 批", final.Batches)
	}
	if final.NextNum != 5032 {
		t.Fatalf("nextNum = %d，跑完应当停在 5032", final.NextNum)
	}
	if final.Detail != "已完成" {
		t.Fatalf("detail = %q，想要 已完成", final.Detail)
	}
	if got := atomic.LoadInt64(&registered); got != 6 {
		t.Fatalf("站点收到 %d 次注册，想要 6 次", got)
	}
}

// 一次只允许一个任务：站点按 IP 限流、手机只有一个出口，两个任务并发只会互相抢同一个
// IP 的当日额度。
func TestRegisterJobRefusesASecondConcurrentJob(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	release := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	defer close(release)
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)
	manager := newNativePanelManager(nativePanelConfig{Root: root})

	server := &Server{}
	if _, err := server.startRegisterJob(manager, registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 5040, BatchSize: 2, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, server, func(st registerJobState) bool { return st.Running })

	if _, err := server.startRegisterJob(manager, registerJobRequest{
		Mode: "phone", StartNum: 6000, Target: 6010, SkipOAuth: true,
	}); err == nil {
		t.Fatal("已经有任务在跑，第二个任务却被接受了")
	}
	server.registerJob().stop()
}

// stop 要真的让任务退出，而且要如实回报有没有任务被停掉。
func TestRegisterJobStopIsHonest(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)

	server := &Server{}
	if stopped := server.registerJob().stop(); stopped {
		t.Fatal("没有任务在跑，stop 却报告停掉了一个")
	}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 9000, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, server, func(st registerJobState) bool { return st.Running })
	if stopped := server.registerJob().stop(); !stopped {
		t.Fatal("有任务在跑，stop 却报告没有")
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	if final.Detail != "已停止" {
		t.Fatalf("detail = %q，想要 已停止", final.Detail)
	}
	if final.NextNum >= 9000 {
		t.Fatalf("stop 没生效，号段被跑完了：nextNum = %d", final.NextNum)
	}
}

// 隧道修不好就不该假装在注册：任务要停下并说清原因，而不是一批批地把号段白划过去。
func TestRegisterJobStopsWhenPhoneTunnelNeverComesBack(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "", errors.New("手机没插"))
	var hits int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)

	server := &Server{}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 5030, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 隧道重试间隔是分钟级的，这里只确认它没有绕过隧道直接去注册。
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("隧道不通却注册了 %d 个号", got)
	}
	state := server.registerJob().snapshot()
	if state.Detail != "等手机隧道恢复" {
		t.Fatalf("detail = %q，想要 等手机隧道恢复", state.Detail)
	}
	server.registerJob().stop()
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
}

func TestStartRegisterJobRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	writeRunnerPanelConfig(t, root, site.URL)
	manager := newNativePanelManager(nativePanelConfig{Root: root})
	server := &Server{}

	for name, request := range map[string]registerJobRequest{
		"未知模式":       {Mode: "carrier-pigeon", StartNum: 1, Target: 2},
		"startNum 为 0": {Mode: "phone", StartNum: 0, Target: 10},
		"target 小于起点": {Mode: "phone", StartNum: 100, Target: 99},
	} {
		if _, err := server.startRegisterJob(manager, request); err == nil {
			t.Fatalf("%s 被接受了", name)
		}
	}
	if state := server.registerJob().snapshot(); state.Running {
		t.Fatal("入参被拒之后仍然留下了一个 running 的任务")
	}
}

// 每批上限跟单次注册接口一致：给一个更大的值要被压回去，而不是原样送进 runRegister
// 再由它报错。
func TestStartRegisterJobClampsBatchSize(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "", errors.New("手机没插"))
	root := t.TempDir()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	writeRunnerPanelConfig(t, root, site.URL)

	server := &Server{}
	state, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 9000, BatchSize: 500, SkipOAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.BatchSize != panelRegisterMax {
		t.Fatalf("batchSize = %d，想要压到 %d", state.BatchSize, panelRegisterMax)
	}
	server.registerJob().stop()
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
}
