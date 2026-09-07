package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	// 规则文案逐字取自原 APK rodata（0x51e… 段，以 "- Decide the next
	// concrete action" 起、"Do not invent tools." 止）。关键是
	// "end with EXACTLY one line" —— 推理档位的模型会先输出思考过程，
	// 指令只出现在末尾。此前恢复成 "respond with:"，配合解析侧的
	// HasPrefix 检查，等于要求整段回复必须以 CALL_TOOL: 开头，推理
	// tone 必然解析失败（纯文本 tone 直接给指令所以能过）。
	rules := `- Decide the next concrete action. Prefer the most specific tool (a document/skill tool beats a raw shell).
- If tool calls are needed, end with the decision lines: one line per call, CALL_TOOL: tool_name({"arg1":"value1"}). To make several independent calls in one round, put their CALL_TOOL lines together at the end with nothing between them.
	- If no tool is needed, end with: NO_TOOL_NEEDED
	- When MODE is required, you MUST select at least one valid declared tool call; never return NO_TOOL_NEEDED.
	- When a tool is explicitly named by the user, select that declared tool or a more specific declared equivalent; do not answer with prose.
- If the user asked to inspect, edit, run, or patch local files, or named a declared tool, you MUST emit CALL_TOOL, not NO_TOOL_NEEDED.
- When a tool writes a file, its text argument must contain the finished deliverable itself. Never pass the instructions, the brief, or a description of what to write; the user reads the file, not the request.
- Never build a document by shelling out. Do not use cat/tee heredocs or echo redirects to create content; call the file-writing tool or the dedicated skill instead.
- workspace_shell is for short operational commands (ls, tests, git, package installs), not for authoring.
- Only use tools from the available list. Validate arguments against the schema. Do not invent tools.`
	// Multi-turn: completed tool evidence was already acted upon, so re-invoking
	// those tools would duplicate work.
	//
	// The gate must name the markers the prompt actually carries. The only
	// producer of this text is flattenPromptMessages, and it writes
	// "[<role> tool_calls]" and "[tool result id=call_x]" — neither contains
	// "tool_calls:" nor "tool[call_". Those two spellings never appeared in a
	// real request, so on every genuine multi-turn call the rules below were
	// silently omitted and the model kept re-invoking tools whose results it had
	// already been shown. The legacy spellings are kept so a caller that pastes a
	// transcript in that shape still trips the gate.
	// 子组递归约束：模型拿到 task/agent/dispatch 类工具时会递归开子组——
	// 子组里再开子组，一层层铺下去，token 与时间双爆（2026-09-08 用户实测
	// 「子组会无限分发子组」）。约束放在路由契约里：子组的 prompt 参数必须是
	// 自包含的叶子任务说明——明确告知执行者不要再用子组工具，直接用读/写/
	// shell 类工具完成。这是提示层约束（无法硬性验证 prompt 内容），配合
	// 并发限制（一次最多一个 task 调用）兜底。
	rules += `
- When you dispatch a task/agent/subagent tool, its prompt must be a self-contained leaf instruction: tell the executor to do the work directly with read/write/shell tools and NOT to dispatch further subagents. Subgroup chains that do not terminate are rejected by the caller.`
	if strings.Contains(prompt, "tool_calls]") || strings.Contains(prompt, "[tool result id=") ||
		strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: [role tool_calls] and [tool result id=...] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	if name := requestedToolChoiceName(choice); name != "" {
		rules += fmt.Sprintf(`
- MODE explicitly requires the declared tool %q. End with that tool's valid CALL_TOOL directive; never return NO_TOOL_NEEDED or a different tool.`, name)
	}
	// The replayed transcript is untrusted evidence, not the router contract. It
	// must come before the contract: placing it last gives a foreign provider's
	// final assistant/user instruction the highest recency and it can make M365
	// emit status prose instead of a decision. This is exactly the history-replay
	// shape when a client closes another provider's conversation and resumes it
	// here. The intent retry used to recover only because its extra directive was
	// appended after the evidence; keep the normal path in the same order.
	//
	// routerTransitNote 与 chathub/tool_protocol.go 的 promptQuirkWorkarounds
	// 同源：上游链路会确定性剥除「单 token 方括号字面量 + ::」（PowerShell
	// 类型加速器形态）。路由轮的 Tools 为空、schema 在正文里走的是另一条组
	// 装路径，不会经过 toolProtocolPrompt，所以这里单独带上同一条约定——让
	// 模型在源头就写不会被吃掉的空格形态（[ string]::），而不是等网关下游
	// 对每个残迹打补丁。空格形态在 PowerShell 里语义等价，两侧零成本。
	routerTransitNote := "Transit note: a tight PowerShell type accelerator like [string]:: is stripped in transit on this link. When your command arguments use one, write the spaced form instead — [ string]::, [ math]::, [ System.Environment]:: — which passes through unchanged and is valid PowerShell. "
	return fmt.Sprintf(`You are a tool selection assistant. Based on the user request, decide which tool to call next.
%s
Available tools: %s

MODE: %s

User request and evidence:
%s

Routing contract:
%s`, routerTransitNote, defs, mode, prompt, rules)
}

