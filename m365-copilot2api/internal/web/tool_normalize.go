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
//
// Important: This ONLY merges assistant messages WITHOUT tool calls into the
// preceding assistant message WITH tool calls. It does NOT merge multiple
// assistant messages with tool calls together (parallel call splitting),
// which should be rejected by validateToolConversation.
func normalizeToolHistory(messages []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, 0, len(messages))

	for i := 0; i < len(messages); i++ {
		m := messages[i]

		// Check if this is a tool-call assistant message followed by commentary
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Look ahead for commentary (assistant messages without tool calls)
			commentary := []oaiMsg{}
			j := i + 1
			for j < len(messages) && messages[j].Role == "assistant" && len(messages[j].ToolCalls) == 0 {
				if messageHasContent(messages[j].Content) {
					commentary = append(commentary, messages[j])
				}
				j++
			}

			// If we found commentary, merge it into this tool-call message
			if len(commentary) > 0 {
				merged := m
				for _, c := range commentary {
					if !messageHasContent(merged.Content) {
						merged.Content = c.Content
					} else {
						merged.Content = fmt.Sprintf("%s\n\n%s", contentToString(merged.Content), contentToString(c.Content))
					}
					if c.ReasoningContent != "" {
						if merged.ReasoningContent == "" {
							merged.ReasoningContent = c.ReasoningContent
						} else {
							merged.ReasoningContent += "\n\n" + c.ReasoningContent
						}
					}
				}
				out = append(out, merged)
				i = j - 1 // Skip the merged commentary
				continue
			}
		}

		out = append(out, m)
	}

	return out
}
