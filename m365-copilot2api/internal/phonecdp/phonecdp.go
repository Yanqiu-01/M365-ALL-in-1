// Package phonecdp 负责把手机上 Cromite 的调试端口接到本机来。
//
// 为什么要有这个包：注册页的 Turnstile 必须由一个真浏览器来解（原因见
// internal/turnstile/chrome.go 顶部的说明）。原先那个浏览器是 PC 上的 Chrome，页面从
// PC 发出，于是必须再拿一条 SOCKS 隧道把流量绕到手机出口去 —— 多一层转发，还多了
// 「隧道端口和注册实际用的地址不一致」这类只在运行时才暴露的问题。
//
// 直接驱动手机上的 Cromite 就没有这些事：页面本来就在手机上加载，出口天然是运营商 IP，
// 换 IP 仍然靠飞行模式。PC 这边只剩一次 adb forward，不再起浏览器进程。
//
// # 手机侧的前提
//
// Cromite 读的命令行文件是 /data/local/tmp/chrome-command-line —— 不是
// cromite-command-line（那个文件在这台机器上存在，但没有任何进程读它，照着包名去改会
// 白改半天）。要确认改动有没有生效，只有一个可靠办法：打开 chrome://version，那一页会把
// 实际生效的命令行原样列出来。
//
// 这个文件里必须有两样东西：
//
//   - --remote-debugging-port=0：Android 上它就会打开默认的 abstract socket
//     chrome_devtools_remote，也就是下面转发的那个。
//   - --no-proxy-server：这台手机的系统设置里 http_proxy 是 ":0" 这个畸形残留值，
//     Chromium 会当成真代理去连，于是所有请求都以 ERR_PROXY_CONNECTION_FAILED 收场
//     （连 127.0.0.1 也一样，因为 pref/系统来源的代理配置不会隐式放过 loopback）。
//     光把 --proxy-server 删掉不够 —— 删掉只会让配置回落到那个系统值，必须显式禁用。
//     注册要的正是直连：手机自己的运营商 IP 就是出口。
//
// 另外两条实测出来的约束，改这块时容易踩：
//
//   - Android 会冻结后台标签页的渲染进程，对着它发 Runtime.enable 会一直没有回包。所以
//     只用自己新建并置于前台的标签页，不要去接管已有的。
//   - Target.createBrowserContext 在 Android 上恒失败，隐身式隔离用不了，见
//     turnstile/phone_cdp.go 里 clearProfileState 的处理。
package phonecdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/procwin"
)

const (
	// DefaultSocket 是 Chromium 在 Android 上监听调试连接的 abstract unix socket 名。
	// 它由 --remote-debugging-socket-name 决定，Chromium 的默认值就是这个。
	DefaultSocket = "chrome_devtools_remote"
	// DefaultLocalPort 是本机这一侧的转发端口。
	//
	// 刻意不和 phone_socks 的 1081 挨着，也不用 9222 —— 9222 是 PC 上 Chrome 的习惯端口，
	// 撞上了会让人以为连的是本机浏览器，而实际连的是手机，排查时最容易被这种巧合误导。
	DefaultLocalPort = 9333
	// DefaultPackage 是 Cromite 的包名。
	DefaultPackage = "org.cromite.cromite"
	// DefaultActivity 是它的主 Activity。Cromite 沿用 Chromium 的类名。
	DefaultActivity = "com.google.android.apps.chrome.Main"
)

// Config 是接通手机调试端口所需的参数。零值可用：除 ADB 之外都有默认值。
type Config struct {
	// ADB 是 adb 可执行文件的路径。必须显式给出：Windows 上 adb 通常装在 WinGet 的包
	// 目录里、并不在 PATH，只写 "adb" 会以「找不到」失败。
	ADB string
	// Serial 指定设备。只接了一台手机时可以留空。
	Serial string
	// Package / Activity 指定要驱动的浏览器。留空用 Cromite。
	Package  string
	Activity string
	// Socket 是手机侧的 abstract unix socket 名。留空用 DefaultSocket。
	Socket string
	// LocalPort 是本机侧的转发端口。留空用 DefaultLocalPort。
	LocalPort int
}

