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

// synthesizeShellArguments 为「纯命令围栏」（```bash\nls -la\n```，没有 JSON
// 参数）合成参数对象。
//
// 网关自己造参数时必须照客户端声明的 schema 来：required 里的字段一个都不能少，
// 否则 validateDetectedToolCalls 必然拒收，模型看不到任何执行结果，只能重试再被
// 拒。硬编码 {"command": …} 就是这么坏事的 —— omp 的 bash schema required 是
// ["command","i"]，2026-09-08 当天同一原因被拒 24 次、跨两个构建。
//
// 只填网关真能诚实给出的值：命令进 command，其余必填字符串字段填一句由命令本身
// 派生的说明（intent 类字段的诚实答案就是「这条命令要做什么」），声明了 default
// 的按 default 填。非字符串的必填字段没有可诚实派生的值，留空 —— 那种情况应当
// 被校验拒收并进修复轮如实告知模型，而不是让网关瞎编一个值。
func synthesizeShellArguments(name, command string, tools []map[string]any) map[string]any {
	return fillRequiredShellArguments(name, map[string]any{"command": command}, tools)
}

// fillRequiredShellArguments 补齐 args 里缺失的必填字段，已有键一律不动。
//
// 三条 shell 参数出口（纯命令围栏、带 JSON 的围栏、裸 JSON 兜底）共用它：
// 模型给了什么就照原样透传，只把它漏掉的必填字段按声明补上。
//
// 只有「意图描述类」字段才由网关代填（omp 的 i）；路径、URL 等语义无法从
// 命令诚实推出的字段不填——硬塞一句 "run: ls" 进 cwd 这类字段是伪造参数，
// 2026-09-09 审计实锤（required 含 cwd 的 schema 会收到 cwd="run: ls -la"
// 并把它当工作目录）。填不了的留给 schema 校验拒收，修复轮如实告知模型。
func fillRequiredShellArguments(name string, args map[string]any, tools []map[string]any) map[string]any {
	fn := toolFunction(name, tools)
	if fn == nil {
		return args
	}
	command, _ := args["command"].(string)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	for _, field := range requiredToolArguments(name, tools) {
		if _, present := args[field]; present {
			continue
		}
		spec, _ := props[field].(map[string]any)
		if def, ok := spec["default"]; ok {
			args[field] = def
			continue
		}
		if typ, _ := spec["type"].(string); typ != "" && typ != "string" {
			continue
		}
		if !intentLikeFieldName(field) {
			continue
		}
		args[field] = summarizeShellCommand(command)
	}
	return args
}

// intentLikeFieldName 判断一个必填字段名是否属于「意图/说明」类——即那句由
// 命令派生的摘要可以诚实填充的字段。路径类、输出类字段一律不算。
func intentLikeFieldName(field string) bool {
	f := strings.ToLower(field)
	if f == "i" {
		// omp 的单字母 intent 字段。
		return true
	}
	for _, word := range []string{"intent", "description", "purpose", "reason", "why", "summary", "note", "comment", "explanation"} {
		if strings.Contains(f, word) {
			return true
		}
	}
	return false
}

// summarizeShellCommand 给出一句关于命令的简短说明，供 intent 类必填字段使用。
// 内容取自命令首行，不编造模型没表达过的意图。截断按 rune 边界，中文命令不会
// 被切出半个字符。
func summarizeShellCommand(command string) string {
	line := strings.TrimSpace(command)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return "run the shell command"
	}
	const limit = 60
	if runes := []rune(line); len(runes) > limit {
		line = strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return "run: " + line
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		// 容错解析：非法转义（C:\Users 的 \U）与裸控制字符先抢救一次。
		// 失败时 v 仍为 nil，走下面的「纯命令围栏」分支 —— 此前带 Windows 路径的
		// JSON 参数解析失败后，整段 JSON 文本会被当成 command 本身派发出去。
		_ = unmarshalJSONTolerant(args, &v)
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
					// 透传模型给的全部参数键，而不是重建 {command,timeout,workdir}。
					// omp 一类客户端的 bash schema required 含 "i"（concise intent），
					// 重建白名单会把它剥掉，validateDetectedToolCalls 必然拒收
					// 「missing required argument i」，模型拿不到执行结果只能重试，
					// 再拒——2026-09-08 用户会话实测六连拒都在这条路径上。
					// 模型漏掉的必填字段按声明补齐，已给的键一律不动。
					cmdBytes, _ := marshalToolArguments(converted, fillRequiredShellArguments(converted, m, tools))
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
				// JSON 形状但 command 键缺失/为空（{"i":"..."}、{"cmd":"ls"} 这类
				// 键名漂移）：此前静默 continue，客户端只看到空回合。把 body 原文
				// 当作命令本身派发——意图（跑一条命令）是清楚的，只是键名写错；
				// 真正的修复由调用方把错误输出回给模型，跟命令执行失败同等对待。
				cmdBytes, _ := marshalToolArguments(converted, synthesizeShellArguments(converted, args, tools))
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			if v == nil {
				// 纯命令围栏（```bash\nls -la\n```）没有 JSON 参数，参数由网关合成。
				// 合成时必须照声明的 schema 补齐必填字段，而不是只塞一个 command：
				// omp 的 bash schema required 是 ["command","i"]，只给 command 必被
				// schemaValid 拒收，模型永远看不到执行结果。
				cmdBytes, _ := marshalToolArguments(converted, synthesizeShellArguments(converted, args, tools))
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
			// 与路由轮的 fail-closed 策略对齐（tool_decision_extract.go 把解析
			// 不出的 JSON 体记为 Malformed 进修复轮）：非 shell 围栏的 body 解析
			// 失败也照样派发——参数原文进 arguments 字符串，schema 校验来拒收，
			// 修复轮来教模型。静默 continue 曾让截断的调用凭空消失（客户端只
			// 看到模型说要做什么，然后什么都没发生）。
			b := []byte(args)
			out = append(out, detectedToolCall{ID: callID(declared, string(b), len(out)), Type: toolType(declared, tools), Name: declared, Arguments: b})
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
		// 容错解析：内联围栏的参数同样常带 Windows 路径（2026-09-09 实测被打断的
		// edit 调用就是这一形态 + C:\Users 路径）。
		if !unmarshalJSONTolerant(c.Body, &v) {
			continue
		}
		m, isMap := v.(map[string]any)
		if !isMap {
			continue
		}
		// 与 canonical 围栏同一条补齐路径：shell 工具缺的必填字段照声明补上，
		// 两路的参数字节才能归一，下面的去重才会命中（否则同一逻辑调用会以
		// 不同的参数各派发一次，命令被执行两遍 —— 2026-09-09 审计实锤）。
		// 判据用小写名进 toolShellNames：这个表覆盖 bash/sh/powershell 等
		// 各种壳层拼写，也与并行限制的口径一致。
		if toolShellNames[strings.ToLower(declared)] {
			m = fillRequiredShellArguments(declared, m, tools)
		}
		b, _ := marshalToolArguments(declared, m)
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
			if !unmarshalJSONTolerant(line[:braceEnd+1], &obj) {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				// 同上：透传全部键。裸 JSON 兜底和围栏转换是同一条决策语义，
				// 白名单重建在这里同样会剥掉 required 字段（如 omp 的 i）。
				// 必填字段的补齐也走同一个函数，三条出口不再各行其是。
				cmdBytes, _ := marshalToolArguments(shell, fillRequiredShellArguments(shell, obj, tools))
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}
