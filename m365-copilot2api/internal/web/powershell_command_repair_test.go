package web

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func powerShellRepairTools(names ...string) []map[string]any {
	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []any{"command"},
				},
			},
		})
	}
	return tools
}

func decodedCommand(t *testing.T, call detectedToolCall) string {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	command, ok := args["command"].(string)
	if !ok {
		t.Fatalf("command is not a string: %#v", args["command"])
	}
	return command
}

// 实测（2026-09-07，直连 4141 逐字符探测）：上游的 Markdown 管线会把
// 单 token 类型加速器字面量整段吃掉——[string]::IsNullOrWhiteSpace 到达
// 客户端时只剩 :IsNullOrWhiteSpace，[math]::Ceiling 只剩 :Ceiling，
// [System.Math]::Ceiling 却原样保留（带点的多 token 名字不是 Markdown
// 引用语法，不受影响）。命中面不止两个已知方法，因此修复按
// 「:Method( 形态 + 已知方法名表」整批恢复，而不是逐个方法打补丁。
func TestPowerShellCommandRepairSelectDecision(t *testing.T) {
	tools := powerShellRepairTools("PowerShell")
	candidates := []toolCandidate{{
		Name: "PowerShell",
		Args: map[string]any{"command": "if (-not :IsNullOrWhiteSpace($value)) { :Ceiling(1.5); :GetFolderPath('Desktop') }"},
	}}

	calls, ok := selectDecision(candidates, tools, "auto")
	if !ok || len(calls) != 1 {
		t.Fatalf("selectDecision: ok=%v calls=%+v", ok, calls)
	}
	want := "if (-not [string]::IsNullOrWhiteSpace($value)) { [math]::Ceiling(1.5); [System.Environment]::GetFolderPath('Desktop') }"
	if got := decodedCommand(t, calls[0]); got != want {
		t.Fatalf("command=%q, want %q", got, want)
	}
}

func TestPowerShellCommandRepairRestoresRegexReplace(t *testing.T) {
	tools := powerShellRepairTools("PowerShell")
	command := "$content=:Replace($content, ''(?s)<Row title=\"Theme Mode\".*?<Row title=\"纸张底色\"'', $replacement)"
	calls, ok := selectDecision([]toolCandidate{{Name: "PowerShell", Args: map[string]any{"command": command}}}, tools, "auto")
	if !ok || len(calls) != 1 {
		t.Fatalf("selectDecision: ok=%v calls=%+v", ok, calls)
	}
	want := "$content=[regex]::Replace($content, ''(?s)<Row title=\"Theme Mode\".*?<Row title=\"纸张底色\"'', $replacement)"
	if got := decodedCommand(t, calls[0]); got != want {
		t.Fatalf("command=%q, want %q", got, want)
	}
}

func TestPowerShellPinsRelativeNpmToCdDirectory(t *testing.T) {
	command := "cd E:\\download\\Oh_my_pi\\oh-my-pi\\packages\\desktop; npm run check:types"
	got := repairPowerShellArguments("PowerShell", map[string]any{"command": command})["command"].(string)
	want := "cd E:\\download\\Oh_my_pi\\oh-my-pi\\packages\\desktop; npm --prefix E:\\download\\Oh_my_pi\\oh-my-pi\\packages\\desktop run check:types"
	if got != want {
		t.Fatalf("command=%q, want %q", got, want)
	}
}

func TestPowerShellLeavesPrefixedNpmUnchanged(t *testing.T) {
	command := "cd E:\\download\\Oh_my_pi\\oh-my-pi; npm --prefix packages/desktop run check:types"
	got := repairPowerShellArguments("PowerShell", map[string]any{"command": command})["command"].(string)
	if got != command {
		t.Fatalf("prefixed npm changed: %q", got)
	}
}
func TestPowerShellCommandRepairSelectAllValid(t *testing.T) {
	tools := powerShellRepairTools("powershell")
	candidates := []toolCandidate{
		{Name: "powershell", Args: map[string]any{"command": ":GetFolderPath('Desktop')"}},
		{Name: "powershell", Args: map[string]any{"command": ":IsNullOrWhiteSpace($value)"}},
	}

	calls := selectAllValid(candidates, tools, "auto")
	if len(calls) != 2 {
		t.Fatalf("calls=%+v", calls)
	}
	if got, want := decodedCommand(t, calls[0]), "[System.Environment]::GetFolderPath('Desktop')"; got != want {
		t.Fatalf("first command=%q, want %q", got, want)
	}
	if got, want := decodedCommand(t, calls[1]), "[string]::IsNullOrWhiteSpace($value)"; got != want {
		t.Fatalf("second command=%q, want %q", got, want)
	}
}