func (c Config) pkg() string {
	if v := strings.TrimSpace(c.Package); v != "" {
		return v
	}
	return DefaultPackage
}

func (c Config) activity() string {
	if v := strings.TrimSpace(c.Activity); v != "" {
		return v
	}
	return DefaultActivity
}

func (c Config) socket() string {
	if v := strings.TrimSpace(c.Socket); v != "" {
		return v
	}
	return DefaultSocket
}

func (c Config) port() int {
	if c.LocalPort > 0 {
		return c.LocalPort
	}
	return DefaultLocalPort
}

// Result 说明这次接通的结果，好让上层把它如实显示出来。
type Result struct {
	// BaseURL 是本机可用的 CDP HTTP 端点，例如 http://127.0.0.1:9333。
	BaseURL string
	// Browser 是浏览器自报的版本，例如 Chrome/148.0.7778.168。
	Browser string
	// UserAgent 是它自报的 UA。留着是为了核对「确实是手机上的移动端浏览器」，
	// 而不是不小心连到了 PC 上的 Chrome。
	UserAgent string
	// Package 是手机上实际被驱动的包名。
	Package string
	// Started 表示这次是否由我们把浏览器拉起来的（false 说明它本来就在跑）。
	Started bool
}

var errNoADB = errors.New("未配置 adb 路径：Windows 上 adb 通常不在 PATH 里，必须给出绝对路径")

