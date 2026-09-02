package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type detectedToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolType(name string, tools []map[string]any) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			if typ, _ := t["type"].(string); typ != "" {
				return typ
			}
		}
	}
	return "function"
}

// allowedToolNames 按客户端声明时的原始拼写建表。键就是声明拼写 ——
// tool_calls 帧必须回这个拼写，客户端是按自己声明的名字派发的。
//
// 因此查表一律走 resolveDeclaredTool，不要直接 allowed[name]：模型写出的
// 大小写和声明拼写经常不一致，直接查会漏。
func allowedToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out[n] = true
			}
		}
	}
	return out
}

// resolveDeclaredTool 把模型写出的工具名解析成客户端声明的那个拼写：
// 先精确匹配，再忽略大小写匹配。返回声明拼写与是否命中。
//
// 加这层是因为大小写不一致曾让真实调用整条丢掉。Claude Code 声明的是
// Bash、Read、Edit、Write、Glob、Grep —— 全部首字母大写；而模型写围栏时
// 最自然的形态是 ```bash（提示词里也是这么说的：call the bash tool）。
// 三处消费方各自假设了不同的大小写，同一张表给出三种答案：
//
//   - declaredShell 查小写字面量 "bash"，声明成 Bash 时返回 ""，
//     于是 shell 特例、以及 fenced_tools.go 里以 shell != "" 为门槛的
//     裸 JSON 兜底，一起失效；
//   - fencedToolCalls 的 shell 分支撞上 allowed["bash"]==false 且
//     shell=="" 就 continue，```bash 的调用被静默丢弃；
//   - declaredFenceStart 先 ToLower 再查这张按声明拼写建的表，恒返回 -1，
//     流式路径既不缓冲未完成的围栏，也不剥离已经取走的围栏。
//
// 实测（工具集取真实的 Bash/Read/Edit/Write/Glob/Grep）：只有大小写完全
// 一致的 ```Bash 能出调用，```bash、```bash+裸命令、```sh 全部 0 候选。
// 既有围栏测试全部把工具声明成小写（"bash"、"read_file"），所以一直是绿的 ——
// 夹具没用真实客户端发的拼写。
func resolveDeclaredTool(allowed map[string]bool, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if allowed[name] {
		return name, true
	}
	lower := strings.ToLower(name)
	for declared := range allowed {
		if strings.ToLower(declared) == lower {
			return declared, true
		}
	}
	return "", false
}

type rejectedToolCall struct {
	Name   string
	Reason string
}

// validateDetectedToolCalls is the final trust boundary before a model-selected
// call is serialized to the client. ChatHub/native events and model-generated
// routing text are both untrusted: an undeclared name such as "unknown_tool"
// must never escape to Claude Code, Codex, or another local tool runner.
func validateDetectedToolCalls(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, []rejectedToolCall) {
	valid := make([]detectedToolCall, 0, len(calls))
	rejected := make([]rejectedToolCall, 0)
	seenCalls := make(map[string]struct{}, len(calls))
	seenIDs := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		requestedName := strings.TrimSpace(call.Name)
		name, fn := declaredTool(requestedName, tools)
		if fn == nil {
			rejected = append(rejected, rejectedToolCall{Name: requestedName, Reason: "tool was not declared by the client"})
			continue
		}
		call.Name = name
		if !toolChoiceAllows(choice, call.Name) {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool_choice does not allow this tool"})
			continue
		}
		args := map[string]any{}
		rawArgs := strings.TrimSpace(string(call.Arguments))
		if rawArgs == "" || rawArgs == "null" {
			args = map[string]any{}
		} else if err := json.Unmarshal(call.Arguments, &args); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments are not a JSON object"})
			continue
		}
		if args == nil {
			args = map[string]any{}
		}
		if err := schemaValid(args, fn); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: err.Error()})
			continue
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments could not be normalized"})
			continue
		}
		call.Arguments = encoded
		signature := call.Name + "\x00" + string(call.Arguments)
		if _, duplicate := seenCalls[signature]; duplicate {
			// SignalR can replay a terminal tool frame. Suppressing an identical
			// validated call is safer than executing a side effect twice.
			continue
		}
		seenCalls[signature] = struct{}{}
		if call.ID == "" {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		} else if _, duplicate := seenIDs[call.ID]; duplicate {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		}
		seenIDs[call.ID] = struct{}{}
		// Type is transport metadata, not an upstream authority. Serialize the
		// caller-declared type so a fabricated native frame cannot alter it.
		call.Type = toolType(call.Name, tools)
		valid = append(valid, call)
	}
	return valid, rejected
}

