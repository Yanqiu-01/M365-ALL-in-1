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
	"net/url"
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
	// 两个都留零时端口从 SOCKS 地址里取，见 EnsureTunnel —— 端口只能有一个来源，
	// 不能再出现「转发建在 1081、注册打在另一个端口」那种各说各话。
	LocalPort  int
	RemotePort int
	// Binary 是手机上 phone-socks 的路径。
	Binary string
	// SOCKS 是 PC 侧访问这条隧道的地址，用来实测。也是隧道端口的权威来源。
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
	// defaultPhoneSocksBinary 只是 TunnelRequest.Binary 为空时的兜底。真正用哪条路径由
	// 面板配置的 phone_socks_bin 决定 —— 部署位置不该写死在库里。
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
	// socks 必须先定下来，端口再从它取：它就是注册和探测实际打的那个地址。
	//
	// 原先的顺序是反的 —— local 写死 1081，socks 在没配时按 local 拼出来。两个字面量只是
	// 碰巧和 DefaultPhoneSOCKS 相同：面板把 phone_socks 换到别的端口后，adb forward 和
	// phone-socks 仍然建在 1081，而探测和注册走的是配置里的端口。那个端口上要是有别的代理
	// 在听（1080 之类很常见），这里会报「隧道一切正常」并给出一个出口 IP，注册却是从另一个
	// 出口发出的：飞行模式照切、切的是没人在用的手机 IP，站点按那个代理的当日额度判重复，
	// 号可能已经建在站点上而密码没落盘 —— 号段中间又多一个孤号。
	socks := firstNonEmpty(strings.TrimSpace(req.SOCKS), DefaultPhoneSOCKS)
	local := req.LocalPort
	if local <= 0 {
		local = socksPort(socks)
	}
	if local <= 0 {
		// socks 写坏了（解不出端口）才退到默认地址的端口。仍然只有这一个来源。
		local = socksPort(DefaultPhoneSOCKS)
	}
	remote := req.RemotePort
	if remote <= 0 {
		remote = local
	}
	binary := firstNonEmpty(strings.TrimSpace(req.Binary), defaultPhoneSocksBinary)

	var result TunnelResult

	// 先看进程，再看转发：转发建在一个没人监听的端口上，连接会立刻被拒，
	// 那种失败比「端口没转发」更难看懂。
	running, err := phoneSocksRunning(ctx, adb, binary)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("查手机上的 phone-socks 失败: %w", err)
	}
	if !running {
		// 手机上那一头听的是 remote：forward 把 PC 的 local 映到设备的 remote。原先传
		// local，两者一旦不同就是「转发建好了、设备上却没人听」——连接立刻被拒，报错完全
		// 看不出根因。
		if err := startPhoneSocks(ctx, adb, binary, remote); err != nil {
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
	hideChildWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("adb forward --list: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), "tcp:"+strconv.Itoa(local)), nil
}

func adbShellOutput(ctx context.Context, adb, script string) (string, error) {
	cmd := exec.CommandContext(ctx, adb, "shell", script)
	hideChildWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// socksPort 取出 SOCKS 地址里的端口，取不到返回 0。
//
// 手机隧道的端口只能有一个权威来源：注册和探测用的那个 SOCKS 地址。adb forward 在 PC 侧
// 监听的端口必须等于它，否则「转发探测通过」和「注册打得通」说的是两个端口。
func socksPort(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if !strings.Contains(raw, "://") {
		// 配置里只写 host:port 也要认：不补 scheme，url.Parse 会把整段当成路径，端口取空。
		raw = "socks5://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == code
	}
	return false
}
