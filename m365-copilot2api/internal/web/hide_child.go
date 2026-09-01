package web

import (
	"os/exec"

	"m365-copilot2api/internal/procwin"
)

// hideChildWindow 让注册 / OAuth 的子进程不弹控制台窗口。实现在 internal/procwin，
// 跨平台差异由那个包的构建标记处理。
func hideChildWindow(cmd *exec.Cmd) { procwin.HideWindow(cmd) }
