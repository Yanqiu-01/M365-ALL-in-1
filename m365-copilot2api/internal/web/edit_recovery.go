package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

const editRecoveryMaxFiles = 4
const editRecoveryPathLimit = 512

// editFailureReason classifies actual Edit errors, not arbitrary mentions of an
// error in successful output or source text. Anthropic's is_error flag is kept
// by the adapter as "Error: "; OpenAI clients may send the bare tool error.
func editFailureReason(result string) string {
	text := strings.TrimSpace(result)
	if strings.HasPrefix(strings.ToLower(text), "error:") {
		text = strings.TrimSpace(text[len("error:"):])
	}
	if strings.HasPrefix(text, "<tool_use_error>") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "<tool_use_error>"))
	}
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, "string to replace not found"):
		return "old_string was not found in the current file"
	case strings.HasPrefix(lower, "file has been modified since read"):
		return "the client's read snapshot is stale"
	case strings.HasPrefix(lower, "no changes to make:") && strings.Contains(lower, "old_string") && strings.Contains(lower, "new_string"):
		return "old_string and new_string were identical; no change was made"
	}
	return ""
}

// editRecoveryInstruction is a correction on the NEXT normal model turn, when
// the caller actually returns the failed tool result. The answer-eject hooks
// only see model prose and run too late for router tool-call early returns.
//
// Only the current trailing batch of results can request recovery. A later
// Read, assistant answer or user request retires the old hint. This is bounded
// by file count, adds no upstream retry loop, never replays a write, and honours
// tool_choice and the available tools. The existing round/deadline limits apply.
func editRecoveryInstruction(messages []oaiMsg, ledger agentLedger, tools []map[string]any, choice any) string {
	readName := ""
	for _, tool := range tools {
		if name := toolCallFunctionName(tool); strings.EqualFold(name, "Read") && toolChoiceAllows(choice, name) {
			readName = name
			break
		}
	}
	if readName == "" || len(messages) == 0 {
		return ""
	}
	start := len(messages)
	for start > 0 && strings.EqualFold(strings.TrimSpace(messages[start-1].Role), "tool") {
		start--
	}
	if start == len(messages) {
		return ""
	}
	completed := make(map[string]toolEvidence, len(ledger.Completed))
	for _, entry := range ledger.Completed {
		completed[entry.ID] = entry
	}
	seen := make(map[string]bool)
	var notes []string
	for i := len(messages) - 1; i >= start && len(notes) < editRecoveryMaxFiles; i-- {
		message := messages[i]
		entry, ok := completed[message.ToolCallID]
		if !ok || !strings.EqualFold(entry.Name, "Edit") {
			continue
		}
		reason := editFailureReason(toolResultEvidenceText(message.Content))
		if reason == "" {
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal([]byte(entry.Arguments), &args) != nil || strings.TrimSpace(args.FilePath) == "" || seen[args.FilePath] {
			continue
		}
		seen[args.FilePath] = true
		// Quote metadata rather than splicing it in as instructions. An unusual
		// huge path stays in the original call; truncating it into a different
		// path would direct the model to the wrong file.
		target := "the file named in the failed Edit call"
		if quoted := mustJSON(args.FilePath); len(quoted) <= editRecoveryPathLimit {
			target = "file " + quoted
		}
		notes = append(notes, fmt.Sprintf("For %s, the last Edit failed: %s.", target, reason))
	}
	if len(notes) == 0 {
		return ""
	}
	return "[edit-recovery]\n" + strings.Join(notes, "\n") +
		"\nA failed Edit is not completed work. The next action for each affected file is a fresh " + readName +
		" call for the relevant range. Use that result to reconstruct old_string from the current file content, excluding only the line-number gutter and preserving indentation. Prepare distinct old_string and new_string values using standard JSON escaping. The mismatch alone does not establish whether indentation, escaping or a file change caused it."
}
