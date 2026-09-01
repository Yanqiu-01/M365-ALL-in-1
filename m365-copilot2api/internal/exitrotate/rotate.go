package exitrotate

// 换出口 IP。原 Python 注册脚本在每次 /api/register 前做这件事：
//
//   phone  本机切飞行模式（Android 上绝不调用电脑端 adb），走手机 SOCKS5
//   clash  PUT Clash 外部控制接口切节点
//   proxy  调用方自己换代理 URL，这里只负责探测出口 IP
//
// Rust CLI（tools/exit-rotate）是首选实现；找不到可执行文件时回退到本包，
// 这样 Android APK 不必再带一份 .so。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"m365-copilot2api/internal/outbound"
)

// Result 是一次换出口的结果。IP 为空表示探测失败。
type Result struct {
	OK      bool   `json:"ok"`
	Mode    string `json:"mode"`
	IP      string `json:"ip,omitempty"`
	PrevIP  string `json:"prevIp,omitempty"`
	Changed bool   `json:"changed"`
	Detail  string `json:"detail,omitempty"`
}

// Request 描述一次换出口。Mode 为 phone / clash / proxy。
type Request struct {
	Mode string `json:"mode"`

	PrevIP string `json:"prevIp,omitempty"`

	// phone
	PhoneSOCKS string `json:"phoneSocks,omitempty"`
	ADB        string `json:"adb,omitempty"`

	// clash
	ClashAPI    string `json:"clashApi,omitempty"`
	ClashSecret string `json:"clashSecret,omitempty"`
	ClashGroup  string `json:"clashGroup,omitempty"`
	ClashProxy  string `json:"clashProxy,omitempty"`
	ClashNode   string `json:"clashNode,omitempty"`
	ExpectIP    string `json:"expectIp,omitempty"`

	// proxy：只探测，不切换。ProbeProxy 是出口 URL。
	ProbeProxy string `json:"probeProxy,omitempty"`
}

var (
	errUnsupportedMode = errors.New("unsupported exit-rotate mode")
	// icanhazip 放第一个：实测 api.ipify.org 在中国移动的手机出口上连不通（curl 直接
	// 000），排在前面等于每次探测都先白等一个超时。手机出口是注册的主力，顺序按它来。
	ipEndpoints = []string{
		"https://icanhazip.com",
		"https://ifconfig.me/ip",
		"https://api.ipify.org",
	}
	clashSettle = 3 * time.Second
	phoneSettle = 3 * time.Second
	// phoneProbeBudget 是切完飞行模式后等运营商把网络接回来的预算。
	//
	// 实测中国移动重新附着要好几秒，phoneSettle(3s) 之后立刻探测基本都超时。而
	// rotatePhone 把探测失败当成「这一轮没换成」，六轮全失败就返回 Changed=false，
	// 调用方于是认为换 IP 没生效 —— 而 IP 其实每次都换了。这个预算就是为了别把
	// 「换成了但还没连上」误判成「没换成」。
	phoneProbeBudget = 30 * time.Second
	phoneProbeStep   = 2 * time.Second
	// probeAttemptTimeout 给单次探测一个上限，否则一次卡死的请求会吃掉整个预算。
	probeAttemptTimeout = 8 * time.Second
)

// Rotate 先尝试 Rust CLI，失败再走 Go 实现。
func Rotate(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bin := findCLI(); bin != "" {
		result, err := rotateCLI(ctx, bin, req)
		if err == nil {
			return result, nil
		}
		// The fallback used to be silent: `if ... err == nil` dropped the CLI's error
		// on the floor with no log at all. The Rust CLI is the preferred
		// implementation, so a build that cannot execute, a JSON contract that
		// drifted, or a binary that crashes on every invocation degraded to the Go
		// path and looked exactly like a deployment that simply has no CLI installed.
		// Nobody had any way to learn the CLI was broken - the fallback works, so the
		// symptom is only that the preferred implementation is never used.
		log.Printf("exit-rotate cli failed, falling back to the Go implementation bin=%s mode=%s err=%v",
			bin, strings.ToLower(strings.TrimSpace(req.Mode)), err)
	}
	return rotateGo(ctx, req)
}

