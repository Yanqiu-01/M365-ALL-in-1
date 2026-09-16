package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

// flattenPromptMessages adapts role-based messages to ChatHub's single text field
// without losing instruction priority or tool-call identity.
func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	return flattenPromptMessagesWithToolNames(messages, attachments, nil)
}

func flattenPromptMessagesWithToolNames(messages []oaiMsg, attachments []chathub.Attachment, priorNames map[string]string) (string, []chathub.Attachment) {
	names := make(map[string]string, len(priorNames))
	for id, name := range priorNames {
		names[id] = name
	}
	var b strings.Builder
	for _, m := range messages {
		recordToolCallNames(names, m)
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		txt, files := parseContent(m.Content)
		attachments = append(attachments, files...)
		// A final empty numbered line ends in the gutter TAB itself. Keep it
		// until promptToolResult has recognised the listing; trimming first
		// would turn "2\t\n" into "2" and prevent normalisation of every row.
		if role != "tool" || len(m.ToolCalls) > 0 {
			txt = strings.TrimSpace(txt)
		}
		if len(m.ToolCalls) > 0 {
			if txt != "" {
				b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
			}
			b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(promptToolCalls(m.ToolCalls))))
			continue
		}
		if role == "tool" {
			txt = promptToolResult(toolResultName(m, names), txt, files)
			// 空结果要说出来，不能渲染成一个空的标题行。
			//
			// 早先内容为空时这里写出的是 "[tool result id=x]" 后面跟一个空行，模型
			// 无法区分「工具成功但没有输出」「工具失败了」和「结果在链路上丢了」——
			// 于是它只能含糊其辞。网关看不到退出码（那是调用方该带上的），但至少
			// 要如实说明自己收到的是一个空结果，而不是假装那里有内容。
			if strings.TrimSpace(txt) == "" {
				b.WriteString(fmt.Sprintf(
					"\n[tool result id=%s]\n(the caller returned this tool result with no content; "+
						"it does not indicate success or failure — if you need to know the outcome, verify it with another call)\n",
					m.ToolCallID))
				continue
			}
			b.WriteString(fmt.Sprintf("\n[tool result id=%s]\n%s\n", m.ToolCallID, txt))
			continue
		}
		if txt == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
	}
	return strings.TrimSpace(b.String()), attachments
}
