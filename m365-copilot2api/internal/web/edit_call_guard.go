package web

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

type editReadRequirement struct {
	path     string
	failedAt int
}

type editCallGuard struct {
	needsRead map[string]editReadRequirement
}

// Client paths are metadata, not paths on the gateway's filesystem. Preserve
// Unix case while treating drive-qualified/UNC Windows spellings consistently.
func editGuardPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	windows := len(value) >= 2 && value[1] == ':' || strings.HasPrefix(value, "//")
	value = path.Clean(value)
	if windows {
		value = strings.ToLower(value)
	}
	return value
}

func newEditCallGuard(messages []oaiMsg) *editCallGuard {
	g := &editCallGuard{needsRead: make(map[string]editReadRequirement)}
	type call struct {
		name, file string
		issuedAt   int
	}
	calls := map[string]call{}
	for index, message := range messages {
		if message.Role == "assistant" {
			for _, raw := range message.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				var args struct {
					FilePath string `json:"file_path"`
				}
				if json.Unmarshal([]byte(toolArgumentsJSON(raw)), &args) == nil && args.FilePath != "" {
					calls[id] = call{name: name, file: args.FilePath, issuedAt: index}
				}
			}
		}
		if message.Role != "tool" {
			continue
		}
		entry, ok := calls[message.ToolCallID]
		if !ok {
			continue
		}
		key := editGuardPath(entry.file)
		result := toolResultEvidenceText(message.Content)
		if strings.EqualFold(entry.name, "Edit") && editFailureReason(result) != "" {
			g.needsRead[key] = editReadRequirement{path: entry.file, failedAt: index}
		}
		if strings.EqualFold(entry.name, "Read") && !toolResultLooksFailed(entry.name, result) {
			if requirement, exists := g.needsRead[key]; exists && entry.issuedAt > requirement.failedAt {
				delete(g.needsRead, key)
			}
		}
	}
	return g
}

// Guard at final emission, after ledger dedupe and every repair/fallback. A
// restored duplicate candidate must never bypass read-before-edit recovery.
// Replacement reads are non-mutating, use the failed call's exact path, and
// must validate against the client's schema and tool_choice like any other call.
func (g *editCallGuard) prepare(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, error) {
	if g == nil || len(g.needsRead) == 0 {
		return calls, nil
	}
	readName := ""
	for _, tool := range tools {
		name := toolCallFunctionName(tool)
		if strings.EqualFold(name, "Read") && toolChoiceAllows(choice, name) {
			readName = name
			break
		}
	}
	out := make([]detectedToolCall, 0, len(calls))
	scheduled := map[string]bool{}
	for _, call := range calls {
		if readName == "" || call.Name != readName {
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		valid, _ := validateDetectedToolCalls([]detectedToolCall{call}, tools, choice)
		if len(valid) == 1 && json.Unmarshal(call.Arguments, &args) == nil && args.FilePath != "" {
			scheduled[editGuardPath(args.FilePath)] = true
		}
	}
	blocked := false
	for _, call := range calls {
		if !strings.EqualFold(call.Name, "Edit") {
			out = append(out, call)
			continue
		}
		var args struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(call.Arguments, &args) != nil {
			out = append(out, call)
			continue
		}
		key := editGuardPath(args.FilePath)
		requirement, exists := g.needsRead[key]
		if !exists {
			out = append(out, call)
			continue
		}
		blocked = true
		if readName == "" || scheduled[key] {
			continue
		}
		encoded, _ := json.Marshal(map[string]string{"file_path": requirement.path})
		read := detectedToolCall{ID: scopedCallID(readName, string(encoded), len(out), "edit-recovery"), Name: readName, Arguments: encoded}
		valid, _ := validateDetectedToolCalls([]detectedToolCall{read}, tools, choice)
		if len(valid) == 1 {
			out = append(out, valid[0])
			scheduled[key] = true
		}
	}
	if blocked && len(out) == 0 {
		return nil, fmt.Errorf("failed Edit requires a successful fresh Read of the same file before another Edit; declare an allowed Read tool or provide its completed result")
	}
	return out, nil
}
