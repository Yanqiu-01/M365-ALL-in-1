package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n```")

// fencedInlineParenCall matches the answer-turn shape where the model opens a
// fence and puts the tool call in directive form on the SAME line:
// ```Edit({"file_path":"C:\\x.md","old_string":"a","new_string":"b"})
// This is not a hypothetical: a live 2026-09-07 continuation turn emitted
// exactly this for a Chinese-path Edit and the call was delivered as visible
// prose instead of a tool_use — the client (Claude Code) saw the model "talk
// about" the edit without performing it, and the follow-up refusal "需要本机的
// edit 或 write 工具" followed. The canonical fence regex requires a newline
// after the info string, so it cannot see this shape; the router directive
// parser only looks for CALL_TOOL. This extractor bridges both gaps: any
// ```Name(args) or ```Name {json} appearing anywhere in text counts as a call.
var fencedInlineParenCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)[ \t]*(\\([^{}]*(?:\\{.*?\\})?\\)[ \t]*\\n?|\\{.*?\\})[ \t]*\\n?```")

// fencedInlineCallCandidates returns the (name, argsSpan) pairs that either
// extractor can see, so callers can deduplicate instead of double-firing.
func fencedInlineCallCandidates(text string) []struct{ Name, Body string } {
	var out []struct{ Name, Body string }
	for _, m := range fencedInlineParenCall.FindAllStringSubmatch(text, -1) {
		name, body := m[1], strings.TrimSpace(m[2])
		if strings.HasPrefix(body, "(") {
			body = strings.TrimSuffix(strings.TrimPrefix(body, "("), ")")
		}
		if body == "" {
			continue
		}
		out = append(out, struct{ Name, Body string }{name, body})
	}
	return out
}

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
					cmdBytes, _ := marshalToolArguments(converted, map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if v == nil {
				cmdBytes, _ := marshalToolArguments(converted, map[string]any{"command": args})
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
	// 围栏内联形态：```Name({...}) 不换行直接给参数。上面的正则要求 info
	// string 之后有换行，看不见这种；答案轮实测出现过（2026-09-07 续写任务，
	// Edit 调用被当正文发给客户端，客户端看到模型「说要做」却没做，随后触发
	// 「需要本机的 edit 或 write 工具」拒答）。这里补一条抽取路径，形态与
	// CALL_TOOL 指令同构，但裹在围栏里。
	for _, c := range fencedInlineCallCandidates(text) {
		declared, ok := resolveDeclaredTool(allowed, c.Name)
		if !ok || !toolChoiceAllows(choice, declared) {
			continue
		}
		var v any
		if json.Unmarshal([]byte(c.Body), &v) != nil {
			continue
		}
		if _, isMap := v.(map[string]any); !isMap {
			continue
		}
		b, _ := json.Marshal(v)
		// 与上面同一个调用可能两种形态都被抓到：按名字+参数去重。
		dup := false
		for _, existing := range out {
			if existing.Name == declared && string(existing.Arguments) == string(b) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
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
				cmdBytes, _ := marshalToolArguments(shell, map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}
