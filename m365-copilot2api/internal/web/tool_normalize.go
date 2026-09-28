package web

import (
	"fmt"
)

// normalizeToolHistory merges interleaved assistant messages to satisfy the
// strict tool protocol: every tool call must be immediately followed by its
// result before another assistant turn. Clients sometimes inject commentary
// between the call and result, which OpenAI's validator rejects.
//
// Example transformation:
//
//	assistant: [tool_call id=X]
//	assistant: "checking the file"        ← interleaved commentary
//	tool: result for id=X
//
// becomes:
//
//	assistant: "checking the file" + [tool_call id=X]
//	tool: result for id=X
func normalizeToolHistory(messages []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, 0, len(messages))
	pendingCalls := []oaiMsg{}

	for i := 0; i < len(messages); i++ {
		m := messages[i]

		// Accumulate assistant messages with tool calls
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			pendingCalls = append(pendingCalls, m)
			continue
		}

		// If we see an assistant message without calls while holding pending calls,
		// merge its content into the first pending call
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && len(pendingCalls) > 0 && messageHasContent(m.Content) {
			first := pendingCalls[0]
			if !messageHasContent(first.Content) {
				first.Content = m.Content
			} else {
				first.Content = fmt.Sprintf("%s\n\n%s", contentToString(first.Content), contentToString(m.Content))
			}
			if m.ReasoningContent != "" {
				if first.ReasoningContent == "" {
					first.ReasoningContent = m.ReasoningContent
				} else {
					first.ReasoningContent += "\n\n" + m.ReasoningContent
				}
			}
			pendingCalls[0] = first
			continue
		}

		// Flush pending calls when we see a tool result or non-assistant message
		if len(pendingCalls) > 0 && (m.Role == "tool" || m.Role != "assistant") {
			out = append(out, pendingCalls...)
			pendingCalls = nil
		}

		out = append(out, m)
	}

	// Flush any remaining calls at the end
	out = append(out, pendingCalls...)
	return out
}
