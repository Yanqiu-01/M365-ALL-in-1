//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// hideChildWindow 抑制 MCP 子进程的控制台窗口。
// 主程序调用了 FreeConsole，因此控制台子系统的子进程会另开一个可见窗口。
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
