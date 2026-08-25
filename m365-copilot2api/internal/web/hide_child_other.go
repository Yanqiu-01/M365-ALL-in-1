//go:build !windows

package web

import "os/exec"

// hideChildWindow 在非 Windows 平台无事可做：那里没有控制台窗口的概念。
func hideChildWindow(cmd *exec.Cmd) {}
