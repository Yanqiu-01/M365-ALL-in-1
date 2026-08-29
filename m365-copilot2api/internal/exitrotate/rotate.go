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
	ipEndpoints        = []string{
		"https://api.ipify.org",
		"https://icanhazip.com",
		"https://ifconfig.me/ip",
	}
	clashSettle = 3 * time.Second
	phoneSettle = 3 * time.Second
)

// Rotate 先尝试 Rust CLI，失败再走 Go 实现。
func Rotate(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bin := findCLI(); bin != "" {
		if result, err := rotateCLI(ctx, bin, req); err == nil {
			return result, nil
		}
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
	if err == nil && ip != "" && ip != prev {
		return Result{OK: true, Mode: "phone", IP: ip, PrevIP: prev, Changed: prev != "", Detail: "current exit already differs"}, nil
	}
	if prev == "" {
		prev = ip
	}
	var last string
	for i := 0; i < 6; i++ {
		if err := toggleAirplane(ctx, req.ADB); err != nil {
			return Result{Mode: "phone", PrevIP: prev, Detail: err.Error()}, err
		}
		ip, err = probeIP(ctx, socks)
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
	detail := ""
	if expect := strings.TrimSpace(req.ExpectIP); expect != "" && expect != ip {
		detail = "exit IP " + ip + " differs from expected " + expect
	}
	return Result{OK: true, Mode: "clash", IP: ip, PrevIP: req.PrevIP, Changed: ip != req.PrevIP, Detail: detail}, nil
}

func probeIP(ctx context.Context, proxyURL string) (string, error) {
	client := http.DefaultClient
	if strings.TrimSpace(proxyURL) != "" {
		clients, err := outbound.New(proxyURL)
		if err != nil {
			return "", err
		}
		client = clients.HTTP
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
		if ip := cleanIPv4(string(body)); ip != "" {
			return ip, nil
		}
		last = fmt.Errorf("no IPv4 in %s response", endpoint)
	}
	if last == nil {
		last = errors.New("exit IP probe failed")
	}
	return "", last
}

func cleanIPv4(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\r\n \t"); i > 0 {
		text = text[:i]
	}
	ip := net.ParseIP(text)
	if ip == nil || ip.To4() == nil {
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