// enforceParallelToolCalls 落实客户端的 parallel_tool_calls。
//
// codex_catalog.go 向客户端宣告 supports_parallel_tool_calls=true，那就必须同样
// 尊重显式的 false —— 否则 Codex 关掉并行后仍会收到多个调用，而它一次只执行一
// 个，剩下的调用不会有结果返回，下一轮历史里就出现「有调用无结果」，被
// validateToolConversation 判为非法。宣告了一项能力却不实现它的关闭语义，比不
// 宣告更糟。
//
// parallel 为 nil（未指定）或 true 时按原样放行；false 时只保留第一个调用，其余
// 作为被拒项报告出去，让调用方能记日志。保留第一个而非整批拒绝，是因为「降为
// 单调用」才是该字段的语义，被丢弃的调用模型下一轮可以重新发起。
func enforceParallelToolCalls(calls []detectedToolCall, parallel *bool) ([]detectedToolCall, []rejectedToolCall) {
	if parallel == nil || *parallel || len(calls) <= 1 {
		return calls, nil
	}
	dropped := make([]rejectedToolCall, 0, len(calls)-1)
	for _, call := range calls[1:] {
		dropped = append(dropped, rejectedToolCall{
			Name:   call.Name,
			Reason: "parallel_tool_calls=false allows one call per turn",
		})
	}
	return calls[:1], dropped
}

func requestedToolChoiceName(choice any) string {
	m, ok := choice.(map[string]any)
	if !ok {
		return ""
	}
	if f, ok := m["function"].(map[string]any); ok {
		if n, ok := f["name"].(string); ok {
			return strings.TrimSpace(n)
		}
	}
	if n, ok := m["name"].(string); ok {
		return strings.TrimSpace(n)
	}
	return ""
}

// toolChoiceRequiresToolCall reports modes where accepting an empty decision
// would silently downgrade an explicit client request into ordinary prose.
func toolChoiceRequiresToolCall(choice any) bool {
	if requestedToolChoiceName(choice) != "" {
		return true
	}
	s, ok := choice.(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), "required")
}

func toolChoiceAllows(choice any, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "none":
			return false
		case "", "auto", "required":
			return true
		default:
			return true
		}
	}
	if requested := requestedToolChoiceName(choice); requested != "" {
		// 忽略大小写：tool_choice 里的拼写与工具声明的拼写不一定一致，
		// 精确比较会把模型写对的调用判成「不是你指定的那个工具」。
		return strings.EqualFold(requested, name)
	}
	return true
}

// toolChoiceAllowsAnyCall 判断调用方是否允许本轮发生任何工具调用。
// 只有 tool_choice:"none" 是禁止；指名某个工具仍然是允许调用。
//
// 与 toolChoiceAllows 分开是因为后者需要一个具体工具名，而有些判断发生在
// 还没有候选名字的时候（例如是否值得为「模型说它不能调工具」跑一轮纠正）。
func toolChoiceAllowsAnyCall(choice any) bool {
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		return strings.ToLower(strings.TrimSpace(s)) != "none"
	}
	if m, ok := choice.(map[string]any); ok {
		if t, ok := m["type"].(string); ok && strings.ToLower(strings.TrimSpace(t)) == "none" {
			return false
		}
	}
	return true
}

func callID(name, args string, index int) string {
	return "call_" + uuid.NewString()
}

func extractToolCalls(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	start := strings.Index(text, "<m365-tool-call>")
	end := strings.Index(text, "</m365-tool-call>")
	if start < 0 || end <= start {
		return nil, false
	}
	var raw any
	if json.Unmarshal([]byte(text[start+len("<m365-tool-call>"):end]), &raw) != nil {
		return nil, false
	}
	items := []any{raw}
	if arr, ok := raw.([]any); ok {
		items = arr
	}
	out := make([]detectedToolCall, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		n, _ := m["name"].(string)
		name, fn := declaredTool(n, tools)
		if fn == nil || !toolChoiceAllows(choice, name) {
			continue
		}
		a, _ := json.Marshal(m["arguments"])
		out = append(out, detectedToolCall{ID: callID(name, string(a), i), Type: toolType(name, tools), Name: name, Arguments: a})
	}
	valid, _ := validateDetectedToolCalls(out, tools, choice)
	return valid, len(valid) > 0
}

func validateToolResult(messages []oaiMsg, known map[string]bool) error {
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required")
			}
			if len(known) > 0 && !known[m.ToolCallID] {
				return fmt.Errorf("unknown tool_call_id: %s", m.ToolCallID)
			}
		}
	}
	return nil
}