// EnsureCromite 保证手机上的浏览器在跑、调试端口已转发到本机，并返回可用的 CDP 端点。
//
// 可以重复调用：转发已存在就不重复加，浏览器已在跑就不重启。
func EnsureCromite(ctx context.Context, cfg Config) (Result, error) {
	adb := strings.TrimSpace(cfg.ADB)
	if adb == "" {
		return Result{}, errNoADB
	}
	base := "http://127.0.0.1:" + strconv.Itoa(cfg.port())
	out := Result{BaseURL: base, Package: cfg.pkg()}

	if err := ensureForward(ctx, cfg); err != nil {
		return out, err
	}

	// 先直接问一次：浏览器可能本来就在跑，那样连 am start 都不必发。
	if ver, err := probeVersion(ctx, base); err == nil {
		out.Browser, out.UserAgent = ver.Browser, ver.UserAgent
		if err := checkIsPhoneBrowser(ver); err != nil {
			return out, err
		}
		return out, nil
	}

	if err := startBrowser(ctx, cfg); err != nil {
		return out, err
	}
	out.Started = true

	// 冷启动到监听 socket 有几秒延迟，轮询而不是睡一个固定时长。
	deadline := time.Now().Add(25 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		ver, err := probeVersion(ctx, base)
		if err == nil {
			out.Browser, out.UserAgent = ver.Browser, ver.UserAgent
			if err := checkIsPhoneBrowser(ver); err != nil {
				return out, err
			}
			return out, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
	return out, fmt.Errorf("已拉起 %s 但 %s 上的调试端口一直不应答（最后一次：%v）；"+
		"请确认 /data/local/tmp/chrome-command-line 里带了 --remote-debugging-port=0，"+
		"并用手机上的 chrome://version 核对它确实生效",
		cfg.pkg(), base, lastErr)
}

// ensureForward 建立 tcp:<本机端口> -> localabstract:<socket> 的转发。
//
// 已经存在同样的转发就什么都不做：adb forward 对重复项是覆盖语义，不会报错，但重复下发
// 会在日志里造成「每个号都在重建转发」的错觉，排查时容易被误导。
func ensureForward(ctx context.Context, cfg Config) error {
	want := "tcp:" + strconv.Itoa(cfg.port())
	target := "localabstract:" + cfg.socket()

	listed, err := adbOutput(ctx, cfg, "forward", "--list")
	if err == nil {
		for _, line := range strings.Split(listed, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			// 一行形如：<serial> tcp:9333 localabstract:chrome_devtools_remote
			if len(fields) >= 3 && fields[1] == want && fields[2] == target {
				return nil
			}
		}
	}
	if _, err := adbOutput(ctx, cfg, "forward", want, target); err != nil {
		return fmt.Errorf("建立调试端口转发失败（%s -> %s）：%w", want, target, err)
	}
	return nil
}

// WipeBrowser 把浏览器整个停掉并擦掉它的数据，让下一次 EnsureCromite 从全新 profile 起跑。
//
// 为什么非得这么重：手机上只有一个长驻 profile，CDP 那几个 clear* 命令只能按来源清，永远盖
// 不全 —— 清得掉 cookie 和两个 origin 的存储，清不掉上一个号留下的渲染进程、还开着的标签页、
// 各种偏好和内存里的状态。实测过一次：一个号成功之后不擦，下一个号就再也走不下去，页面停在
// 注册站上、四个 sandboxed renderer 全挂着，五分钟一个号都没出来；把 pm clear 一发立刻恢复。
//
// force-stop 和 pm clear 都发：pm clear 本身会杀进程，但先 force-stop 能让「进程已退出」和
// 「数据已清空」分成两步，其中任一步失败时报错指向明确。
//
// /data/local/tmp/chrome-command-line 不在应用数据里，pm clear 不会动它（实测确认），所以
// 擦完之后那些必需的启动参数（尤其是 --remote-debugging-port 和 --no-proxy-server）还在。
//
// 失败要往上报而不是忽略：擦不掉就意味着下一个号在脏 profile 上跑，而那个故障的现象是「解不
// 出 Turnstile」，会把排查引到出口 IP 上去，事后极难归因。宁可当场停下。
func WipeBrowser(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.ADB) == "" {
		return errNoADB
	}
	if _, err := adbOutput(ctx, cfg, "shell", "am", "force-stop", cfg.pkg()); err != nil {
		return fmt.Errorf("停掉 %s 失败，无法保证和上一个号隔离：%w", cfg.pkg(), err)
	}
	out, err := adbOutput(ctx, cfg, "shell", "pm", "clear", cfg.pkg())
	if err != nil {
		return fmt.Errorf("清空 %s 的数据失败，无法保证和上一个号隔离：%w", cfg.pkg(), err)
	}
	// pm clear 失败时退出码仍是 0，只在 stdout 上回 "Failed"。不看这一行就会把失败当成功。
	if !strings.Contains(out, "Success") {
		return fmt.Errorf("清空 %s 的数据没有成功（pm clear 回：%s）", cfg.pkg(), strings.TrimSpace(out))
	}
	return nil
}

// startBrowser 把浏览器拉到前台并打开 about:blank。
//
// 用 about:blank 而不是直接打开注册页：这一步只为了让它开始监听调试端口，页面导航由
// CDP 那边控制，混在一起会让「浏览器起没起来」和「页面开没开对」两件事纠缠在一块。
func startBrowser(ctx context.Context, cfg Config) error {
	_, err := adbOutput(ctx, cfg, "shell", "am", "start",
		"-n", cfg.pkg()+"/"+cfg.activity(),
		"-a", "android.intent.action.VIEW",
		"-d", "about:blank")
	if err != nil {
		return fmt.Errorf("拉起 %s 失败：%w", cfg.pkg(), err)
	}
	return nil
}

type versionInfo struct {
	AndroidPackage  string `json:"Android-Package"`
	Browser         string `json:"Browser"`
	ProtocolVersion string `json:"Protocol-Version"`
	UserAgent       string `json:"User-Agent"`
	WebSocketURL    string `json:"webSocketDebuggerUrl"`
}

// probeVersion 问一次 /json/version。连不上和回包不对要分开报：前者是端口没通，
// 后者是连到了别的东西上。
func probeVersion(ctx context.Context, base string) (versionInfo, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(base, "/")+"/json/version", nil)
	if err != nil {
		return versionInfo{}, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return versionInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return versionInfo{}, fmt.Errorf("/json/version 返回 %s", resp.Status)
	}
	var info versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return versionInfo{}, fmt.Errorf("/json/version 回包解不开：%w", err)
	}
	if strings.TrimSpace(info.WebSocketURL) == "" {
		return info, errors.New("/json/version 里没有 webSocketDebuggerUrl")
	}
	return info, nil
}

