//go:build windows

package web

import (
	"os/exec"
	"syscall"
)

// hideChildWindow 让子进程不弹控制台窗口。
//
// 为什么必须显式设置：主程序在 cmd/server/hide_console_windows.go 里调用了
// FreeConsole 把自己从控制台分离。分离之后进程没有控制台可继承，Windows 在
// 启动控制台子系统的子进程（python.exe、taskkill.exe）时就会**新建**一个
// 控制台窗口 —— 于是每跑一次注册/OAuth 就闪出一个黑框。
//
// CREATE_NO_WINDOW 明确要求不创建控制台。stdout/stderr 仍然通过管道回收，
// 因此面板里的日志不受影响。HideWindow 一并设置，覆盖那些会自建窗口的程序。
func hideChildWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	const createNoWindow = 0x08000000
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
