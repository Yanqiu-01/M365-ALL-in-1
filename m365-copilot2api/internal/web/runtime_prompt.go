package web

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// runtimeWorkspaceMarker 用来避免多轮对话把同一条环境说明重复塞进去。
const runtimeWorkspaceMarker = "M365-gateway-runtime"

// runtimeWorkspaceInstruction 告诉模型它在用户自己的设备上工作。
//
// 写法上有意先给能力、再给约束。上一版只反复否定 /mnt/data，却要求模型跑
// pwd 和 ls —— 这两个命令在 PowerShell 里不存在，一失败模型就断定自己没有
// 文件系统访问权,于是退回它熟悉的 /mnt/data。提示词自己造成了它要防的幻觉。
//
// 这条提示是给所有协议入口的（OpenAI / Responses / Anthropic 最终都进
// openaiChat）。custom exec 另有一条更严的约束,见 protocol_compat.go。
func runtimeWorkspaceInstruction() string {
	cwd, err := os.Getwd()
	if err != nil || strings.TrimSpace(cwd) == "" {
		cwd = "(unavailable — run the working-directory probe below before touching files)"
	}
	return fmt.Sprintf(`[%s]
You are running through the M365 Copilot2API gateway, which executes on the user's own machine: a real computer with a real filesystem.

## What you can do here

The caller's harness gives you real file and shell tools. You can read, create, edit, and delete files on this machine, and run shell commands, exactly as a developer sitting at it would. When you need to see something on disk, read it. When you need to change something, change it.

If a specific tool you want is not offered in this request, say which one you need and ask for it. Do not substitute a guess, and do not describe an edit as though you had made it.

## This machine

%s
Working directory: %s

Paths here look like the working directory above — not like a Linux container path. Use paths relative to that directory, or absolute paths in the form shown above.

## Before you write

Confirm where you are before creating or editing files:

%s

Read a file before overwriting it. State a file as created, modified, or verified only after a tool call returned success — never from intent alone.

## One clarification about uploaded files

If a conversation ever references %s, that is where Microsoft 365 keeps files a user uploaded to Copilot's own service. It is unrelated to this machine and does not exist here. The working directory above is the only project root.

## Tool etiquette

Do not offer Microsoft 365 or Copilot native tools as a substitute for the caller's local tools. The caller's tools are the ones that can actually see and change this machine.`,
		runtimeWorkspaceMarker,
		describeRuntimeHost(runtime.GOOS, runtime.GOARCH),
		cwd,
		probeInstructions(runtime.GOOS),
		uploadedFilesPath(),
	)
}

// uploadedFilesPath 单独抽出来,让那个容易被模型当成本机路径的字符串在整条
// 提示词里只出现一次。重复提它反而会把它推成上下文里最显眼的路径。
func uploadedFilesPath() string {
	return "/mnt/data"
}

func describeRuntimeHost(goos, goarch string) string {
	switch goos {
	case "android":
		return fmt.Sprintf("Host: Android/%s, inside the RikkaHub workspace on the user's phone. "+
			"The workspace files area is mounted at /workspace (the 软件区) and is a normal writable directory — "+
			"treat it as the project root and write there directly.", goarch)
	case "windows":
		return fmt.Sprintf("Host: Windows/%s, the user's own PC. "+
			"The shell is PowerShell, and local or mapped paths such as C:\\ and E:\\ refer to the caller's real filesystem. "+
			"POSIX-only commands are unavailable: use Get-ChildItem instead of ls, Get-Content instead of cat, "+
			"Get-Location instead of pwd, and $env:NAME instead of $NAME.", goarch)
	case "linux":
		return fmt.Sprintf("Host: Linux/%s. This may be a PC or a phone running RikkaHub under proot; "+
			"either way it is a real filesystem with a POSIX shell.", goarch)
	case "darwin":
		return fmt.Sprintf("Host: macOS/%s, the user's own Mac, with a POSIX shell.", goarch)
	default:
		return fmt.Sprintf("Host: %s/%s. Treat the process working directory as the project root.", goos, goarch)
	}
}

// probeInstructions 给的是这台机器上真的能跑的命令。给错 shell 的命令比不给
// 更糟：模型跑失败一次,就会推断自己没有文件系统。
func probeInstructions(goos string) string {
	if goos == "windows" {
		return "- `Get-Location` to confirm the current directory\n" +
			"- `Get-ChildItem` to list it\n" +
			"- `Test-Path <path>` before assuming a file or directory exists"
	}
	return "- `pwd` to confirm the current directory\n" +
		"- `ls -la` to list it\n" +
		"- `test -e <path>` before assuming a file or directory exists"
}

// ensureRuntimeWorkspaceInstruction 把当前运行时环境说明放在消息列表最前。
// previous_response_id、目标 continuation 和子代理历史可能带有旧版环境说明,
// 因此命中 marker 时刷新该说明,不能沿用声称 Linux /mnt/data 的旧文本。
func ensureRuntimeWorkspaceInstruction(messages []oaiMsg) []oaiMsg {
	instruction := runtimeWorkspaceInstruction()
	out := make([]oaiMsg, 0, len(messages)+1)
	out = append(out, oaiMsg{Role: "system", Content: instruction})
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") &&
			strings.Contains(fmt.Sprint(message.Content), runtimeWorkspaceMarker) {
			continue
		}
		out = append(out, message)
	}
	return out
}

// attachRuntimeIdentityToIncrement re-prepends the runtime-marker system message
// onto an incremental answer prompt. Session Bind stores the injected system at
// index 0, so HistoryLen slices it off on turn 2+; without this, ChatHub only
// sees the new user/tool turn and the cloud identity (Linux sandbox) reasserts.
// Only the gateway runtime marker is re-attached — not the caller's full harness.
func attachRuntimeIdentityToIncrement(answerPrompt string) string {
	answerPrompt = strings.TrimSpace(answerPrompt)
	if answerPrompt == "" {
		return answerPrompt
	}
	if strings.Contains(answerPrompt, runtimeWorkspaceMarker) {
		return answerPrompt
	}
	identity, _ := flattenPromptMessages([]oaiMsg{{
		Role:    "system",
		Content: runtimeWorkspaceInstruction(),
	}}, nil)
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return answerPrompt
	}
	return identity + "\n\n" + answerPrompt
}
