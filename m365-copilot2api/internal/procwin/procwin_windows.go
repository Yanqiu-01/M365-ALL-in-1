//go:build windows

// Package procwin 只做一件事：让本程序起的子进程不弹控制台窗口。
//
// 为什么要单独一个包：主程序在 cmd/server/hide_console_windows.go 里调用 FreeConsole
// 把自己从控制台分离，之后它启动的每一个控制台子系统程序（adb.exe、python.exe、
// taskkill.exe……）都会被 Windows **新建**一个控制台窗口，于是屏幕上一直闪黑框。
//
// 这段逻辑原先在 internal/web 和 internal/mcp 里各抄了一份，而 internal/exitrotate
// 没有 —— 偏偏 adb 全是它在起，换一次 IP 好几个黑框。把批量注册从 PowerShell 搬进
// 网关本来就是为了不再闪窗，结果换 IP 这条路径把它带回来了。一份实现放在这里，谁起
// 子进程谁调用，就不会再出现「某个包忘了」这种事。
package procwin

import (
	"os/exec"
	"syscall"
)

// createNoWindow 即 CREATE_NO_WINDOW，明确要求不为子进程创建控制台。
// stdout/stderr 仍然通过管道回收，日志不受影响。
const createNoWindow = 0x08000000

// HideWindow 让 cmd 启动的子进程不出现控制台窗口。
//
// 对 GUI 子系统的程序（chrome.exe）本就不会有控制台，这里是无害的；对控制台程序则是
// 唯一能压住黑框的办法。HideWindow 一并设置，覆盖那些会自建窗口的程序。
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
