package web

import (
	"fmt"
	"strings"
)

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Every assistant call must be followed by
// exactly one matching tool result before another model turn is requested.
func validateToolConversation(messages []oaiMsg) error {
	pending := map[string]bool{}
	completed := map[string]bool{}
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("tool results missing before assistant message at index %d", i)
			}
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					return fmt.Errorf("assistant tool call missing id at index %d", i)
				}
				if pending[id] || completed[id] {
					return fmt.Errorf("duplicate tool call id: %s", id)
				}
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required at index %d", i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("unexpected tool result: %s", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
		}
	}
	if len(pending) > 0 {
		for id := range pending {
			return fmt.Errorf("missing tool result for tool_call_id: %s", id)
		}
	}
	return nil
}

// toolCallFunctionName reads the function name a call declares, tolerating both
// the nested {"function":{"name":...}} shape and a flattened {"name":...} one.
func toolCallFunctionName(call map[string]any) string {
	if fn, ok := call["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			return strings.TrimSpace(n)
		}
	}
	n, _ := call["name"].(string)
	return strings.TrimSpace(n)
}

// messageHasContent reports whether a message carries anything a model could
// read. contentToString cannot answer this: a tool-call turn sets content to
// null, and the default branch renders that through fmt.Sprint as the literal
// "<nil>" -- five characters that are not content.
func messageHasContent(c any) bool {
	if c == nil {
		return false
	}
	return strings.TrimSpace(contentToString(c)) != ""
}

// sanitizeToolConversation drops assistant tool calls that carry no id or no
// name, together with the orphan tool results that answer them.
//
// A relay that translates finish_reason but loses the tool_calls payload hands
// the client an empty call; the client answers it with an empty tool_call_id and
// then replays both on every subsequent turn. Rejecting that pair bricks the
// conversation permanently for a defect neither end can repair. The blocks are
// inert -- an unnamed call is not executable and an empty tool_call_id can never
// be matched -- so dropping them loses nothing. Returns the cleaned messages
// plus one note per drop, for the caller to log.
func sanitizeToolConversation(messages []oaiMsg) ([]oaiMsg, []string) {
	var notes []string
	out := make([]oaiMsg, 0, len(messages))
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) == 0 {
				break
			}
			kept := make([]map[string]any, 0, len(m.ToolCalls))
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if strings.TrimSpace(id) == "" || toolCallFunctionName(call) == "" {
					notes = append(notes, fmt.Sprintf("dropped malformed assistant tool call at index %d", i))
					continue
				}
				kept = append(kept, call)
			}
			if len(kept) == len(m.ToolCalls) {
				break
			}
			if len(kept) == 0 {
				m.ToolCalls = nil
				if !messageHasContent(m.Content) {
					notes = append(notes, fmt.Sprintf("dropped empty assistant message at index %d", i))
					continue
				}
			} else {
				m.ToolCalls = kept
			}
		case "tool":
			if strings.TrimSpace(m.ToolCallID) == "" {
				notes = append(notes, fmt.Sprintf("dropped orphan tool result at index %d", i))
				continue
			}
		}
		out = append(out, m)
	}
	return out, notes
}
