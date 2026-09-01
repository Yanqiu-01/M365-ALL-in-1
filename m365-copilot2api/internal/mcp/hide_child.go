package mcp

import (
	"os/exec"

	"m365-copilot2api/internal/procwin"
)

// hideChildWindow 抑制 MCP 子进程的控制台窗口。实现在 internal/procwin，跨平台差异
// 由那个包的构建标记处理。
func hideChildWindow(cmd *exec.Cmd) { procwin.HideWindow(cmd) }
