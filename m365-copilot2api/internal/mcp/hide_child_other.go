//go:build !windows

package mcp

import "os/exec"

// hideChildWindow 在非 Windows 平台无事可做。
func hideChildWindow(cmd *exec.Cmd) {}
