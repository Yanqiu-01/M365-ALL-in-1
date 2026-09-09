package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

// flattenPromptMessages adapts role-based messages to ChatHub's single text field
// without losing instruction priority or tool-call identity.
func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var b strings.Builder
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		txt, files := parseContent(m.Content)
		attachments = append(attachments, files...)
		txt = strings.TrimSpace(txt)
		if len(m.ToolCalls) > 0 {
			if txt != "" {
				b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
			}
			b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(m.ToolCalls)))
			continue
		}
		if role == "tool" {
			// Image-only (and other non-text) tool results still produce
			// attachments above. If we leave the text empty, the model is told
			// the call returned nothing even though the file is on this turn.
			if strings.TrimSpace(txt) == "" {
				if note := attachmentPresenceNote(files); note != "" {
					txt = note
				}
			}
			txt = compactToolResult(txt, ledgerResultLimit)
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
