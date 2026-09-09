package web

import (
	"runtime"
	"strings"
	"testing"
)

// 两条提示词路径都必须带上 Windows 路径规则：答案轮走
// runtimeWorkspaceInstruction，路由轮不经过它、单独组装。此前 Transit note
// 就是只加了一处，路由轮照旧出错。
func TestWindowsPathRuleInRuntimeHostDescription(t *testing.T) {
	desc := describeRuntimeHost("windows", "amd64")
	for _, want := range []string{"quote", "forward slashes", "E:downloadclaude", "not a valid JSON escape"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("Windows 宿主说明缺少 %q:\n%s", want, desc)
		}
	}
}

func TestWindowsPathRuleAbsentOnOtherHosts(t *testing.T) {
	// 别的宿主没有这个问题，规则不该出现在那里挤占注意力。
	for _, goos := range []string{"linux", "darwin", "android"} {
		if desc := describeRuntimeHost(goos, "amd64"); strings.Contains(desc, "E:downloadclaude") {
			t.Fatalf("%s 宿主说明混入了 Windows 路径规则:\n%s", goos, desc)
		}
	}
}

func TestRouterPromptCarriesWindowsPathRule(t *testing.T) {
	prompt := modelToolRouterPrompt("列出 E 盘那个目录", []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash"}},
	}, "auto")
	if runtime.GOOS != "windows" {
		if strings.Contains(prompt, "Windows path rule") {
			t.Fatalf("非 Windows 宿主不该带 Windows 路径规则")
		}
		t.Skipf("非 Windows 宿主（%s），路由轮无需这条规则", runtime.GOOS)
	}
	for _, want := range []string{"Windows path rule", "not a valid JSON escape", "quote the path"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("路由轮提示词缺少 %q", want)
		}
	}
	// Transit note 必须还在：两条规则共用同一段前言，改动容易互相顶掉。
	if !strings.Contains(prompt, "[ string]::") {
		t.Fatal("路由轮提示词丢了 Transit note")
	}
}