var toolRefusalPatterns = []string{
	"tools are not available",
	"tool is not available",
	"cannot access the Windows path",
	"only provides Linux",
	"只提供 Linux 容器",
	"工具未暴露",
	"工具不可用",
	"没有可调用的",
	"无法继续操作",
	"will not pretend",
	"will not fake",
	"cannot fake",
	"would be fabricated",
	"cannot fabricate",
	"refuse to fabricate",
	"not actually registered",
	"not actually available",
	"not exposed in this",
	"not available in this session",
	"cannot execute on this platform",
	"没有 Windows 执行接口",
	"回复通道没有",
	"没有执行接口",
	"不会虚构",
	"不会!转入",
	"不会转入",
	"execution environment has changed",
	"执行环境已经切换",
	"无法访问上一会话",
	"/mnt/data",
	"current execution environment has changed",
	"linux sandbox",
	// 2026-08-24: the model asserted "the tools actually provided to me this
	// turn still contain no PowerShell, Read, Glob ... only isolated container
	// tools, and that container does not map C:\ or E:\". It names the declared
	// tools while denying they were declared, so a match on tool names alone is
	// not enough -- these phrasings keyed on "what I was given this turn".
	"提供给我的工具",
	"提供给我的工具中",
	"实际提供给我",
	"本轮实际提供",
	"仍没有 PowerShell",
	"只有隔离的容器工具",
	"隔离的容器工具",
	"未映射",
	"不能自行连接",
	"未开放的工具",
	"tools actually provided",
	"tools provided to me",
	"no tools were provided",
	"not among the tools",
	"isolated container",
	"linux container",
	"running in a container",
	"cannot modify source code",
	"没有连接到",
	"Windows 执行接口",
	"I can run that for you",
	"running in sandbox",
	"executing in sandbox",
	"code interpreter",
	"python sandbox",
	"sandbox environment",
	// 2026-09-02: measured on a clean Claude CLI run against /v1/messages (36
	// tools declared, choice=auto). The answer turn said "I don't have a
	// dedicated file-read tool wired up in this session that can browse
	// `E:\Temp\m365-clean-probe\` directly -- the `python_execution` tool runs
	// in a sandboxed environment and can't reach your local drive paths", then
	// offered "Paste the file contents here" and "Run this in PowerShell
	// yourself". It reached the client uncorrected: the list already held
	// "sandbox environment" and "python sandbox", but the model wrote
	// "sandboxed environment" and "python_execution", and strings.Contains
	// bridges neither. Each phrasing below is a span of that sentence, kept
	// apostrophe-free where possible so a curly-quote variant still matches.
	"sandboxed environment",
	"python_execution",
	"python execution tool",
	"access the local filesystem path",
	"reach your local drive",
	"dedicated file-read tool",
	"wired up in this session",
	// The denial's fallback: hand the work back to the caller. Both spans are
	// long enough that a legitimate answer describing a user's PowerShell
	// script or pasted file will not contain them.
	"paste the file contents here",
	"in powershell yourself",
}

func isToolRefusal(text string) bool {
	low := strings.ToLower(text)
	for _, p := range toolRefusalPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

var sandboxHallucinationPatterns = []string{
	"I can run that for you",
	"I'll run that",
	"let me run that",
	"let me execute",
	"running in sandbox",
	"executing in sandbox",
	"code interpreter",
	"python sandbox",
	"sandbox environment",
	"/mnt/data",
	"linux container",
	"linux sandbox",
	// 2026-08-24: the model asserted "the tools actually provided to me this
	// turn still contain no PowerShell, Read, Glob ... only isolated container
	// tools, and that container does not map C:\ or E:\". It names the declared
	// tools while denying they were declared, so a match on tool names alone is
	// not enough -- these phrasings keyed on "what I was given this turn".
	"提供给我的工具",
	"提供给我的工具中",
	"实际提供给我",
	"本轮实际提供",
	"仍没有 PowerShell",
	"只有隔离的容器工具",
	"隔离的容器工具",
	"未映射",
	"不能自行连接",
	"未开放的工具",
	"tools actually provided",
	"tools provided to me",
	"no tools were provided",
	"not among the tools",
	"isolated container",
	"cloud sandbox",
	"execution environment has changed",
	"cannot access the Windows path",
	"only provides Linux",
	"只提供 Linux 容器",
	"执行环境已经切换",
	"I don't have SSH access tools",
	"I don't have any tools",
	"none of which can reach",
	// 同上：模型用「本轮提供给我的工具里没有」来否认已声明的工具。
	"提供给我的工具",
	"实际提供给我",
	"只有隔离的容器工具",
	"隔离的容器工具",
	"未映射",
	"未开放的工具",
	"tools actually provided",
	"tools provided to me",
	"isolated container",
	// 2026-09-02: same measured Claude CLI run against /v1/messages as the batch
	// in toolRefusalPatterns. Only the half that asserts a hosted runtime
	// belongs here -- "the `python_execution` tool runs in a sandboxed
	// environment and can't reach your local drive paths". The existing
	// "sandbox environment" and "python sandbox" missed it by one word each.
	// The fallback offers ("paste the file contents here", "run this in
	// PowerShell yourself") are refusal shapes, not sandbox claims, so they
	// stay out of this list.
	"sandboxed environment",
	"python_execution",
	"python execution tool",
	"access the local filesystem path",
	"reach your local drive",
}

func isSandboxHallucination(text string) bool {
	low := strings.ToLower(text)
	for _, p := range sandboxHallucinationPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
