package exitrotate

import (
	"os/exec"

	"m365-copilot2api/internal/procwin"
)

// hideChildWindow 让子进程不弹控制台窗口。这个包起的全是控制台程序 —— adb.exe 在每次
// 换 IP 时要跑好几次（forward --list、shell、飞行模式开关），不压住就是每注册一个号闪
// 好几个黑框。实现在 internal/procwin，跨平台差异由那个包的构建标记处理。
func hideChildWindow(cmd *exec.Cmd) { procwin.HideWindow(cmd) }
