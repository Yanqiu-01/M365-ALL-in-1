package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// patchRunnerRegisterConfig 往已经写好的 config.json 的 register 段里补几个键。
//
// 刻意直接改文件而不是走 saveRegisterConfig：要测的是「配置里有这个键时代码怎么用它」，
// 不该顺带依赖写入路径也正确。
func patchRunnerRegisterConfig(t *testing.T, root string, keys map[string]any) {
	t.Helper()
	cfgPath := filepath.Join(root, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	reg, ok := cfg["register"].(map[string]any)
	if !ok {
		reg = map[string]any{}
		cfg["register"] = reg
	}
	for key, value := range keys {
		reg[key] = value
	}
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// okSite 是一个总是回「注册成功」的站点桩。
func okSite(t *testing.T) *httptest.Server {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	t.Cleanup(site.Close)
	return site
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
	// 进行中的措辞要带重试进度：静态的一句「等手机隧道恢复」和放弃之后长得一样。
	if !strings.HasPrefix(state.Detail, "等手机隧道恢复（重试 ") {
		t.Fatalf("detail = %q，想要「等手机隧道恢复（重试 n/m）」", state.Detail)
	}
	if strings.Contains(state.Detail, "已停止") {
		t.Fatalf("detail = %q，还在重试却写了「已停止」", state.Detail)
	}
	if !state.Running {
		t.Fatalf("还在重试却报 running=false：%#v", state)
	}
	server.registerJob().stop()
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
}

// 隧道彻底不可用而任务已经返回时，面板必须能看出「不是还在重试，是停了」，并且要说清
// 从哪个号接着跑。
//
// 这两种处境原先共用一句「等手机隧道恢复」：还在重试是它，goroutine 已经返回也是它。
// 操作者盯着面板看不出区别，会以为任务还活着而一直等 —— 而站点每个 IP 一天只放行一次
// 注册，等掉的每一天都是白等的额度，号段就停在那里不动。
//
// 用「配置读不出来」来触发：ensurePhoneTunnel 开头的 panelData 失败会直接返回错误，
// 一次重试都不做，于是这条终止路径能在毫秒级走到，不用等满 30 分钟的重试预算。
// 顺带也覆盖了另一半：错误跟隧道无关时，措辞不能硬说是隧道在等恢复。
func TestRegisterJobTerminalTunnelFailureIsDistinguishableFromRetrying(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	manager := newNativePanelManager(nativePanelConfig{Root: root})

	const start = 5026
	server := &Server{}
	// 起跑之后再弄坏配置会撞上「第一批已经在跑」；显式给了 BatchSize 时 startRegisterJob
	// 不读配置，所以起跑前弄坏它不会被入参校验挡掉。
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{ 这不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.startRegisterJob(manager, registerJobRequest{
		Mode: "phone", StartNum: start, Target: 5030, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })

	if !strings.Contains(final.Detail, "已停止") {
		t.Fatalf("detail = %q：任务已经返回了，面板上必须写「已停止」，否则看着还像在重试", final.Detail)
	}
	if strings.Contains(final.Detail, "等手机隧道恢复") {
		t.Fatalf("detail = %q：终止态不能沿用「等手机隧道恢复」—— 那是重试中的措辞", final.Detail)
	}
	if !strings.Contains(final.Detail, strconv.Itoa(start)) {
		t.Fatalf("detail = %q：终止态要带上下一个未尝试的号 %d，否则续跑点只能靠对账翻",
			final.Detail, start)
	}
	// 落盘日志是事后唯一的信息源（内存里只有最近 40 条，nextNum 也不落盘），续跑点必须在里面。
	joined := strings.Join(final.Notes, "\n")
	if !strings.Contains(joined, "任务已停止") || !strings.Contains(joined, strconv.Itoa(start)) {
		t.Fatalf("日志里没有「任务已停止」和续跑点 %d：%q", start, joined)
	}
	// running 和 Detail 不能互相打架。
	if final.Running {
		t.Fatalf("goroutine 已经返回却仍报 running=true：%#v", final)
	}
	if final.NextNum != start {
		t.Fatalf("nextNum = %d，第一批就死了，续跑点应当还是 %d", final.NextNum, start)
	}
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

// 手机上 phone-socks 放在哪里属于部署决定，配置里必须能钉住。
//
// ensurePhoneTunnel 从来没填过 TunnelRequest.Binary，于是 EnsureTunnel 只能用它自己写死
// 的 /data/local/tmp/phone-socks —— 换个位置部署（有的机型会清那个目录），每一批都以
// 「手机上没有可执行的 …」失败，而配置里根本没有能改它的键。
func TestRegisterJobPassesConfiguredPhoneSocksBinaryToTunnel(t *testing.T) {
	stubRotate(t)
	const want = "/data/local/tmp/socks-elsewhere"
	seen := make(chan string, 4)
	old := ensureTunnel
	t.Cleanup(func() { ensureTunnel = old })
	ensureTunnel = func(_ context.Context, req exitrotate.TunnelRequest) (exitrotate.TunnelResult, error) {
		select {
		case seen <- req.Binary:
		default:
		}
		return exitrotate.TunnelResult{OK: true, IP: "2409:895a:1::1"}, nil
	}

	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	patchRunnerRegisterConfig(t, root, map[string]any{"phone_socks_bin": want})

	server := &Server{}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 5026, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })

	select {
	case got := <-seen:
		if got != want {
			t.Fatalf("TunnelRequest.Binary = %q，想要配置里的 %q", got, want)
		}
	default:
		t.Fatal("phone 模式却没有检查过隧道")
	}
}

// 请求不带 batchSize 时要听配置里的 register_batch_size，而不是永远退回写死的 20。
func TestStartRegisterJobFallsBackToConfiguredBatchSize(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "", errors.New("手机没插"))
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	patchRunnerRegisterConfig(t, root, map[string]any{"register_batch_size": 7})

	server := &Server{}
	state, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 9000, SkipOAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.BatchSize != 7 {
		t.Fatalf("batchSize = %d，想要配置里的 7", state.BatchSize)
	}
	server.registerJob().stop()
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
}

// 配置里的值也要压到单次注册接口的上限：写了 500 不压的话每一批都会以「单次最多注册
// 20 个账号」失败 —— 一个从配置文件里就能埋下的、跑起来才发现的坑。
func TestStartRegisterJobClampsConfiguredBatchSize(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "", errors.New("手机没插"))
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	patchRunnerRegisterConfig(t, root, map[string]any{"register_batch_size": 500})

	server := &Server{}
	state, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 9000, SkipOAuth: true,
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

// 请求里显式给的 batchSize 仍然优先：那是调用方对这一次任务的明确意图，配置只是默认值。
func TestStartRegisterJobPrefersRequestBatchSizeOverConfig(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "", errors.New("手机没插"))
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	patchRunnerRegisterConfig(t, root, map[string]any{"register_batch_size": 7})

	server := &Server{}
	state, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 9000, BatchSize: 3, SkipOAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.BatchSize != 3 {
		t.Fatalf("batchSize = %d，想要请求里的 3", state.BatchSize)
	}
	server.registerJob().stop()
	waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
}

