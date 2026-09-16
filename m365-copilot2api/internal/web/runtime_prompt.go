package web

import (
	"fmt"
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
	return fmt.Sprintf(`[%s]
You are running through the M365 Copilot2API gateway, which executes on the user's own machine: a real computer with a real filesystem.

## What you can do here

The caller's harness gives you real file and shell tools. You can read, create, edit, and delete files on this machine, and run shell commands, exactly as a developer sitting at it would. When you need to see something on disk, read it. When you need to change something, change it.

If a specific tool you want is not offered in this request, say which one you need and ask for it. Do not substitute a guess, and do not describe an edit as though you had made it.

## This machine

%s
The caller's tool session has its own working directory. That directory is not this gateway process's launch folder, and it is not automatically the project being edited. After each shell call the session may reset to the caller's default directory.

Use absolute paths for files, cd, npm, git, and package-manager commands. Relative npm/git commands belong in the target project, not in whatever directory the shell happens to open in. If a declared shell tool has a working_directory / workdir / cwd argument, set it to the target project; a leading cd in the command is not enough by itself.

## Before you write

Confirm where you are before creating or editing files:

%s

Read a file before overwriting it. State a file as created, modified, or verified only after a tool call returned success — never from intent alone.

## One clarification about uploaded files

If a conversation ever references %s, that is where Microsoft 365 keeps files a user uploaded to Copilot's own service. It is unrelated to this machine and does not exist here. The project being edited is the one named in the user's request or in the latest successful Read/Edit path, never this gateway's launch folder.

## Tool etiquette

Do not offer Microsoft 365 or Copilot native tools as a substitute for the caller's local tools. The caller's tools are the ones that can actually see and change this machine.`,
		runtimeWorkspaceMarker,
		describeRuntimeHost(runtime.GOOS, runtime.GOARCH),
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
		// The host shell and the caller's tool are different things. Claude Code
		// declares a Bash tool (which this host serves through Git Bash) while the
		// interactive shell is PowerShell — a prompt that only says "the shell is
		// PowerShell" made the model emit Get-Location/Get-ChildItem into the Bash
		// tool, which died with "command not found". Name both halves, and let the
		// caller's tool description (which says bash/POSIX when it is) win for
		// tool arguments.
		return fmt.Sprintf("Host: Windows/%s, the user's own PC. "+
			"Local or mapped paths such as C:\\ and E:\\ refer to the caller's real filesystem, "+
			"and PowerShell cmdlets like Get-ChildItem, Get-Content and Get-Location run on this host. "+
			"When a declared tool's own description specifies its shell or syntax (for example a bash/POSIX shell tool), "+
			"write commands in THAT tool's dialect, not in PowerShell.\n"+
			"%s", goarch, windowsPathQuotingRule())
	case "linux":
		// GOOS=linux 只说明内核，不说明宿主形态。PC 上这台网关的调用方就是
		// Claude Code / Codex 这类 CLI 编程代理，之前写成「可能是跑 RikkaHub
		// 的手机」会让 PC 场景的模型按手机工作区（/workspace）的假设干活。
		// proot/RikkaHub 只发生在手机上，两者分开表述。
		return fmt.Sprintf("Host: Linux/%s. "+
			"On a PC the caller is a CLI coding agent such as Claude Code or Codex, and this is a real filesystem with a POSIX shell. "+
			"On a phone the gateway may run inside the RikkaHub workspace under proot — also a real filesystem, with the workspace as the project root.", goarch)
	case "darwin":
		return fmt.Sprintf("Host: macOS/%s, the user's own Mac, with a POSIX shell.", goarch)
	default:
		return fmt.Sprintf("Host: %s/%s. Treat the process working directory as the project root.", goos, goarch)
	}
}

// windowsPathQuotingRule 针对 Windows 上「反斜杠路径被两层吃掉」的实测故障。
//
//	POSIX shell 层：Git Bash 里裸写 E:\download\claude 会被当成转义序列吃掉
//	反斜杠，变成 E:downloadclaude。grep 因此报「无此文件或目录」，模型看到的
//	却像是「搜索完成、零结果」，于是据此下结论——2026-09-09 用户实测就是这样
//	丢掉了整批 grep 结果。加引号或改用正斜杠都能正常工作。
//
//	JSON 层：C:\Users 里的 \U 不是合法 JSON 转义，参数整体解析失败。网关侧已
//	有兜底（json_salvage.go），但双反斜杠或正斜杠从一开始就不会触发。
//
// 两层的正确写法是同一个，所以在提示词里合并成一条规则给出。
func windowsPathQuotingRule() string {
	return "Windows path rule, both halves matter: (1) in a POSIX/bash shell tool, always quote a " +
		"backslash path (\"E:\\project\\file.md\") or write it with forward slashes (E:/project/file.md) — " +
		"unquoted, the shell eats the backslashes, so E:\\download\\claude becomes E:downloadclaude and the " +
		"command silently finds nothing; a zero-result grep on a path you did not quote means this, not an " +
		"empty search result. (2) in JSON tool arguments, write backslashes doubled (\"C:\\\\Users\\\\me\\\\a.md\") " +
		"or use forward slashes, because a single backslash before a letter is not a valid JSON escape."
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
