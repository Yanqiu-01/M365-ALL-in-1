//go:build windows

package web

import (
	"os/exec"
	"syscall"
	"testing"
)

// 回归：注册/OAuth 的子进程必须带 CREATE_NO_WINDOW。主程序在启动时调用了
// FreeConsole，之后 Windows 会为每个控制台子系统的子进程新建一个可见窗口，
// 表现为跑注册/OAuth 时闪出黑框。
func TestHideChildWindowSetsNoWindowFlag(t *testing.T) {
	const createNoWindow = 0x08000000

	cmd := exec.Command("cmd", "/c", "exit")
	hideChildWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; the console window would still appear")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow = false, want true")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CreationFlags = %#x, missing CREATE_NO_WINDOW (%#x)",
			cmd.SysProcAttr.CreationFlags, createNoWindow)
	}

	// 既有的 CreationFlags 必须被保留而不是覆盖。
	const someExistingFlag = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	cmd2 := exec.Command("cmd", "/c", "exit")
	cmd2.SysProcAttr = &syscall.SysProcAttr{CreationFlags: someExistingFlag}
	hideChildWindow(cmd2)
	if cmd2.SysProcAttr.CreationFlags&someExistingFlag == 0 {
		t.Error("pre-existing CreationFlags were dropped")
	}
	if cmd2.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Error("CREATE_NO_WINDOW not added alongside existing flags")
	}

	// nil 入参不得 panic。
	hideChildWindow(nil)
}