// normalizeToolDirective 把全角冒号统一成半角。模型用中文作答时常输出
// "CALL_TOOL：name(...)"，此前一律解析失败并落到修复轮。全角冒号占 3 字节、
// 半角占 1 字节，长度会变，因此规范化后的文本要整体用于后续下标运算。
func normalizeToolDirective(text string) string {
	return strings.ReplaceAll(text, "：", ":")
}

// lastToolDirectiveIndex 返回最后一个 CALL_TOOL 指令的起始下标（大小写
// 不敏感），没有则返回 -1。推理模型的指令位于思考之后，必须取最后一处：
// 思考过程里可能复述过 "CALL_TOOL:" 字样，取第一处会解析到错误的参数。
//
// 「算不算一处指令」必须与 extractDirectiveCandidates 用同一个判据
// （directiveTarget），否则两个识别器会漂移：此前这里要求标记后紧跟半角冒号，
// 抽取侧只要求装饰之后有名字，于是 "CALL_TOOL Read({...})" 能被抽出候选却在
// 位置上不可见 —— 要么整轮 parsed=false（raw_candidates=0），要么选中思考里
// 那条带冒号的旧指令。
func lastToolDirectiveIndex(text string) int {
	lower := strings.ToLower(text)
	last := -1
	for offset := 0; ; {
		idx := strings.Index(lower[offset:], directiveMarker)
		if idx < 0 {
			return last
		}
		idx += offset
		offset = idx + len(directiveMarker)
		if name, _ := directiveTarget(text[offset:]); name != "" {
			last = idx
		}
	}
}

// hasTrailingNoToolMarker 判断 NO_TOOL_NEEDED 是否作为收尾指令出现。
// 取末尾若干字符做窗口：指令按规则独占最后一行，而思考正文里的提及
// 通常位于更靠前的位置。
func hasTrailingNoToolMarker(text string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "。.!！`\"'\u201d\u3002")
	const window = 64
	tail := trimmed
	if len(tail) > window {
		tail = tail[len(tail)-window:]
	}
	return strings.Contains(strings.ToUpper(tail), "NO_TOOL_NEEDED")
}

// parseModelToolDecision 抽取模型的路由决策。
//
// 实现委托给 tool_decision_extract.go 的统一抽取器：先枚举全部候选（不问
// 它被什么包裹），再统一校验并取最后一个有效的。这样新增一种输出包装方式
// 时无需在此新增分支 —— 此前按模型逐个打补丁（gpt-5.6 的 CALL_TOOL 前缀、
// gpt-5.5 的思考期花括号、中文全角冒号……）每换一个模型就要返工一次。
//
// 返回值 parsed 表示「模型给出了可理解的决策」，与是否选中工具无关：
// NO_TOOL_NEEDED 和空 calls 都是 parsed=true、calls 为空。
func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = normalizeDecisionText(strings.TrimSpace(text))
	if text == "" {
		return nil, false
	}

	// Decision selection is positional. Never fall back to an earlier valid
	// call when the model's final frame is malformed or explicitly says no tool:
	// that causes stale calls to execute after the stream has already terminated.
	directiveAt := lastToolDirectiveIndex(text)
	decisions := extractEnvelopeDecisions(text)
	// 以工具名命名的围栏与 JSON 信封是同一类「帧」：整帧要么全部合法，要么
	// 判为不可解析。合并进同一个候选序列，位置语义（取最后一帧）不变。
	decisions = append(decisions, extractFencedDecisions(text, tools)...)
	decisions = append(decisions, extractXMLNamedDecisions(text)...)
	noToolAt, noTool := trailingNoToolDecision(text)

	latestAt := -1
	latestKind := ""
	if directiveAt >= 0 {
		latestAt, latestKind = directiveAt, "directive"
	}
	latestEnvelope := -1
	for i := range decisions {
		if decisions[i].At > latestAt {
			latestAt, latestKind, latestEnvelope = decisions[i].At, "envelope", i
		}
	}
	if noTool && noToolAt > latestAt {
		latestKind = "no_tool"
	}

	switch latestKind {
	case "no_tool":
		if toolChoiceRequiresToolCall(choice) {
			return nil, false
		}
		return nil, true
	case "directive":
		return selectDirectiveFrame(text, directiveAt, tools, choice)
	case "envelope":
		decision := decisions[latestEnvelope]
		if decision.Malformed {
			return nil, false
		}
		calls := selectAllValid(decision.Calls, tools, choice)
		// Reject a partially invalid final envelope rather than silently running
		// only the subset that happened to validate.
		if len(calls) != len(decision.Calls) {
			return nil, false
		}
		if len(calls) == 0 && toolChoiceRequiresToolCall(choice) {
			return nil, false
		}
		return calls, true
	default:
		return nil, false
	}
}

