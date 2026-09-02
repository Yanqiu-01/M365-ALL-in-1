package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n```")

// declaredShell returns the shell-ish tool name the client actually
// declared (bash/sh/shell/powershell/cmd), or "" if none. Forcing an
// undeclared bash call on clients that don't support it (issue #12) makes
// them error out and loop, so conversion only happens for declared tools.
// 返回声明拼写而不是小写字面量：忽略大小写匹配，声明成 Bash 时也要认出来，
// 否则 shell 特例与下面以 shell != "" 为门槛的裸 JSON 兜底会一起失效。
func declaredShell(allowed map[string]bool) string {
	for _, n := range []string{"bash", "sh", "shell", "powershell", "cmd"} {
		if declared, ok := resolveDeclaredTool(allowed, n); ok {
			return declared
		}
	}
	return ""
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		_ = json.Unmarshal([]byte(args), &v)
		// Auto-convert bash/shell code blocks to tool calls, but only when
		// the client declared the tool.
		if lower := strings.ToLower(name); lower == "bash" || lower == "sh" || lower == "shell" || lower == "powershell" || lower == "cmd" {
			// 声明拼写优先：客户端声明 Bash 而模型写 ```bash 时，派发的名字
			// 必须是 Bash，否则客户端认不出来。
			converted, ok := resolveDeclaredTool(allowed, name)
			if !ok {
				if shell == "" {
					continue
				}
				converted = shell
			}
			if m, ok := v.(map[string]any); ok {
				if cmd, hasCmd := m["command"]; hasCmd && cmd != "" {
					cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if v == nil {
				cmdBytes, _ := json.Marshal(map[string]any{"command": args})
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			continue
		}
		// 忽略大小写解析，并且之后一律用声明拼写：tool_choice 判定、callID、
		// toolType、派发出去的 Name 都必须是客户端声明的那个名字。
		declared, ok := resolveDeclaredTool(allowed, name)
		if !ok || !toolChoiceAllows(choice, declared) {
			continue
		}
		if v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, detectedToolCall{ID: callID(declared, string(b), len(out)), Type: toolType(declared, tools), Name: declared, Arguments: b})
	}
	// Also check for plain JSON objects with a "command" field (not in fenced blocks)
	if len(out) == 0 && shell != "" {
		for i := 0; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			end := strings.Index(text[i:], "\n")
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			braceEnd := strings.LastIndex(line, "}")
			if braceEnd < 0 {
				continue
			}
			if !strings.Contains(line[:braceEnd+1], `"command"`) {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[:braceEnd+1]), &obj) != nil {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}