// checkIsPhoneBrowser 确认连上的确实是手机上的浏览器。
//
// 值得单独检查：转发端口是本机地址，一旦手机掉线而本机恰好有别的 Chromium 在同一端口上
// 监听，注册会静默地从 PC 出口发出去 —— 那正是这套改造要消掉的情形，而且它不会报错，
// 只会表现为「换 IP 一切正常但号一直失败」。
func checkIsPhoneBrowser(info versionInfo) error {
	if strings.TrimSpace(info.AndroidPackage) == "" {
		return fmt.Errorf("调试端口应答了，但回包里没有 Android-Package（Browser=%q）——"+
			"这说明连上的不是手机上的浏览器，注册会从本机出口发出，因此这里停下",
			strings.TrimSpace(info.Browser))
	}
	if !strings.Contains(info.UserAgent, "Android") {
		return fmt.Errorf("调试端口应答了，但 UA 里没有 Android（%q），不敢当成手机浏览器用",
			strings.TrimSpace(info.UserAgent))
	}
	return nil
}

// BrowserWebSocket 问出浏览器级的调试 WebSocket 地址，可以直接用来 Dial。
func BrowserWebSocket(ctx context.Context, base string) (string, error) {
	info, err := probeVersion(ctx, base)
	if err != nil {
		return "", err
	}
	return WebSocketURL(base, info.WebSocketURL)
}

// WebSocketURL 把 /json/version 给的 ws 地址改写到本机转发端口上。
//
// 为什么要改写：手机回的地址里带的是它自己那一侧的 host:port，多数情况下恰好也是
// 127.0.0.1:<同一个端口>，但那是巧合。按 base 的 host 重建才和实际连接路径一致。
func WebSocketURL(base, raw string) (string, error) {
	parsedWS, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("调试 WebSocket 地址解不开：%w", err)
	}
	parsedBase, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return "", fmt.Errorf("CDP 端点地址解不开：%w", err)
	}
	parsedWS.Scheme = "ws"
	parsedWS.Host = parsedBase.Host
	return parsedWS.String(), nil
}

func httpClient() *http.Client {
	return &http.Client{
		Timeout: 8 * time.Second,
		// 调试端口是 adb 转发到本机的回环地址，任何代理都必须绕开 —— 走代理会把它变成
		// 一次外网请求，然后以超时收场。
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
			DisableKeepAlives:   true,
			MaxIdleConnsPerHost: -1,
		},
	}
}

// adbOutput 跑一条 adb 命令并回收输出。
//
// 每一处都要压掉控制台窗口：主程序启动时调用了 FreeConsole，之后它起的每个控制台子进程
// 都会被 Windows 新建一个控制台窗口，而 adb 在注册过程中要跑很多次 —— 那就是屏幕上
// 一直闪黑框的来源。
func adbOutput(ctx context.Context, cfg Config, args ...string) (string, error) {
	full := args
	if serial := strings.TrimSpace(cfg.Serial); serial != "" {
		full = append([]string{"-s", serial}, args...)
	}
	cmd := exec.CommandContext(ctx, strings.TrimSpace(cfg.ADB), full...)
	procwin.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("adb %s: %w (%s)", strings.Join(args, " "), err, text)
	}
	return text, nil
}
