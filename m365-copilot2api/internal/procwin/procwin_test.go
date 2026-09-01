package procwin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	spawnRe = regexp.MustCompile(`exec\.Command(Context)?\(`)
	hideRe  = regexp.MustCompile(`hideChildWindow\(|procwin\.HideWindow\(|HideWindow\(`)
)

// 回归：每一个起子进程的地方都必须压掉控制台窗口。
//
// 这个测试扫源码而不是测行为，因为这类缺陷的形态就是「某个包忘了调」—— 行为测试只能
// 覆盖已经想到的调用点，想不到的那个正是会漏的那个。实际发生过：internal/web 和
// internal/mcp 各有一份 hideChildWindow，而 internal/exitrotate 没有，偏偏 adb.exe
// 全是它在起，于是换一次 IP 闪好几个黑框 —— 把批量注册搬进网关本来就是为了不闪窗。
//
// 同样的扫源码手法在 internal/web/web_assets_test.go 里已经用过（从 panel.html 里提出
// /api/ 字面量，断言每个都已路由）。
func TestEverySubprocessSpawnHidesItsWindow(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "web", "docs", "assets", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !spawnRe.MatchString(line) {
				continue
			}
			// 只看紧随其后的几条语句：约定是构造完 cmd 立刻压窗口，隔太远就等于没有约定。
			// 按语句数算而不是行数 —— 注释和空行不是语句，一段三行的说明不该把调用挤出窗口。
			if hideFollows(lines[i:], 3) {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			offenders = append(offenders, filepath.ToSlash(rel)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("这些地方起了子进程但没有压掉控制台窗口（在构造 cmd 之后调用 procwin.HideWindow，"+
			"或本包内的 hideChildWindow 包装）：\n  %s", strings.Join(offenders, "\n  "))
	}
}

// hideFollows 判断 lines[0]（构造 cmd 的那一行）之后 limit 条语句内有没有压窗口的调用。
// 注释行和空行跳过，不计入 limit。
func hideFollows(lines []string, limit int) bool {
	seen := 0
	for _, line := range lines {
		if hideRe.MatchString(line) {
			return true
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		seen++
		if seen > limit {
			return false
		}
	}
	return false
}

// moduleRoot 从测试所在目录向上找 go.mod。测试的工作目录是它自己的包目录，而这个测试
// 要看整个模块。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("往上找不到 go.mod，无法确定模块根目录")
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
