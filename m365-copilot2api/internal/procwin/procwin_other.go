//go:build !windows

package procwin

import "os/exec"

// HideWindow 在非 Windows 平台无事可做：那里没有控制台窗口的概念。
func HideWindow(cmd *exec.Cmd) {}
