package web

import (
	"fmt"
	"strings"
	"testing"
)

// 这条提示词存在的唯一理由是压住「我在 /mnt/data」这个幻觉。下面几条断言把
// 上一版踩过的坑固定住,避免以后有人把否定式写法改回去。

func TestRuntimeWorkspaceInstructionLeadsWithCapability(t *testing.T) {
	got := runtimeWorkspaceInstruction()

	// 必须先给能力。模型缺的是「我能读写这台机器」的确信,不是更多禁令。
	for _, want := range []string{
		"real computer with a real filesystem",
		"read, create, edit, and delete files",
		"run shell commands",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction missing affirmative capability statement %q", want)
		}
	}

	// 反过来:不能只剩禁令。
	if strings.Contains(got, "Never assume the workspace is") {
		t.Error("instruction reverted to the prohibition-only phrasing")
	}
}

func TestRuntimeWorkspaceInstructionMentionsUploadPathExactlyOnce(t *testing.T) {
	got := runtimeWorkspaceInstruction()

	// 上一版提了四次,把这个路径推成了上下文里最显眼的路径 —— 正好是要防的。
	// 保留一次是为了解释它是什么;超过一次就是在强化幻觉。
	if n := strings.Count(got, "/mnt/data"); n != 1 {
		t.Errorf("/mnt/data appears %d times, want exactly 1 (as an explanation, not a repeated ban)", n)
	}

	if !strings.Contains(got, "unrelated to this machine") {
		t.Error("the single /mnt/data mention must explain what it actually is")
	}
}

func TestProbeInstructionsMatchTheShell(t *testing.T) {
	// 给错 shell 的探测命令比不给更糟:模型跑失败一次就会断定自己没有文件
	// 系统访问权,然后退回 /mnt/data。
	win := probeInstructions("windows")
	for _, want := range []string{"Get-Location", "Get-ChildItem", "Test-Path"} {
		if !strings.Contains(win, want) {
			t.Errorf("windows probe missing %q", want)
		}
	}
	for _, unwanted := range []string{"pwd", "ls -la", "test -e"} {
		if strings.Contains(win, unwanted) {
			t.Errorf("windows probe suggests POSIX-only %q, which fails in PowerShell", unwanted)
		}
	}

	posix := probeInstructions("linux")
	for _, want := range []string{"pwd", "ls -la"} {
		if !strings.Contains(posix, want) {
			t.Errorf("posix probe missing %q", want)
		}
	}
	if strings.Contains(posix, "Get-ChildItem") {
		t.Error("posix probe suggests a PowerShell cmdlet")
	}
}

func TestDescribeRuntimeHostPerPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want []string
	}{
		{"windows", []string{"Windows/amd64", "user's own PC", "PowerShell", "Get-ChildItem"}},
		{"android", []string{"Android/arm64", "RikkaHub", "/workspace", "writable"}},
		{"linux", []string{"Linux/arm64", "real filesystem"}},
		{"darwin", []string{"macOS/arm64", "own Mac"}},
		{"plan9", []string{"plan9/386", "working directory as the project root"}},
	}
	for _, c := range cases {
		arch := "arm64"
		if c.goos == "windows" {
			arch = "amd64"
		} else if c.goos == "plan9" {
			arch = "386"
		}
		got := describeRuntimeHost(c.goos, arch)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("describeRuntimeHost(%q): missing %q in %q", c.goos, want, got)
			}
		}
	}
}

func TestEnsureRuntimeWorkspaceInstructionIsIdempotent(t *testing.T) {
	first := ensureRuntimeWorkspaceInstruction([]oaiMsg{{Role: "user", Content: "list files"}})
	if len(first) != 2 {
		t.Fatalf("len(first) = %d, want 2", len(first))
	}
	if !strings.EqualFold(first[0].Role, "system") {
		t.Errorf("first message role = %q, want system", first[0].Role)
	}

	second := ensureRuntimeWorkspaceInstruction(first)
	if len(second) != len(first) {
		t.Errorf("instruction inserted twice: len went %d -> %d", len(first), len(second))
	}
}

func TestEnsureRuntimeWorkspaceInstructionRefreshesStaleContinuationContext(t *testing.T) {
	stale := oaiMsg{Role: "system", Content: "[M365-gateway-runtime] only an isolated Linux workspace /mnt/data is available"}
	messages := ensureRuntimeWorkspaceInstruction([]oaiMsg{
		stale,
		{Role: "user", Content: "continue the active goal in E:\\download"},
	})
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2 after replacing stale runtime context", len(messages))
	}
	got := fmt.Sprint(messages[0].Content)
	for _, want := range []string{"M365-gateway-runtime", "user's own PC", "Windows/amd64", "PowerShell", "C:\\", "E:\\"} {
		if !strings.Contains(got, want) {
			t.Errorf("refreshed continuation context missing %q", want)
		}
	}
	if strings.Contains(got, "only an isolated Linux workspace") {
		t.Fatal("stale Linux workspace assertion survived refresh")
	}
	if strings.Count(got, "/mnt/data") != 1 || !strings.Contains(got, "uploaded") {
		t.Fatal("/mnt/data must only be explained as the Copilot upload location")
	}
}
