package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"m365-copilot2api/internal/outbound"
)

// HandleV1 is the FlareSolverr-compatible JSON endpoint.
func HandleV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"POST required"}`, http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeFlare(w, flareReply{Status: "error", Message: err.Error()})
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeFlare(w, flareReply{Status: "error", Message: "invalid json"})
		return
	}
	cmd := strings.ToLower(strings.TrimSpace(fmt.Sprint(payload["cmd"])))
	switch cmd {
	case "request.get", "request.post":
	default:
		writeFlare(w, flareReply{Status: "error", Message: "unsupported cmd"})
		return
	}
	page := strings.TrimSpace(fmt.Sprint(payload["url"]))
	if page == "" {
		writeFlare(w, flareReply{Status: "error", Message: "url is required"})
		return
	}
	timeout := DefaultTimeout
	if v, ok := payload["maxTimeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	solved, err := solveLocal(ctx, page, strField(payload, "displayName"), strField(payload, "username"), strField(payload, "password"), proxyField(payload))
	if err != nil {
		writeFlare(w, flareReply{Status: "error", Message: err.Error()})
		return
	}
	html := solved.HTML
	if html == "" && solved.Token != "" {
		html = `<!doctype html><input name="cf-turnstile-response" value="` + htmlEscape(solved.Token) + `">`
	}
	writeFlare(w, flareReply{
		Status:  "ok",
		Message: "Challenge solved!",
		Solution: flareSolution{
			URL:       page,
			Status:    200,
			Response:  html,
			UserAgent: solved.UserAgent,
		},
	})
}

// proxyField 取出 proxy 字段，同时接受 FlareSolverr 的嵌套写法和裸字符串。
//
// 本包自己的客户端（solver.go）按 FlareSolverr 规范发的是 {"proxy":{"url":...}}。
// 早先这里用 strField 读，等于对一个 map 调 fmt.Sprint，得到的是
// "map[url:socks5://...]" —— 就算下游用它也不可能连得上。两种形状都收下，避免
// 同一个进程的两端各说一套。
func proxyField(payload map[string]any) string {
	v, ok := payload["proxy"]
	if !ok || v == nil {
		return ""
	}
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if raw, ok := typed["url"]; ok && raw != nil {
			if s, ok := raw.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func strField(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "<nil>" {
		return ""
	}
	return s
}

func writeFlare(w http.ResponseWriter, reply flareReply) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.EqualFold(reply.Status, "ok") {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func htmlEscape(v string) string {
	r := strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;")
	return r.Replace(v)
}

// Listen serves the built-in FlareSolverr API on addr, typically 127.0.0.1:8191.
func Listen(ctx context.Context, addr string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", HandleV1)
	mux.HandleFunc("/v1/", HandleV1)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("built-in FlareSolverr listening on http://%s/v1", addr)
	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type localSolution struct {
	Token     string
	HTML      string
	UserAgent string
}

func solveLocal(ctx context.Context, page, display, username, password, proxy string) (localSolution, error) {
	// 桌面端根本不该走到这里：内置求解器只是转交给 App 内 WebView 的中继，
	// cmd/server 在非 Android 上已经不启动它。万一用户把 flaresolverr_url 指到了
	// 一台跑着本程序的桌面机，也要给出能照着做的提示，而不是叫他去开安卓 App。
	if err := webViewSupported(hostOS()); err != nil {
		return localSolution{}, err
	}
	dir := flareDir()
	if dir == "" {
		return localSolution{}, errors.New("内置 FlareSolverr 需要 App 数据目录。请打开修改版M365 后再注册")
	}
	return solveViaWebView(ctx, dir, page, display, username, password, proxy)
}

// Cancel asks the in-app register WebView to stop the current job.
// Cancel 通过在协作目录里放下 cancel 标记来取消进行中的 WebView 求解，返回是否
// 真的发出了这个信号。
//
// 返回值不是可选的礼节：协作目录由 M365_DATA_DIR 决定，而桌面端默认不设这个变量
// （树里唯一的设置者是 build/android/qemu-smoke.sh）。此时无处可写，取消什么也没
// 做。调用方据此如实回话，不要在根本没取消的情况下告诉用户已停止。
func Cancel() bool {
	dir := flareDir()
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, "cancel"), []byte("1\n"), 0o600); err != nil {
		return false
	}
	// result 只是给等待方的提示，写不成也不影响 cancel 标记本身已经生效。
	_ = os.WriteFile(filepath.Join(dir, "result"), []byte("0\nCANCEL\n"), 0o600)
	return true
}

// hostOS 报告当前平台，可用 M365_TURNSTILE_FORCE_OS 覆盖。
//
// 存在这个覆盖点，是因为直接读 runtime.GOOS 的判断在构造上就无法测试：WebView 中
// 继路径只在 Android 成立，而测试跑在开发机上。硬编码会让那条路径永远测不到。
func hostOS() string {
	if v := strings.TrimSpace(os.Getenv("M365_TURNSTILE_FORCE_OS")); v != "" {
		return v
	}
	return runtime.GOOS
}

// WebViewAvailable 报告这台机器上内置求解器是否可能解出结果。
//
// cmd/server 据此决定要不要启动它。桌面端不启动有两个理由：它必然解不出来，而且
// 8191 正是真实 FlareSolverr 的默认端口 —— 让一个解不出结果的中继占着它，用户就
// 再也起不了能用的求解器。
func WebViewAvailable() bool { return webViewSupported(hostOS()) == nil }

// webViewSupported 说明为什么这台机器上没有内置求解器，并给出能照着做的替代方案。
//
// 内置求解器自己解不了 Turnstile —— 它只是把任务通过协作目录转交给 App 内的
// WebView。桌面端没有那个 WebView，所以这里必须明确拒绝，而不是回一句「请打开修改
// 版M365」把 PC 用户引到一个不存在的 App 上。
func webViewSupported(goos string) error {
	if goos == "android" {
		return nil
	}
	return fmt.Errorf(
		"内置求解器只在 Android 上可用：它把验证任务转交给 App 内的 WebView，而 %s 上没有这个 WebView。"+
			"请在本机起一个真正的 FlareSolverr（例如 docker run -d -p 8191:8191 ghcr.io/flaresolverr/flaresolverr:latest），"+
			"再把面板里的 flaresolverr_url 指向它", goos)
}

func flareDir() string {
	root := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if root == "" {
		return ""
	}
	return filepath.Join(root, "flare")
}

// webviewSlot 串行化 WebView 求解：同一时刻只能有一个任务在 App 里跑。
//
// 用容量 1 的 channel 而不是 sync.Mutex，因为 Mutex 的 Lock 无法被 ctx 打断。原先
// 的写法是无条件阻塞：一个排在队尾的请求即使自己的 deadline 早就过了，也会在拿到
// 锁之后照样往协作目录写一份新的 job —— 给 WebView 派了一个调用方已经放弃的任务，
// 还会留下 job/result 残留去干扰下一次求解。
var webviewSlot = make(chan struct{}, 1)

// acquireWebView 取得求解槽位，等待期间尊重 ctx。
func acquireWebView(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 已经过期就不要再排队。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("验证请求已取消，未提交给 WebView: %w", err)
	}
	select {
	case webviewSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待上一个验证任务完成时超时，未提交新任务: %w", ctx.Err())
	}
}

func releaseWebView() {
	select {
	case <-webviewSlot:
	default:
	}
}

func oneLine(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

// solveViaWebView 把一次求解任务交给应用内的 WebView，通过协作目录里的文件交换。
//
// proxy 收下但不会转发，这一点是有意的，不是漏掉：job 文件是 5 行的固定协议，读
// 取方在改版 APK 里，不在本仓库，无法验证它是否接受第 6 行 —— 贸然加字段会让握手
// 直接失效。WebView 跑在应用进程里，用的是设备自身的网络，所以调用方选的出口在这
// 一步本来也不生效。与其静默丢弃，不如记一行日志让它可见。
func solveViaWebView(ctx context.Context, dir, page, display, username, password, proxy string) (localSolution, error) {
	if p := strings.TrimSpace(proxy); p != "" {
		log.Printf("turnstile: WebView solve ignores the requested egress proxy %s (in-app WebView uses the device network; job protocol has no proxy field)", outbound.RedactProxyURL(p))
	}
	if err := acquireWebView(ctx); err != nil {
		return localSolution{}, err
	}
	defer releaseWebView()
	// 排队可能耗掉整个 deadline。拿到槽位后再确认一次，避免写下一份没人等的 job。
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return localSolution{}, fmt.Errorf("取得验证槽位时请求已结束，未提交任务: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return localSolution{}, err
	}
	ready := filepath.Join(dir, "ready")
	if _, err := os.Stat(ready); err != nil {
		return localSolution{}, errors.New("验证页还没启动。请先打开修改版M365 主界面，等几秒后再点开始注册")
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	job := filepath.Join(dir, "job")
	tmp := filepath.Join(dir, "job.tmp")
	result := filepath.Join(dir, "result")
	status := filepath.Join(dir, "status")
	cancel := filepath.Join(dir, "cancel")
	_ = os.Remove(result)
	_ = os.Remove(status)
	_ = os.Remove(cancel)
	body := strings.Join([]string{id, oneLine(page), oneLine(display), oneLine(username), oneLine(password)}, "\n") + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return localSolution{}, err
	}
	if err := os.Rename(tmp, job); err != nil {
		return localSolution{}, err
	}
	defer os.Remove(job)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	jobTaken := false
	pageLoaded := false
	for {
		select {
		case <-ctx.Done():
			return localSolution{}, timeoutError(jobTaken, pageLoaded)
		case <-ticker.C:
			if _, err := os.Stat(cancel); err == nil {
				_ = os.Remove(cancel)
				return localSolution{}, errors.New("已取消验证")
			}
			if _, err := os.Stat(job); err != nil {
				jobTaken = true
			}
			if raw, err := os.ReadFile(status); err == nil && strings.Contains(string(raw), "loaded") {
				pageLoaded = true
			}
			raw, err := os.ReadFile(result)
			if err != nil {
				continue
			}
			lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
			if len(lines) < 2 {
				continue
			}
			token := strings.TrimSpace(lines[1])
			if token == "CANCEL" {
				_ = os.Remove(result)
				return localSolution{}, errors.New("已取消验证")
			}
			if strings.TrimSpace(lines[0]) != id {
				continue
			}
			_ = os.Remove(result)
			if token == "" {
				return localSolution{}, errors.New("后台验证没有拿到 Turnstile token")
			}
			return localSolution{Token: token, UserAgent: "M365-WebView"}, nil
		}
	}
}

func timeoutError(jobTaken, pageLoaded bool) error {
	switch {
	case !jobTaken:
		return errors.New("后台验证没有接到任务。请保持修改版M365 在前台，不要锁屏")
	case !pageLoaded:
		return errors.New("后台验证没有打开注册站。请检查网络后重试")
	default:
		return errors.New("注册页已打开，但没有完成填表和提交")
	}
}