// 修复后必须能被 PowerShell 解析执行：整条命令通过 Parser 解析不报错，
// 是「修出来的前缀语义正确」的最直接证据。仅在本机有 powershell 时跑。
func TestPowerShellCommandRepairProducesParseableCommand(t *testing.T) {
	pwsh, err := exec.LookPath("powershell")
	if err != nil {
		t.Skipf("powershell unavailable: %v", err)
	}
	tools := powerShellRepairTools("PowerShell")
	corrupt := "if (-not :IsNullOrWhiteSpace($name)) { :GetFolderPath('Desktop') }"
	calls, ok := selectDecision([]toolCandidate{{Name: "PowerShell", Args: map[string]any{"command": corrupt}}}, tools, "auto")
	if !ok || len(calls) != 1 {
		t.Fatalf("selectDecision: ok=%v n=%d", ok, len(calls))
	}
	cmd := decodedCommand(t, calls[0])
	check := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
		"param([string]$cmd) $errs = $null; $null = [System.Management.Automation.Language.Parser]::ParseInput($cmd, [ref]$null, [ref]$errs); if ($errs) { $errs | ForEach-Object { $_.Message }; exit 1 }", cmd)
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("repaired command is not parseable: %v\ncommand=%q\n%s", err, cmd, out)
	}
}

// 修复不得破坏本来就合法的命令：$env:、label:、以及带点的完整限定名
// [System.Environment]:: 都在 PowerShell 语法里出现 :Method( 形态之外，
// 不得被二次加工。
func TestPowerShellCommandRepairLeavesUnrelatedValuesUnchanged(t *testing.T) {
	command := ":IsNullOrWhiteSpace($value); [System.Environment]::GetFolderPath('Desktop'); Set-Location C:\\Temp; $env:Path; label: while ($true) { break label }"
	repaired := repairPowerShellArguments("PoWeRsHeLl", map[string]any{"command": command})
	got := repaired["command"].(string)

	// :IsNullOrWhiteSpace( 与 :GetFolderPath( 是被吃前缀，必须恢复。
	if !strings.Contains(got, "[string]::IsNullOrWhiteSpace($value)") {
		t.Fatalf("eaten accelerator not restored: %q", got)
	}
	if !strings.Contains(got, "[System.Environment]::GetFolderPath('Desktop')") {
		t.Fatalf("eaten GetFolderPath not restored: %q", got)
	}
	// 完整限定名只出现一次：没有把原本就完整的那份也再拼一层前缀。
	if n := strings.Count(got, "[System.Environment]::GetFolderPath"); n != 1 {
		t.Fatalf("qualified name altered (count=%d): %q", n, got)
	}
	// $env:Path 与 label: 保持原样。
	if !strings.Contains(got, "$env:Path") || !strings.Contains(got, "label: while") {
		t.Fatalf("legal PowerShell syntax changed: %q", got)
	}

	// 非 command 参数不动。
	args := map[string]any{"command": command, "note": ":GetFolderPath("}
	if got := repairPowerShellArguments("PoWeRsHeLl", args)["note"]; got != args["note"] {
		t.Fatalf("non-command argument changed: %#v", got)
	}

	// 非 PowerShell 工具不动：bash 命令里的 :method( 不是 PowerShell 语义。
	nonPowerShell := repairPowerShellArguments("Bash", map[string]any{"command": ":GetFolderPath('Desktop')"})
	if got := nonPowerShell["command"]; got != ":GetFolderPath('Desktop')" {
		t.Fatalf("non-PowerShell command changed: %q", got)
	}
}

func TestPowerShellCommandRepairFencedPaths(t *testing.T) {
	tools := powerShellRepairTools("PowerShell")
	cases := []string{
		"```PowerShell\n{\"command\":\":GetFolderPath('Desktop')\"}\n```",
		"{\"command\":\":GetFolderPath('Desktop')\"}",
	}
	for _, text := range cases {
		calls := fencedToolCalls(text, tools, "auto")
		if len(calls) != 1 {
			t.Fatalf("text=%q calls=%+v", text, calls)
		}
		if got, want := decodedCommand(t, calls[0]), "[System.Environment]::GetFolderPath('Desktop')"; got != want {
			t.Fatalf("text=%q command=%q, want %q", text, got, want)
		}
	}
}
