package web

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type toolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	Failed    bool   `json:"failed"`
	// Answered 记录「是否收到过这个 id 的 tool 消息」，与结果内容是否为空无关。
	// 判定 Pending 必须用它而不是 Result == ""：空结果是合法的（无输出的命令、
	// 纯写入调用、只含图片块的 content），把那些当成未应答会让下一轮报 409。
	Answered bool `json:"answered"`
}
type agentLedger struct {
	Completed           []toolEvidence `json:"completed"`
	Pending             []toolEvidence `json:"pending"`
	ToolRounds          int            `json:"tool_rounds"`
	RepeatedCall        bool           `json:"repeated_call"`
	RepeatedFailure     bool           `json:"repeated_failure"`
	RepetitionSignature string         `json:"repetition_signature,omitempty"`
}

var failureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\berror\b|\bfailed\b|\bfailure\b|exception|traceback|timed?\s*out|permission denied|not found|refused)`)

func compactToolResult(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit < 200 {
		limit = 200
	}
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 80
	if tail < 80 {
		tail = 80
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-head-tail) + s[len(s)-tail:]
}

// toolResultLooksFailed 判断一次工具结果是否表示失败。
//
// 不能直接对结果全文套 failureSignal：读文件类工具返回的是源代码，其中
// "Exception"、"error"、"failed"、"not found" 都是正常标识符或字符串
// —— 例如 inventory.py 首行就是 class StockError(Exception)。实测一次成功的
// read_file 因此被记为 failed，模型随后连续多轮声称「read_file 结果被标记为
// 失败，无法完整审查代码」，直到步数耗尽。
//
// 因此：观察类工具只在结构化的失败字段或显式错误前缀上判定；其余工具沿用
// 原有的宽匹配。
func toolResultLooksFailed(name, result string) bool {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return false
	}
	if toolLooksObservational(name) {
		// 结构化结果：只认显式的失败字段。
		var probe map[string]any
		if json.Unmarshal([]byte(trimmed), &probe) == nil {
			if v, ok := probe["error"]; ok && fmt.Sprint(v) != "" && fmt.Sprint(v) != "<nil>" {
				return true
			}
			if v, ok := probe["passed"].(bool); ok {
				return !v
			}
			// 成功的读取/列举结果不含 error 字段，直接视为成功。
			return false
		}
		// 非 JSON：只认开头的显式错误说明，避免命中正文里的标识符。
		lower := strings.ToLower(trimmed)
		for _, prefix := range []string{"error", "failed", "failure", "exception:", "traceback"} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
		return false
	}
	return failureSignal.MatchString(compactToolResult(result, 4000))
}

// scopedCallID returns a globally unique tool call id. The scope parameters
// are kept for signature compatibility with callers that pass per-turn
// context; the id itself must not depend on call content or scope text,
// otherwise repeating the same tool+arguments across turns collides
// (duplicate tool call id errors from clients).
func scopedCallID(name, args string, index int, scope string) string {
	return "call_" + uuid.NewString()
}

// toolArgumentsJSON 从一个工具调用对象中取出 arguments 的字符串形式。
// arguments 已是字符串时原样返回；是结构化对象时序列化为 JSON；
// 其余类型退回 fmt.Sprint。
//
// APK 证据（tools/apktool，agent_ledger.go:81-91，240 字节）：
// 调用图仅含 encoding/json.Marshal 与 fmt.Sprint，无其它项目内调用。
func toolArgumentsJSON(call map[string]any) string {
	function, _ := call["function"].(map[string]any)
	arguments := function["arguments"]
	if arguments == nil {
		return ""
	}
	if text, ok := arguments.(string); ok {
		return text
	}
	if encoded, err := json.Marshal(arguments); err == nil {
		return string(encoded)
	}
	return fmt.Sprint(arguments)
}

func buildAgentLedger(messages []oaiMsg) agentLedger {
	calls := map[string]toolEvidence{}
	order := []string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, raw := range m.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args := fmt.Sprint(fn["arguments"])
				if id != "" {
					calls[id] = toolEvidence{ID: id, Name: name, Arguments: args}
					order = append(order, id)
				}
			}
		}
		if m.Role == "tool" {
			if e, ok := calls[m.ToolCallID]; ok {
				raw := contentToString(m.Content)
				e.Result = compactToolResult(raw, 4000)
				// 收到 tool 消息这件事本身就是「已应答」，与内容是否为空无关。
				//
				// 空结果在协议上完全合法：一条没有输出的命令、一次只做写入的调用、
				// 或者 content 里只有非文本块（图片）都会得到空串。早先仅以
				// Result == "" 判定 Pending，于是这些调用被当成从未返回，下一轮
				// CanContinue 直接抛 409 "pending tool results must be returned
				// before another turn" —— 而客户端明明已经返回了。
				e.Answered = true
				e.Failed = toolResultLooksFailed(e.Name, raw)
				calls[m.ToolCallID] = e
			}
		}
	}
	l := agentLedger{}
	seenCall := map[string]int{}
	seenFailure := map[string]int{}
	for _, id := range order {
		e := calls[id]
		l.ToolRounds++
		sig := e.Name + "\x00" + e.Arguments
		seenCall[sig]++
		if seenCall[sig] >= 2 {
			l.RepeatedCall = true
			l.RepetitionSignature = sig
		}
		if !e.Answered {
			l.Pending = append(l.Pending, e)
		} else {
			l.Completed = append(l.Completed, e)
			if e.Failed {
				fs := e.Name + "\x00" + e.Arguments + "\x00" + normalizeFailure(e.Result)
				seenFailure[fs]++
				if seenFailure[fs] >= 2 {
					l.RepeatedFailure = true
					l.RepetitionSignature = fs
				}
			}
		}
	}
	return l
}
func normalizeFailure(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "#")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
func (l agentLedger) RouterContext() string {
	type compact struct {
		Completed    []toolEvidence `json:"completed"`
		Pending      []toolEvidence `json:"pending"`
		RepeatedCall bool           `json:"repeated_call"`
	}
	b, _ := json.Marshal(compact{l.Completed, l.Pending, l.RepeatedCall})
	// 逐字取自原 APK rodata。关键是后半句：read/inspect/check/test 在工作区
	// 状态变化后允许重复，只禁止「参数完全相同的变更类调用」。
	//
	// 恢复期误写成「A completed call is final evidence; do not issue the same
	// name and arguments again.」—— 等于告诉模型任何调用都不得重复。于是它
	// 在需要再写一次文件、再跑一次 run_tests 时选择回 NO_TOOL_NEEDED，网关
	// 又反复推它「必须调用 run_tests」，指令自相矛盾。实测表现为 bugfix 跑满
	// 14 步、algorithm 连续三次「提前结束」，最终罚到地板分。
	hint := "Use only this compact evidence. Do not repeat a completed mutating call with identical arguments. Read, inspect, check, and test calls may be repeated after workspace state changes."
	if l.RepeatedFailure {
		hint += " The same call failed repeatedly; change strategy instead of retrying unchanged."
	}
	return hint + "\nEVIDENCE_LEDGER: " + string(b)
}
func canonicalToolArguments(s string) string {
	s = strings.TrimSpace(s)
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}

func (l agentLedger) hasCompleted(name, args string) bool {
	want := canonicalToolArguments(args)
	for _, e := range l.Completed {
		if e.Name == name && canonicalToolArguments(e.Arguments) == want {
			return true
		}
	}
	return false
}

// toolLooksObservational 判断工具名是否属于只读/观察类。
//
// APK 证据（tools/apktool，agent_ledger.go:237-244，192 字节）：
// TrimSpace + ToLower 后对一个全局字符串切片逐项 strings.Contains。
// 该切片位于 0x1be6000+2656，实测 22 项，内容与顺序如下。
func toolLooksObservational(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, word := range []string{"read", "list", "get", "search", "find", "fetch", "inspect", "stat", "status", "describe", "info", "test", "check", "verify", "validate", "browser", "lookup", "diff", "log", "show", "view", "grep"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// shouldSuppressCompletedCall 判定一个已完成过的调用是否应被压制重放：
// 只读类可以安全跳过，写入类不压制以免丢失副作用。
//
// APK 证据（agent_ledger.go:247-251，128 字节）：
// 依次调用 toolLooksObservational 与 toolLooksMutating。
func shouldSuppressCompletedCall(name string) bool {
	if toolLooksObservational(name) {
		return true
	}
	return !toolLooksMutating(strings.ToLower(strings.TrimSpace(name)))
}

// filterCompletedCalls 剔除「同名同参且已有结果」的调用，避免重复劳动。
//
// 但验证/只读类工具是例外：状态被改变之后，重新验证是必要动作而非重复劳动。
// 评测里的 run_tests 参数恒为 {}，一旦第一次调用进入 ledger，之后每次请求都
// 被剔除 —— calls 变空，网关据此判定「模型未调用工具」，追加闭环提示再推一轮，
// 模型再次请求 run_tests 又被剔除，直到 14 步耗尽。表现就是运行日志里连续的
// 「第 N 步提前结束，强制进入测试闭环」，最终因闭环未通过罚到地板分。
//
// 判定「状态已改变」以 ledger 中是否存在变更类调用为准：只要在该验证工具完成
// 之后发生过写入，就必须允许再验证一次。
func filterCompletedCalls(calls []detectedToolCall, l agentLedger) []detectedToolCall {
	out := calls[:0]
	for _, c := range calls {
		if !l.hasCompleted(c.Name, string(c.Arguments)) {
			out = append(out, c)
			continue
		}
		// 已完成过：验证/只读类工具在状态发生过变更后必须允许重新执行。
		//
		// 「状态是否变更」不能只看该调用之后 —— 模型的常见轨迹是
		// 写文件 → run_tests 失败 → 再 run_tests 确认，中间并未再写文件，
		// 此时 run_tests 本身就是最后一次完成的调用，按「之后是否有写入」
		// 判定必然为 false，验证请求仍被剔除，评测继续空转。
		// 实测 algorithm 任务连续三次「提前结束」即是此情形。
		//
		// 因此改为：只要整条 ledger 里存在过变更类调用，验证工具即可重复。
		// 变更之前的纯观察仍然受限（见 mutatedAfter 的调用点被移除后由
		// hasAnyMutation 承担），避免无意义的重复读取。
		if toolLooksObservational(c.Name) && l.hasAnyMutation() {
			out = append(out, c)
		}
	}
	return out
}

// hasAnyMutation 判断本轮对话里是否发生过变更类调用（写文件、执行命令等）。
//
// 只要有过变更，工作区状态就与最初不同，验证/只读类工具的重复调用即为必要
// 动作而非重复劳动。若全程只有观察类调用，则重复观察确实无意义，仍应剔除。
func (l agentLedger) hasAnyMutation() bool {
	for _, e := range l.Completed {
		if !toolLooksObservational(e.Name) {
			return true
		}
	}
	return false
}
func (l agentLedger) CanContinue(maxRounds int) error {
	if maxRounds <= 0 {
		maxRounds = 32
	}
	if l.ToolRounds >= maxRounds {
		return fmt.Errorf("tool round limit reached: %d", maxRounds)
	}
	if len(l.Pending) > 0 {
		return fmt.Errorf("pending tool results must be returned before another turn")
	}
	return nil
}
func maxToolRounds() int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_ROUNDS"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 512 {
			return n
		}
		return 32
	}
	if n := currentSettings().MaxToolRounds; n > 0 && n <= 512 {
		return n
	}
	return 32
}
func activeMessages(messages []oaiMsg) []oaiMsg {
	last := -1
	for i, m := range messages {
		if m.Role == "user" {
			last = i
		}
	}
	if last <= 0 {
		return messages
	}
	return messages[last:]
}

// completionEvidenceAllows 判断一段最终答复是否与 ledger 里的工具证据相符。
// 调用方（server.go 非流式路径）在返回 false 时会整段丢弃模型答复，换成一句
// 「无法确认完成」—— 所以这里每 return 一次 false，用户就收到一句模型没写过的话。
//
// 删掉的分支：ledger 为空时，曾用 unsupportedSuccess 正则
//
//	(?i)\b(installed|created|written|executed|ran|started|deployed|deleted|
//	       verified|completed|succeeded|successful(?:ly)?)\b
//
// 对答复全文宽匹配，命中即 false。它想拦的是「没调工具却宣称干完了」，实际拦的是
// 任何含有这些常见英文过去分词的句子 —— 而这些词本身就是日常英语。
//
// 实测（对运行中的网关，请求里一个工具都没声明）：
//   - 让它翻译「数据已经写入磁盘」→ 正确答复含 "written" → 整段被换成罐头话；
//   - 让它翻译「磁盘正在接收数据」→ "The disk is receiving data." 不含关键词 → 正常返回；
//   - 同一条请求，历史里有一条已完成的工具结果 → 走 len(Completed)>0 分支 → 正常返回。
//
// 一批源码审查答复也是这样被静默改写的。
//
// 根本原因不是正则不够严，而是这个分支的前提不成立：ledger 为空意味着这一轮
// 压根没有工具调用，也就没有「未经验证的外部动作」可拦。而且从签名看不出区别 ——
// 「没声明工具」和「声明了工具但一次没调」在 ledger 里都是空，纯文本启发式必然
// 在普通散文上误伤。真要拦「声明了工具却空口宣称成功」，判据在
// isToolRefusal / correctSandboxDrift 那一侧（能看见 tools），不在这里。
//
// 保留的两条判据都有实际证据支撑，不要一起删：
//   - Pending>0：客户端还欠着工具结果，此时宣称完成一定无据；
//   - Completed>0：有工具证据却说「无法确认」，与证据自相矛盾。
func completionEvidenceAllows(answer string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	if len(l.Completed) == 0 {
		// 无工具调用 = 无外部动作可核验，答复只是散文，原样放行。
		return true
	}
	low := strings.ToLower(answer)
	failureKeywords := []string{"cannot confirm", "not confirmed", "unable to confirm", "no tool result", "no matching tool results were returned", "no external action has been verified"}
	for _, h := range failureKeywords {
		if strings.Contains(low, h) {
			return false
		}
	}
	return true
}
func completedCallIDs(l agentLedger) []string {
	o := make([]string, 0, len(l.Completed))
	for _, e := range l.Completed {
		o = append(o, e.ID)
	}
	sort.Strings(o)
	return o
}