func findCLI() string {
	if explicit := strings.TrimSpace(os.Getenv("M365_EXIT_ROTATE")); explicit != "" {
		if info, err := os.Stat(explicit); err == nil && info.Mode().IsRegular() {
			return explicit
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, name := range []string{"exit-rotate", "exit-rotate.exe", "libexitrotate.so"} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func rotateCLI(ctx context.Context, bin string, req Request) (Result, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, bin, "--json")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("exit-rotate cli: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var result Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return Result{}, fmt.Errorf("exit-rotate cli json: %w", err)
	}
	return result, nil
}

func rotateGo(ctx context.Context, req Request) (Result, error) {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case "phone":
		return rotatePhone(ctx, req)
	case "clash":
		return rotateClash(ctx, req)
	case "proxy", "ip":
		ip, err := probeIP(ctx, firstNonEmpty(req.ProbeProxy, req.PhoneSOCKS, req.ClashProxy))
		if err != nil {
			return Result{Mode: mode, PrevIP: req.PrevIP, Detail: err.Error()}, err
		}
		return Result{OK: true, Mode: mode, IP: ip, PrevIP: req.PrevIP, Changed: ip != "" && ip != req.PrevIP}, nil
	default:
		return Result{}, fmt.Errorf("%w: %s", errUnsupportedMode, req.Mode)
	}
}

func rotatePhone(ctx context.Context, req Request) (Result, error) {
	prev := strings.TrimSpace(req.PrevIP)
	socks := firstNonEmpty(req.PhoneSOCKS, "socks5://127.0.0.1:1081")
	ip, err := probeIP(ctx, socks)
	// 只有确实知道上一个 IP 时，「当前出口已经不同」这个判断才成立。
	//
	// prev 为空说明上一轮探测失败或这是第一次轮换，此时任何非空 ip 都满足
	// ip != prev，早先这里会直接返回 OK 且 Changed=false —— 飞行模式一次都没切，
	// 下一个号仍从同一个出口注册，而调用方看到 OK 就继续了。既然不知道有没有变，
	// 就必须真的去切一次。
	if err == nil && ip != "" && prev != "" && ip != prev {
		return Result{OK: true, Mode: "phone", IP: ip, PrevIP: prev, Changed: true, Detail: "current exit already differs"}, nil
	}
	if prev == "" {
		prev = ip
	}
	var last string
	for i := 0; i < 6; i++ {
		if err := toggleAirplane(ctx, req.ADB); err != nil {
			return Result{Mode: "phone", PrevIP: prev, Detail: err.Error()}, err
		}
		// 关掉飞行模式不等于网络已经回来，要给运营商重新附着的时间再探测。
		ip, err = probeIPSettled(ctx, socks, phoneProbeBudget)
		if err != nil {
			last = err.Error()
			continue
		}
		if ip != "" && ip != prev {
			return Result{OK: true, Mode: "phone", IP: ip, PrevIP: prev, Changed: true}, nil
		}
		last = "exit IP unchanged (" + ip + ")"
	}
	return Result{OK: ip != "", Mode: "phone", IP: ip, PrevIP: prev, Changed: false, Detail: last}, nil
}

func toggleAirplane(ctx context.Context, adbPath string) error {
	if useLocalAirplane() {
		var last error
		for _, pair := range localAirplaneToggles() {
			if err := runCmd(ctx, pair[0][0], pair[0][1:]...); err != nil {
				last = err
				continue
			}
			if err := wait(ctx, phoneSettle); err != nil {
				return err
			}
			if err := runCmd(ctx, pair[1][0], pair[1][1:]...); err != nil {
				return fmt.Errorf("关闭飞行模式失败: %w", err)
			}
			return nil
		}
		if last == nil {
			last = errors.New("no local airplane-mode command")
		}
		return fmt.Errorf("本机无法切换飞行模式: %w。请改用 Clash 节点或当前代理，或手动开关飞行模式", last)
	}
	adb := firstNonEmpty(adbPath, "adb")
	if err := runCmd(ctx, adb, "shell", "cmd", "connectivity", "airplane-mode", "enable"); err != nil {
		return fmt.Errorf("adb airplane-mode enable: %w", err)
	}
	if err := wait(ctx, phoneSettle); err != nil {
		return err
	}
	if err := runCmd(ctx, adb, "shell", "cmd", "connectivity", "airplane-mode", "disable"); err != nil {
		return fmt.Errorf("adb airplane-mode disable: %w", err)
	}
	return nil
}

func useLocalAirplane() bool {
	if runtime.GOOS == "android" {
		return true
	}
	_, err := os.Stat("/system/bin/cmd")
	return err == nil
}

func localAirplaneToggles() [][2][]string {
	cmdBin := firstExisting("/system/bin/cmd", "cmd")
	settingsBin := firstExisting("/system/bin/settings", "settings")
	suBin := firstExisting("/system/xbin/su", "/system/bin/su", "su")
	var pairs [][2][]string
	if cmdBin != "" {
		pairs = append(pairs, [2][]string{
			{cmdBin, "connectivity", "airplane-mode", "enable"},
			{cmdBin, "connectivity", "airplane-mode", "disable"},
		})
	}
	if suBin != "" && cmdBin != "" {
		pairs = append(pairs, [2][]string{
			{suBin, "-c", cmdBin + " connectivity airplane-mode enable"},
			{suBin, "-c", cmdBin + " connectivity airplane-mode disable"},
		})
	}
	if settingsBin != "" {
		pairs = append(pairs, [2][]string{
			{settingsBin, "put", "global", "airplane_mode_on", "1"},
			{settingsBin, "put", "global", "airplane_mode_on", "0"},
		})
	}
	return pairs
}

func firstExisting(paths ...string) string {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
		if !strings.Contains(path, "/") {
			if looked, err := exec.LookPath(path); err == nil {
				return looked
			}
		}
	}
	return ""
}

