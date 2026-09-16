package web

import (
	"encoding/json"
	"strconv"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// recordToolCallNames only learns identities from assistant calls, never from
// tool output text. A seed from the full history also covers result-only
// increments whose assistant call is already in the upstream conversation.
func recordToolCallNames(names map[string]string, message oaiMsg) {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
		return
	}
	for _, call := range message.ToolCalls {
		id, _ := call["id"].(string)
		if name := toolCallFunctionName(call); id != "" && name != "" {
			names[id] = name
		}
	}
}

func toolCallNames(messages []oaiMsg) map[string]string {
	names := make(map[string]string)
	for _, message := range messages {
		recordToolCallNames(names, message)
	}
	return names
}

func toolResultName(message oaiMsg, names map[string]string) string {
	if name := names[message.ToolCallID]; name != "" {
		return name
	}
	return message.Name // Some OpenAI callers supply the optional tool name.
}

// normalizeReadGutter changes only the ONE delimiter TAB in a recognised Read
// listing. It preserves padding, content indentation, CRLF, empty lines and
// reminders byte-for-byte. Unknown formats (including already-rendered arrows),
// error messages, other tools and non-increasing line numbers are left alone.
// Work line by line: a multiline ^\s* regexp would also consume blank lines.
func normalizeReadGutter(name, text string) string {
	if !strings.EqualFold(strings.TrimSpace(name), "Read") {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	delimiters := make([]int, len(lines))
	previous, rows := 0, 0
	reminder := false
	for i, line := range lines {
		delimiters[i] = -1
		trimmed := strings.TrimSpace(line)
		if reminder {
			if strings.Contains(trimmed, "</system-reminder>") {
				reminder = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "<system-reminder>") {
			reminder = !strings.Contains(trimmed, "</system-reminder>")
			continue
		}
		if trimmed == "" {
			continue
		}
		start := 0
		for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
			start++
		}
		end := start
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		if end == start || end == len(line) || line[end] != '\t' {
			return text
		}
		number, err := strconv.Atoi(line[start:end])
		if err != nil || number <= previous {
			return text
		}
		previous, rows = number, rows+1
		delimiters[i] = end
	}
	if rows == 0 || reminder {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 2*rows)
	for i, line := range lines {
		if at := delimiters[i]; at >= 0 {
			b.WriteString(line[:at])
			b.WriteString("→")
			b.WriteString(line[at+1:])
		} else {
			b.WriteString(line)
		}
	}
	return b.String()
}

// promptToolResult is shared by rendering and budgeting. Normalisation happens
// BEFORE truncation, which can otherwise cut a line number or split a listing.
func promptToolResult(name, text string, files []chathub.Attachment) string {
	if strings.TrimSpace(text) == "" {
		if note := attachmentPresenceNote(files); note != "" {
			text = note
		}
	}
	return compactToolResult(normalizeReadGutter(name, text), toolResultPromptLimit)
}

// promptArguments removes the JSON-in-a-JSON-string presentation layer, not any
// escaping INSIDE an argument. RawMessage avoids float64 rounding of large IDs.
// History is evidence: invalid/truncated/non-object arguments stay verbatim and
// never pass through the model-output salvage heuristics.
func promptArguments(arguments string) any {
	trimmed := strings.TrimSpace(arguments)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return arguments
}

func promptToolCalls(calls []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		// Copy both maps: the original is also used for validation, session
		// identity, deduplication and the client-facing protocol.
		display := make(map[string]any, len(call))
		for k, v := range call {
			display[k] = v
		}
		if fn, ok := call["function"].(map[string]any); ok {
			f := make(map[string]any, len(fn))
			for k, v := range fn {
				f[k] = v
			}
			if arguments, ok := fn["arguments"].(string); ok {
				f["arguments"] = promptArguments(arguments)
			}
			display["function"] = f
		}
		out = append(out, display)
	}
	return out
}

type promptToolEvidence struct {
	toolEvidence
	Arguments any `json:"arguments"`
}

func promptEvidence(entries []toolEvidence) []promptToolEvidence {
	if entries == nil {
		return nil
	}
	out := make([]promptToolEvidence, len(entries))
	for i, entry := range entries {
		out[i] = promptToolEvidence{toolEvidence: entry, Arguments: promptArguments(entry.Arguments)}
	}
	return out
}