// 内存里只留最近 40 条，而一次跑几千个号意味着「哪些号失败了、要重试哪些」这类事在跑完
// 之前就被后面的批次挤掉了 —— 那恰恰是事后唯一要读的东西。落盘那份必须比内存环长命。
func TestRegisterJobNotesOutliveTheInMemoryRingOnDisk(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)

	// 每批 1 个、50 个号 → 启动 1 条 + 每批 1 条 + 收尾 1 条 = 52 条 > 40，前面十几条必然
	// 已经不在内存里。
	const start, target = 5026, 5075
	server := &Server{}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: start, Target: target, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	if final.Success != target-start+1 {
		t.Fatalf("success = %d，想要 %d", final.Success, target-start+1)
	}
	if len(final.Notes) > registerJobMaxNotes {
		t.Fatalf("内存里留了 %d 条，上限是 %d", len(final.Notes), registerJobMaxNotes)
	}

	logPath := filepath.Join(root, "register-job.log")
	if final.LogPath != logPath {
		t.Fatalf("logPath = %q，想要 %q", final.LogPath, logPath)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := string(raw)
	memory := strings.Join(final.Notes, "\n")

	// 先确认这个用例真的有话可说：被挤掉的那几条必须确实不在内存里，否则它证明不了任何事。
	for _, dropped := range []string{"任务启动", "批次 5026-5026", "批次 5030-5030"} {
		if strings.Contains(memory, dropped) {
			t.Fatalf("样本不够：%q 还留在内存里，这个用例什么都没验证到", dropped)
		}
		if !strings.Contains(onDisk, dropped) {
			t.Fatalf("落盘日志里没有 %q：\n%s", dropped, onDisk)
		}
	}
	// 最后一条也要在：不能只落了前半段就断了。
	if !strings.Contains(onDisk, "跑完：到 5075") {
		t.Fatalf("落盘日志里没有收尾那一行：\n%s", onDisk)
	}

	lines := strings.Split(strings.TrimRight(onDisk, "\n"), "\n")
	if len(lines) <= registerJobMaxNotes {
		t.Fatalf("落盘 %d 行，没有超过内存上限 %d，等于没留下额外历史", len(lines), registerJobMaxNotes)
	}
	// 每行都要带完整日期：内存里只有 [HH:MM:SS]，而这种任务会跨过午夜，事后翻日志分不清
	// 「03:12:07 失败」是哪一天的。
	// 允许日期之后有缩进：批次里的子项（失败明细、report.Notes）是缩进两格写的，那是
	// 刻意的层次，和「这一行有没有完整日期」无关。收尾的 \S 只用来挡住「只有时间戳、
	// 正文是空的」那种行。
	stamped := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} +\S`)
	for i, line := range lines {
		if !stamped.MatchString(line) {
			t.Fatalf("第 %d 行没有完整日期前缀：%q", i+1, line)
		}
	}
}

// 日志开不出来不该拖死任务：任务是值钱的那个，日志只是诊断。
//
// 用一个同名目录顶掉日志文件 —— 以写方式打开一个目录在 Windows 和 Linux 上都必然失败，
// 不像权限位那样依赖平台。
func TestRegisterJobKeepsRegisteringWhenTheLogCannotBeOpened(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, okSite(t).URL)
	if err := os.MkdirAll(filepath.Join(root, "register-job.log"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := &Server{}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}), registerJobRequest{
		Mode: "phone", StartNum: 5026, Target: 5028, BatchSize: 1, SkipOAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	if final.Success != 3 {
		t.Fatalf("success = %d，想要 3：写不了日志不该少注册一个号", final.Success)
	}
	if final.Detail != "已完成" {
		t.Fatalf("detail = %q，想要 已完成", final.Detail)
	}
	// 但也不能静默：日志没了这件事本身要说一声。
	if !strings.Contains(strings.Join(final.Notes, "\n"), "日志落盘不可用") {
		t.Fatalf("日志打不开却没有任何提示：%#v", final.Notes)
	}
	if final.LogPath != "" {
		t.Fatalf("logPath = %q，日志根本没开成，不该报一个位置出去", final.LogPath)
	}
}

// 跑到一半写不下去了（盘满了、U 盘被拔了）也一样：就地放弃这份日志，说一次，然后继续。
//
// 关键是不能留着坏句柄反复重试 —— note 是在 j.mu 里跑的，状态接口要拿同一把锁，每条日志
// 都去撞一次坏掉的目标会把面板一起拖住。
func TestRegisterJobNoteGivesUpOnceWhenTheLogWriteFails(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "register-job-*.log")
	if err != nil {
		t.Fatal(err)
	}
	// 提前关掉句柄，之后每次 WriteString 都会失败 —— 等价于跑到一半目标没了。
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	job := &registerJob{logFile: file}
	job.note("第一条")
	if job.logFile != nil {
		t.Fatal("写失败之后还留着句柄，后面每条日志都会再撞一次")
	}
	job.note("第二条")
	job.note("第三条")

	joined := strings.Join(job.snapshot().Notes, "\n")
	if !strings.Contains(joined, "日志写入失败") {
		t.Fatalf("写失败却没有任何提示：%q", joined)
	}
	if got := strings.Count(joined, "日志写入失败"); got != 1 {
		t.Fatalf("「日志写入失败」出现 %d 次，只该说一次", got)
	}
	for _, want := range []string{"第一条", "第二条", "第三条"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q 没进内存：写不了日志不该连内存里的进度都丢掉", want)
		}
	}
}
