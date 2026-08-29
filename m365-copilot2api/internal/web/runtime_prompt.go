package web

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// runtimeWorkspaceMarker 用来避免多轮对话把同一条环境说明重复塞进去。
const runtimeWorkspaceMarker = "M365-gateway-runtime"

// runtimeWorkspaceInstruction 告诉模型它在用户自己的 Windows / Android 设备上
// 工作，而不是云端沙箱，更不是 /mnt/data。
//
// 这条提示是给所有协议入口的（OpenAI / Responses / Anthropic 最终都进
// openaiChat）。custom exec 另有一条更严的约束，见 protocol_compat.go。
func runtimeWorkspaceInstruction() string {
	cwd, err := os.Getwd()
	if err != nil || strings.TrimSpace(cwd) == "" {
		cwd = "(unknown — inspect with pwd before writing files)"
	}
	host := describeRuntimeHost(runtime.GOOS, runtime.GOARCH)
	return fmt.Sprintf(`[%s]
You are running through the M365 Copilot2API gateway on the user's own device.
%s
Process working directory: %s
Rules:
- This is a real Windows or Android environment, not a cloud sandbox and not /mnt/data.
- Never assume the workspace is /mnt/data, /root, /tmp, or any other guessed absolute path.
- On Android / RikkaHub the files area is typically /workspace (the 软件区). On Windows it is the directory from which the gateway was started. Always inspect pwd and ls before creating or editing files.
- Use relative paths from the current working directory. Do not invent another project root.
- Do not mention Microsoft 365 native tools as a substitute for the caller's local tools.`, runtimeWorkspaceMarker, host, cwd)
}

func describeRuntimeHost(goos, goarch string) string {
	switch goos {
	case "android":
		return fmt.Sprintf("Host OS: Android/%s. You are inside the Android app or RikkaHub workspace on the user's phone.", goarch)
	case "windows":
		return fmt.Sprintf("Host OS: Windows/%s. You are on the user's Windows PC.", goarch)
	case "linux":
		return fmt.Sprintf("Host OS: Linux/%s. This may be a PC or a phone running RikkaHub proot; still not /mnt/data. Treat the process working directory as the only project root.", goarch)
	case "darwin":
		return fmt.Sprintf("Host OS: macOS/%s. Treat the process working directory as the project root.", goarch)
	default:
		return fmt.Sprintf("Host OS: %s/%s. Treat the process working directory as the project root. Do not assume /mnt/data exists.", goos, goarch)
	}
}

// ensureRuntimeWorkspaceInstruction 把环境说明放在消息列表最前。已有同一条
// marker 时不重复插入，避免 previous_response_id / 多轮工具调用把提示词堆叠。
func ensureRuntimeWorkspaceInstruction(messages []oaiMsg) []oaiMsg {
	instruction := runtimeWorkspaceInstruction()
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") &&
			strings.Contains(fmt.Sprint(message.Content), runtimeWorkspaceMarker) {
			return messages
		}
	}
	out := make([]oaiMsg, 0, len(messages)+1)
	out = append(out, oaiMsg{Role: "system", Content: instruction})
	return append(out, messages...)
}