// selectDirectiveFrame 对 CALL_TOOL 指令路径做末帧选择。
//
// 信封与围栏路径都支持一次发起多个调用（selectAllValid / 相邻围栏合并成同一
// 帧），指令路径此前却只认最后一条：契约写死 "end with EXACTLY one line"，选择
// 侧也只取最后一个指令位置的候选。模型真发出多行 CALL_TOOL 时，除最后一行外
// 全部被静默丢弃 —— 2026-09-08 实测 router 每轮只 emit 1 个调用的来源之一。
//
// 末帧语义与信封路径对齐：位置在 directiveAt 的候选是末帧第一条；之后每一条
// 与前一条之间只有空白（换行、缩进）的候选并入同一帧。整帧统一校验：
//   - 全部合法 → 全部执行（多调用并行）；
//   - 帧内任何一条无效 → 整帧失败关闭，进入修复轮 —— 绝不静默执行子集，也
//     绝不复活更早的、已被模型推翻的指令。
//
// 单指令行为不变：末帧只有一条时与旧 selectDecision 完全等价。
func selectDirectiveFrame(text string, directiveAt int, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	candidates := extractDirectiveCandidates(text)
	if len(candidates) == 0 {
		return nil, false
	}
	// 定位 directiveAt 所在的候选，再向两侧扩展出完整帧。directiveAt 是
	// 最后一条指令的位置，锚点可能在帧中间：向前的相邻候选属于同一帧（
	// 模型一次发了多行调用），向前遇到正文隔断才停；向后同理。
	anchor := -1
	for i, candidate := range candidates {
		if candidate.At == directiveAt {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return nil, false
	}
	// 向前：相邻即并入（前一条 End 到本条 At 之间只有空白）。
	start := anchor
	for start > 0 && candidates[start-1].End > 0 &&
		directivesAreAdjacent(text, candidates[start-1], candidates[start]) {
		start--
	}
	// 向后：从锚点起相邻即并入，遇到正文隔断另起帧（那些帧比末帧旧，丢弃）。
	end := anchor
	for end+1 < len(candidates) && directivesAreAdjacent(text, candidates[end], candidates[end+1]) {
		end++
	}
	frame := candidates[start : end+1]
	// selectDecision 的末位优先语义保留给单条帧；多条帧用 selectAllValid。
	if len(frame) == 1 {
		return selectDecision(frame, tools, choice)
	}
	calls := selectAllValid(frame, tools, choice)
	if len(calls) != len(frame) {
		return nil, false
	}
	if len(calls) == 0 && toolChoiceRequiresToolCall(choice) {
		return nil, false
	}
	return calls, true
}

// directivesAreAdjacent 判断两条指令之间是否只有空白（含换行）。前一条的参数
// 体闭合（End）到后一条标记起点（At）之间不能有任何实义字符。
func directivesAreAdjacent(text string, prev, next toolCandidate) bool {
	end := prev.End
	if end <= 0 || end > len(text) {
		return false
	}
	return strings.TrimSpace(text[end:next.At]) == ""
}
