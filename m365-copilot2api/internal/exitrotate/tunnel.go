package exitrotate

// 手机出口的隧道维护。
//
// 注册走的是「PC 上的 Chrome -> adb forward -> 手机上的 phone-socks -> 运营商网络」，
// 这条链路上 adb forward 会随拔线/重启消失，phone-socks 也可能被系统回收。任何一环断掉，
// 注册的表现都是「注册表单没有出现」——一个完全看不出根因的错误。所以每批之前实测一次，
// 断了就地修复。
//
// 刻意不做的事：不往手机上推二进制。推送属于部署，网关不该在跑批的中途改手机上的文件；
// 找不到可执行文件就如实报错，让人去部署，而不是悄悄换掉一个正在用的东西。

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// TunnelRequest 描述一次隧道检查。
type TunnelRequest struct {
	// ADB 是 adb 可执行文件路径。空则用 PATH 上的 "adb"。
	ADB string
	// LocalPort / RemotePort 是 adb forward 的两端，通常相同。
	LocalPort  int
	RemotePort int
	// Binary 是手机上 phone-socks 的路径。
	Binary string
	// SOCKS 是 PC 侧访问这条隧道的地址，用来实测。
	SOCKS string
}

// TunnelResult 报告隧道状态。IP 是实测拿到的手机出口地址。
type TunnelResult struct {
	OK      bool   `json:"ok"`
	IP      string `json:"ip,omitempty"`
	Forward bool   `json:"forwardRepaired"`
	Process bool   `json:"processRestarted"`
	Detail  string `json:"detail,omitempty"`
}

const (
	defaultPhoneSocksBinary = "/data/local/tmp/phone-socks"
	tunnelProbeBudget       = 20 * time.Second
)

// EnsureTunnel 确认手机隧道可用，必要时修复 adb forward 和 phone-socks，最后实测出口 IP。
//
// 返回的 error 只表示「修不好」。修好了但中途做过修复动作，会在 Result 里如实标出来 ——
// 隧道反复需要修复本身就是一个信号（线松了、手机在回收进程），不该被静默掉。
func EnsureTunnel(ctx context.Context, req TunnelRequest) (TunnelResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	adb := firstNonEmpty(strings.TrimSpace(req.ADB), "adb")
	local := req.LocalPort
	if local <= 0 {
		local = 1081
	}
	remote := req.RemotePort
	if remote <= 0 {
		remote = local
	}
	binary := firstNonEmpty(strings.TrimSpace(req.Binary), defaultPhoneSocksBinary)
	socks := firstNonEmpty(strings.TrimSpace(req.SOCKS), "socks5://127.0.0.1:"+strconv.Itoa(local))

	var result TunnelResult

	// 先看进程，再看转发：转发建在一个没人监听的端口上，连接会立刻被拒，
	// 那种失败比「端口没转发」更难看懂。
	running, err := phoneSocksRunning(ctx, adb, binary)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("查手机上的 phone-socks 失败: %w", err)
	}
	if !running {
		if err := startPhoneSocks(ctx, adb, binary, local); err != nil {
			result.Detail = err.Error()
			return result, err
		}
		result.Process = true
		if err := wait(ctx, 2*time.Second); err != nil {
			return result, err
		}
	}

	forwarded, err := adbForwardPresent(ctx, adb, local)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("查 adb forward 失败: %w", err)
	}
	if !forwarded {
		spec := "tcp:" + strconv.Itoa(local)
		if err := runCmd(ctx, adb, "forward", spec, "tcp:"+strconv.Itoa(remote)); err != nil {
			result.Detail = err.Error()
			return result, fmt.Errorf("重建 adb forward 失败: %w", err)
		}
		result.Forward = true
	}

	ip, err := probeIPSettled(ctx, socks, tunnelProbeBudget)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("手机隧道通了但探不到出口 IP: %w", err)
	}
	result.OK, result.IP = true, ip
	return result, nil
}

// phoneSocksRunning 用 ps 查进程。
//
// 只看名字而不看完整命令行：安卓的 ps 输出格式在各版本间不一致，实测 CDY-AN90(SDK 29)
// 的 `ps -A` 只给到可执行文件名。
func phoneSocksRunning(ctx context.Context, adb, binary string) (bool, error) {
	name := binary
	if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	out, err := adbShellOutput(ctx, adb, "ps -A 2>/dev/null | grep "+name+" | grep -v grep")
	if err != nil {
		// grep 没匹配到时退出码为 1，这不是错误，是「没在跑」。
		if isExitCode(err, 1) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(out, name), nil
}

// startPhoneSocks 在手机上后台拉起 phone-socks。
func startPhoneSocks(ctx context.Context, adb, binary string, port int) error {
	exists, err := adbShellOutput(ctx, adb, "test -x "+binary+" && echo yes || echo no")
	if err != nil {
		return fmt.Errorf("检查 %s 是否可执行失败: %w", binary, err)
	}
	if !strings.Contains(exists, "yes") {
		return fmt.Errorf("手机上没有可执行的 %s：请先 adb push 并 chmod 755", binary)
	}
	cmd := fmt.Sprintf("nohup %s -listen 127.0.0.1:%d -quiet >/data/local/tmp/phone-socks.log 2>&1 &",
		binary, port)
	if _, err := adbShellOutput(ctx, adb, cmd); err != nil {
		return fmt.Errorf("拉起 phone-socks 失败: %w", err)
	}
	return nil
}

func adbForwardPresent(ctx context.Context, adb string, local int) (bool, error) {
	cmd := exec.CommandContext(ctx, adb, "forward", "--list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("adb forward --list: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), "tcp:"+strconv.Itoa(local)), nil
}

func adbShellOutput(ctx context.Context, adb, script string) (string, error) {
	cmd := exec.CommandContext(ctx, adb, "shell", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == code
	}
	return false
}