func rotateClash(ctx context.Context, req Request) (Result, error) {
	api := strings.TrimRight(strings.TrimSpace(req.ClashAPI), "/")
	group := strings.TrimSpace(req.ClashGroup)
	node := strings.TrimSpace(req.ClashNode)
	if api == "" || group == "" || node == "" {
		return Result{}, errors.New("clash api, group and node are required")
	}
	endpoint := api + "/proxies/" + strings.ReplaceAll(urlPathEscape(group), "+", "%20")
	body, _ := json.Marshal(map[string]string{"name": node})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if secret := strings.TrimSpace(req.ClashSecret); secret != "" {
		httpReq.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return Result{Mode: "clash", Detail: err.Error()}, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("clash switch HTTP %d", resp.StatusCode)
	}
	if err := wait(ctx, clashSettle); err != nil {
		return Result{}, err
	}
	ip, err := probeIP(ctx, firstNonEmpty(req.ClashProxy, req.ProbeProxy))
	if err != nil {
		return Result{Mode: "clash", PrevIP: req.PrevIP, Detail: err.Error()}, err
	}
	// An ExpectIP mismatch is a failed rotation, not a warning.
	//
	// This used to return OK:true and Changed:true with the mismatch recorded only in
	// Detail, and no caller reads Detail: panel_register.go gates on `rotateErr == nil
	// && !rotated.Changed`, so both of its signals said the rotation had succeeded.
	// Clash accepted the node switch and then egressed somewhere else - a stale
	// selector, a node that failed over, a group that ignored the PUT - and the caller
	// went on to register the next account through an exit it had explicitly asked not
	// to use. If the operator named an expected exit, not reaching it is the whole
	// failure this mode is supposed to detect.
	if expect := strings.TrimSpace(req.ExpectIP); expect != "" && expect != ip {
		detail := "exit IP " + ip + " differs from expected " + expect
		return Result{Mode: "clash", IP: ip, PrevIP: req.PrevIP, Detail: detail},
			fmt.Errorf("clash rotation reached %s, expected %s", ip, expect)
	}
	return Result{OK: true, Mode: "clash", IP: ip, PrevIP: req.PrevIP, Changed: ip != req.PrevIP}, nil
}

// probeIPSettled 在预算内反复探测，直到拿到出口 IP。
//
// 单次探测不够用：手机刚关掉飞行模式时网络还没接回来，这时探测必然失败，而调用方会把
// 「探测不到」读成「IP 没换」。给每次探测单独的超时，一次卡死的请求就不会吃掉整个预算。
func probeIPSettled(ctx context.Context, proxyURL string, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var last error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, probeAttemptTimeout)
		ip, err := probeIP(attemptCtx, proxyURL)
		cancel()
		if err == nil && ip != "" {
			return ip, nil
		}
		if err != nil {
			last = err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		if werr := wait(ctx, phoneProbeStep); werr != nil {
			return "", werr
		}
	}
	if last == nil {
		last = errors.New("exit IP probe failed")
	}
	return "", last
}

func probeIP(ctx context.Context, proxyURL string) (string, error) {
	client := outbound.HTTPClient()
	if strings.TrimSpace(proxyURL) != "" {
		clients, err := outbound.New(proxyURL)
		if err != nil {
			return "", err
		}
		if clients != nil && clients.HTTP != nil {
			client = clients.HTTP
		}
	}
	var last error
	for _, endpoint := range ipEndpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			last = err
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if ip := cleanExitIP(string(body)); ip != "" {
			return ip, nil
		}
		last = fmt.Errorf("no exit IP in %s response", endpoint)
	}
	if last == nil {
		last = errors.New("exit IP probe failed")
	}
	return "", last
}

// cleanExitIP 从探测服务的响应里取出出口地址，IPv4 和 IPv6 都要认。
//
// 原先只认 IPv4，这在手机出口上是错的：中国移动的移动网络下发的是 IPv6（实测
// 2409:895a:… ，中国广东深圳移动）。于是 probeIP 永远返回空，rotatePhone 切了 6 次飞行
// 模式仍然判定「探测失败」，最后报 OK:false —— 而 IP 其实每次都换了。手机出口是绕开站点
// 「每 IP 每天 1 次」限制的关键，不能因为地址族判错而报废。
func cleanExitIP(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\r\n \t"); i > 0 {
		text = text[:i]
	}
	ip := net.ParseIP(text)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
