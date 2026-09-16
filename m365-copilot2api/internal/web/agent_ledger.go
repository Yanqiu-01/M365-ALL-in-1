package web

import (
	"encoding/json"
	"fmt"
	"log"
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

// ledgerResultLimit 是单条工具结果在 ledger 存储、prompt 渲染、失败判定三处
// 共用的截断上限（2026-09-09 从 4000 放大）。
//
// 4000 的年代背景是「每条结果都是一小段确认或短输出」；实际工作负载里模型
// 经常一轮读进几千行的源码/配置，头部 1/3 + 尾部的窗口让中间内容全部消失，
// 模型引用不到自己刚读过的段落，只能重读（浪费一轮）或者编造中间内容。
// 上限的存在意义只剩「防单条结果吃满整个请求预算」，不再承担「省 token」：
// 路由 prompt 的总量由 routerEvidenceMaxBytes 与 routerMaxGroups 各自兜底。
const ledgerResultLimit = 64 << 10

// compactToolResult 超限时保留头尾、省略中段。省略标记要说清三件事：
// 省了多少、中间大概是什么位置、以及「读全文请重新带范围调用」——否则模型把
// 这行当成「平台把全文截断了」（2026-09-09 用户实测），以为自己看到的就是
// 上游的全部输出，转而向用户报告内容缺失。头尾窗口让模型既能识别内容又能
// 锚定行号；真正要中段时它需要知道「用带 offset/limit 的读法去取」。
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
	// 行号锚定：头部结尾与尾部开头所在行，模型可据此发起点读取。
	headLine := 1 + strings.Count(s[:head], "\n")
	tailLine := 1 + strings.Count(s[:len(s)-tail], "\n")
	omitted := len(s) - head - tail
	// 完整版标记（~190 字节）只在面向模型的预算下使用。门槛取
	// ledgerResultLimit/4（16KB）：低于这个量级的结果要么是 benchmark 内部
	// 日志（不喂给模型），要么本身会被 head/tail 窗口基本盖满，190 字节的
	// 标记占比过高；面向模型的出口全部 ≥64KB。
	if limit >= ledgerResultLimit/4 {
		return s[:head] +
			fmt.Sprintf("\n... [gateway truncation for prompt budget: %d bytes omitted here (lines ~%d-%d); this is NOT the upstream's full output — to read the middle, re-read the source with an offset/range instead of citing it] ...\n",
				omitted, headLine, tailLine) +
			s[len(s)-tail:]
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", omitted) + s[len(s)-tail:]
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
	// OpenAI clients may return the bare Edit error without Anthropic's
	// authoritative "Error:" prefix. Keep the ledger consistent with recovery
	// for stale snapshots and identical replacements as well as not-found.
	if strings.EqualFold(strings.TrimSpace(name), "Edit") && editFailureReason(trimmed) != "" {
		return true
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
	// 命令/委派类工具的输出本身就常常「谈论」错误，而不是「是」错误：
	// Bash 跑 grep error、go test 打出 TestErrorPath PASS、构建打出
	// "0 errors"、Glob 列出 errors.go、Task 汇报「已修好那次 permission
	// denied」—— 宽匹配把这五种成功全判成失败。实测这些名字（Bash / Glob /
	// Task / TodoWrite）恰好落在两张关键字表之外，是同一处命名表不完备的
	// 另一面。
	//
	// 这类工具改按「壳层怎么报失败」判定：显式非零退出码，或开头就是错误
	// 说明。正文中间出现 error 一词不再算失败 —— 那正是它成功读到的内容。
	if toolShellLike(name) {
		return shellResultLooksFailed(compactToolResult(result, ledgerResultLimit))
	}
	return failureSignal.MatchString(compactToolResult(result, ledgerResultLimit))
}

// toolShellLike 判断工具是否属于「输出里会引用错误文本」的命令/委派类。
// 判据与 toolLooksMutating 的壳层部分同源，但这里只关心输出如何解读，
// 不关心副作用，因此 write/edit 一类不在内：它们的输出是简短确认，
// 出现 error 一词通常确实是失败。
func toolShellLike(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if toolShellNames[name] {
		return true
	}
	for _, word := range []string{"exec", "shell", "command", "bash", "terminal", "run"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// shellFailureSignal 只匹配壳层真正的失败信号：非零退出码，或行首的错误说明。
// 与 failureSignal 的差别是不含裸 \berror\b —— 命令输出引用错误文本是常态。
var shellFailureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\bsegmentation fault\b|\bcore dumped\b|command not found|no such file or directory)`)

// shellResultLooksFailed 判定命令输出是否表示失败。
func shellResultLooksFailed(result string) bool {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return false
	}
	if shellFailureSignal.MatchString(trimmed) {
		return true
	}
	// 行首的错误说明才算失败，正文中间的不算。只看前几行：失败的命令通常
	// 一开头就报错，而成功的长输出里第 40 行出现 error 一词是正常内容。
	lines := strings.SplitN(trimmed, "\n", 4)
	for i, line := range lines {
		if i >= 3 {
			break
		}
		lower := strings.ToLower(strings.TrimSpace(line))
		for _, prefix := range []string{"error", "fatal", "failed", "failure", "exception:", "traceback", "permission denied", "panic:"} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
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

// toolResultEvidenceText renders a tool result for the evidence ledger.
//
// contentToString stays the generic text extractor: history byte accounting,
// similarity hashing and token estimation all rely on it reporting "" for
// non-text content, so the attachment note must not leak in there.
//
// The ledger is different. It is what the answer turn reads back as proof of
// what the tools returned, and an image-only Read result rendered as "" made
// the answer model state that the read returned nothing -- while the image was
// attached to that very turn. Observed on a cross-provider resume: two Read
// calls on a PNG, then "both Read operations returned empty results".
func toolResultEvidenceText(content any) string {
	if text := strings.TrimSpace(contentToString(content)); text != "" {
		return contentToString(content)
	}
	if _, files := parseContent(content); len(files) > 0 {
		if note := attachmentPresenceNote(files); note != "" {
			return note
		}
	}
	return contentToString(content)
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
				raw := toolResultEvidenceText(m.Content)
				e.Result = compactToolResult(normalizeReadGutter(e.Name, raw), ledgerResultLimit)
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

// routerEvidenceFullResults is how many of the NEWEST completed calls keep
// their full result text. Older calls keep identity only.
//
// Dropping a result body is safe in a way that dropping a call is not:
// completedEvidence and filterCompletedCalls (agent_ledger.go:291, :396) match
// on Name plus canonicalised Arguments and never read Result, and every call
// site passes the real ledger struct rather than this text (server.go:1889,
// 1901, 1942, 2223, 2263, 2302). So identity is what stops the model redoing
// work, and identity is what this keeps for every call. The result body only
// has to be present while it is fresh enough to be quoted into an answer.
const routerEvidenceFullResults = 8

// routerEvidenceMaxBytes caps the serialised ledger. Identity-only entries run
// ~120 bytes. It sits in the same spirit as routerMaxGroups' 40-group cap on
// the other half of this prompt (router_history_trim.go:12).
//
// 结果保真优先于总量预算（2026-09-09）：单条结果本身的截断由 ledgerResultLimit
// 在入口处统一控制，这里只兜「条目数 × 单条」的乘积上限。之前 32KB 会在多轮
// 工作负载里把中段结果整个 elide 掉，模型引用不到自己刚读过的内容；放大后
// 配合 routerMaxGroups 的历史条数上限，实际 prompt 仍受 trim 层约束。
const routerEvidenceMaxBytes = 256 << 10

// elidedResultMarker replaces an older result body. It says the call completed
// so the model does not treat the entry as an untried action.
const elidedResultMarker = "[completed; result body elided to bound prompt size]"

// compactRouterEvidence bounds what RouterContext serialises. It returns the
// entries to emit and how many were dropped entirely.
func compactRouterEvidence(completed []toolEvidence) ([]toolEvidence, int) {
	out := make([]toolEvidence, len(completed))
	copy(out, completed)
	// Tool arguments come from the caller too. Bound every entry before the
	// list-level loop: a single newest entry cannot be dropped, so an unbounded
	// Arguments value would otherwise still dominate the router prompt.
	for i := range out {
		out[i].Arguments = compactToolResult(out[i].Arguments, ledgerResultLimit)
	}
	// Strip result bodies from all but the newest routerEvidenceFullResults.
	for i := 0; i < len(out)-routerEvidenceFullResults; i++ {
		if out[i].Result != "" {
			out[i].Result = elidedResultMarker
		}
	}
	dropped := 0
	for len(out) > 1 {
		b, err := json.Marshal(promptEvidence(out))
		if err != nil || len(b) <= routerEvidenceMaxBytes {
			break
		}
		// Oldest first. The newest evidence is the evidence that stops the model
		// repeating the step it just took, so it is never the victim.
		out = out[1:]
		dropped++
	}
	return out, dropped
}

func (l agentLedger) RouterContext() string {
	type compact struct {
		Completed    []promptToolEvidence `json:"completed"`
		Pending      []promptToolEvidence `json:"pending"`
		RepeatedCall bool                 `json:"repeated_call"`
	}
	completed, dropped := compactRouterEvidence(l.Completed)
	b, _ := json.Marshal(compact{promptEvidence(completed), promptEvidence(l.Pending), l.RepeatedCall})
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
	if dropped > 0 {
		// Say so rather than let the list read as exhaustive: a model told the
		// ledger is complete will re-run early steps it cannot see.
		hint += fmt.Sprintf(" %d earlier completed calls are omitted from this list; treat this conversation as already having more history than shown.", dropped)
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
	return l.completedEvidence(name, args) != nil
}

// completedEvidence 返回「同名同参且已应答」的那条证据，没有先例时返回 nil。
//
// filterCompletedCalls 需要的不只是「完成过没有」，还要知道那一次是成功还是失败：
// 一次失败的调用没有留下任何可复用的结果，把它当成「已完成的劳动」而压制重试，
// 等于让模型无法从一次瞬时错误（文件被占用、命令超时、网络抖动）里恢复。
func (l agentLedger) completedEvidence(name, args string) *toolEvidence {
	want := canonicalToolArguments(args)
	for i := range l.Completed {
		if l.Completed[i].Name == name && canonicalToolArguments(l.Completed[i].Arguments) == want {
			return &l.Completed[i]
		}
	}
	return nil
}

// toolObservationalNames are read-only tool names containing none of the
// substrings toolLooksObservational scans for, matched whole for the same reason
// as toolShellNames. Glob is Claude Code's file matcher and is the case that
// exposed the gap: its output is a list of paths, so a repository containing
// errors.go was enough to classify a successful match as a failure.
var toolObservationalNames = map[string]bool{
	"glob": true, "cat": true, "head": true, "tail": true, "ls": true,
	"dir": true, "tree": true, "pwd": true, "which": true, "whoami": true,
	"notebook": true, "todoread": true, "peek": true, "query": true,
	"count": true, "measure": true, "explain": true, "summarize": true,
}

// toolLooksObservational 判断工具名是否属于只读/观察类。
//
// APK 证据（tools/apktool，agent_ledger.go:237-244，192 字节）：
// TrimSpace + ToLower 后对一个全局字符串切片逐项 strings.Contains。
// 该切片位于 0x1be6000+2656，实测 22 项，内容与顺序如下。
func toolLooksObservational(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if toolObservationalNames[name] {
		return true
	}
	for _, word := range []string{"read", "list", "get", "search", "find", "fetch", "inspect", "stat", "status", "describe", "info", "test", "check", "verify", "validate", "browser", "lookup", "diff", "log", "show", "view", "grep"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// toolCanRepeatSameArguments 判断「同名同参的重复调用」有可能是必要动作，
// 而不是一定属于重复劳动。
//
// 判据是「不是明确的变更类」，而不是「是观察类」：两张关键字表都不完备，真实
// 客户端的工具名大量落在两表之外 —— Claude Code 的 Bash / Glob / Task、轮询类的
// poll_* 既不含 read/list/test 等观察词，也不含 write/exec/run 等变更词。原判据
// 只放行观察类，这些名字于是被当成变更类无条件剔除：Bash{"command":"go test ./..."}
// 与评测里的 run_tests({}) 是同一件事，只是名字没被表命中。
//
// 实测症状（2026-09-02 网关日志，13:06:13 / 13:06:36 / 13:15:24）：
// raw_candidates=1 post_ledger=0 valid_calls=0 rejected_calls=0 —— 模型选中的
// 合法工具（解析阶段已校验过名字已声明且参数合 schema）被 ledger 静默丢空，
// 该轮退化成散文或 intent_retry。
//
// observational 那一半必须保留：run_tests 同时含 "test"（观察）与 "run"（变更），
// 只按「不是变更类」判定会把它剔掉 —— 那正是要防住的那次回归。
//
// 注意：本函数的表达式与上面的 shouldSuppressCompletedCall 恰好同形，但语义相反
// （那个名字读作「应当压制」）。shouldSuppressCompletedCall 在当前代码里已无生产
// 调用方，且其注释与命名按相反极性解释同一判据，故不复用、不合并。
func toolCanRepeatSameArguments(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return toolLooksObservational(name) || !toolRewritesState(name)
}

// toolWritesToStream 判断工具写入的对象是活动进程的流（stdin/stdout/stderr），
// 而不是磁盘上的持久状态。
//
// 这类名字含 "write" 却不改写工作区。Codex 的 write_stdin{"chars":""} 是
// 「继续读取那个还在跑的会话的输出」的规范写法：同名同参重复正是它的本意 ——
// 一条 30 秒还没结束的命令，必须能一直问下去。按「含 write 即改写状态」判定，
// 第二次轮询会被 ledger 当成重复劳动剔除，而轮询恰恰只能同参重复。
//
// 实测症状（2026-09-03 20:48:50，/v1/responses，declared_tools=31）：
// exec_command 起了一条 30 秒未结束的 PowerShell 扫描（session 7773），
// write_stdin{"chars":"","session_id":7773} 轮询一次之后，模型再次请求轮询时
// raw_candidates=1 post_ledger=0 valid_calls=0 rejected_calls=0，该轮退化成
// ordinary_answer_fallback，回答写成「这个回合没有提供相应的本地 PowerShell
// 工具，因此我无法继续读取 E 盘」。工具其实声明了 31 个，是 ledger 把模型
// 选中的那一个静默丢空了 —— 模型没有幻觉，它如实描述了自己收到的东西。
func toolWritesToStream(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, word := range []string{"stdin", "stdout", "stderr"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// toolRewritesState 判断工具的用途本身就是改写状态 —— 写文件、打补丁、删除、
// 安装。这类调用同参重放是把同一次改动做两遍，必须继续剔除。
//
// 与 toolLooksMutating 的区别是「用途」与「能力」之别，两者不能混用：
// Bash 能改文件（所以 toolLooksMutating 为真，用于并行度与「状态是否变过」的
// 判定），但它的同参重放通常是改完代码后复跑一次测试 —— 那是必要动作。
// 判「能不能重复」必须问用途，问能力会把 Bash{"command":"go test"} 连同
// write_file 一起剔掉，那正是 ledger 静默丢空的成因之一。
func toolRewritesState(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	// 流写入不是持久状态改写，必须先放行：否则 write_stdin 的轮询会被当成
	// 重复的写操作剔除（见 toolWritesToStream 的实测记录）。
	if toolWritesToStream(name) {
		return false
	}
	for _, word := range []string{"write", "edit", "delete", "remove", "move", "rename", "create", "patch", "apply", "install", "update", "upload", "publish", "commit", "push", "deploy"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
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
	// 不能用 calls[:0] 复用底层数组：那会就地改写入参，调用方手上的原始候选
	// 被写坏，于是「去重把候选清空了，退回原始候选」这种补救根本无法实现 ——
	// 退回去拿到的已经是被覆盖过的内容。dedupeCompletedCalls 依赖这一点。
	out := make([]detectedToolCall, 0, len(calls))
	for _, c := range calls {
		prior := l.completedEvidence(c.Name, string(c.Arguments))
		if prior == nil {
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
		//
		// 「可重复」的判据由 toolLooksObservational 放宽为
		// toolCanRepeatSameArguments（不是明确变更类即可）：两表之外的名字
		// （Bash / Glob / poll_*）原先被无条件剔除，与 hasAnyMutation 用
		// !toolLooksObservational 把同一个名字算作「变更」自相矛盾 —— 同一份
		// 名字在相隔十几行的两处被判成相反的类别。实测症状见
		// toolCanRepeatSameArguments 的注释（post_ledger=0 静默丢空）。
		if !toolCanRepeatSameArguments(c.Name) {
			continue
		}
		// 壳层/委派类（Bash、Task）失败过的同一条命令不自动重放：半途失败的脚本
		// 可能已经删了文件、已经推了一半，重放会把副作用做第二遍。这一条必须压在
		// hasAnyMutation 之前 —— 那次失败的 Bash 本身就被算作一次变更，否则它自己
		// 就把自己的闸门顶开了。
		//
		// 只挡「同参且失败过」这一种情况。模型改完代码后主动复跑同一条测试命令走的
		// 是下面 hasAnyMutation 那条路径，不受影响。
		if prior.Failed && toolShellLike(c.Name) {
			continue
		}
		// 失败的先例不构成已完成的劳动：那一次没有产出可复用的结果。
		if prior.Failed || l.hasAnyMutation() {
			out = append(out, c)
		}
	}
	return out
}

// dedupeCompletedCalls 是 filterCompletedCalls 的唯一生产入口，它多守一条
// 不变量：**去重可以缩短候选列表，但不允许把它清空。**
//
// 为什么这条不变量比去重本身重要：
//
// filterCompletedCalls 靠 toolLooksObservational / toolRewritesState 这类
// 子串关键词表给「客户端任意起名的工具」分类。这些表按定义不可能完备 ——
// 工具名是 Codex、Claude Code、各家 MCP 自己定的字符串。判错是常态，不是意外。
// 已经踩过三次：Bash/Glob/poll_* 落在两表之外被当变更类剔除；write_stdin 含
// "write" 被当持久写入剔除；image-only 结果被当空结果。每次都是换个名字复发。
//
// 判错本身可以忍，真正致命的是判错之后的后果是**静音**的：候选归零后该轮退化成
// ordinary_answer_fallback，回答轮拿到 native_tools=0，模型如实说「这个回合没给
// 我工具」。用户看到的是「模型在幻觉」，真实故障点隔着三层，日志里只有一个
// post_ledger=0 能看出来。三次定位不到都是因为它不响。
//
// 而清空候选并不换来任何保护：防死循环的闸是 CanContinue（server.go:1748，
// 超限返 409 tool_round_limit），跟这里无关。去重规则本身也已经在 prompt 里
// 正确地告诉过模型了（见 RouterContext 的 hint：不得重放同参的变更类调用，
// 读取/检查/测试类在状态变化后可以重复）。模型看得见规则还是选了同一个调用，
// 那就照发 —— 客户端执行一次幂等写入的代价，远小于把整轮任务变成一句
// 「我没有工具」。
//
// 列表里还有其他候选时，去重照常生效：三个候选里剔掉一个重复的，剩两个，
// 永远不会归零。归零只在「模型只选了一个、而它被判成重复」时发生，那正是
// 判错代价最大的场合。
func dedupeCompletedCalls(requestID string, calls []detectedToolCall, l agentLedger) []detectedToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := filterCompletedCalls(calls, l)
	if len(out) > 0 {
		return out
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	log.Printf("[ledger-dedupe-override] id=%s kept=%d tools=%s reason=dedupe emptied a parsed candidate set; emitting instead of degrading to prose",
		requestID, len(calls), strings.Join(names, ","))
	return calls
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
