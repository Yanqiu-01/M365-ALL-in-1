package web

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// 路由决策抽取：格式无关的统一实现。
//
// 背景（为什么不再按模型逐个打补丁）：
// 路由轮要求模型「思考完，最后一行给出 CALL_TOOL: name({...}) 或
// NO_TOOL_NEEDED」；修复轮与 required 重试轮则要求 {"calls":[...]} 的 JSON。
// 实际回复的包装方式随模型、档位、语言千变万化 —— markdown 围栏、粗体、
// 行首序号、中文全角冒号、指令后追加解释、思考里复述示例 JSON……
// 逐个 if 分支去猜，每换一个模型就要返工一次。
//
// 因此这里改成两段式：
//  1. 抽取：把整段文本里所有「看起来像一次调用」的候选全部枚举出来，
//     不判断它是否合法、也不关心它被什么包裹；
//  2. 校验：对候选按出现顺序统一做「工具存在 + choice 允许 + schema 合法」
//     检查，取最后一个通过的。
//
// 「取最后一个」来自 APK 的规则原文 end with EXACTLY one line：真正的决策在
// 末尾，思考中的示例、被否决的方案都在前面。新增一种包装方式时只需让抽取器
// 多认一种边界，校验与调用点都不必改动。

// toolCandidate 是一次尚未校验的候选调用。
type toolCandidate struct {
	Name string
	Args map[string]any
	At   int // 在原文中的位置，用于「取最后一个」
}

// envelopeDecision keeps the envelope boundary as well as its candidates.
// Keeping malformed candidates instead of dropping them is important: a final
// invalid frame must trigger the caller's repair path, not resurrect a stale
// valid call from an earlier frame.
type envelopeDecision struct {
	At        int
	Calls     []toolCandidate
	Malformed bool
}

const (
	directiveMarker = "call_tool"
	noToolMarker    = "no_tool_needed"
)

// normalizeDecisionText 统一全角标点与代码围栏，使后续抽取只面对一种形态。
// 只替换等宽的 ASCII 对应物，不改变字节长度以外的语义。
func normalizeDecisionText(text string) string {
	replacer := strings.NewReplacer(
		"：", ":",
		"（", "(",
		"）", ")",
		"“", `"`,
		"”", `"`,
		"，", ",",
		"\r\n", "\n",
		"\r", "\n",
	)
	return replacer.Replace(text)
}

// stripDecorations 去掉不影响语义的包装字符，让指令行的边界可被识别。
// markdown 围栏、粗体星号、行首列表符号都属于此类。
func stripDecorations(line string) string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "`")
	line = strings.TrimSpace(line)
	// 行首的列表/序号标记：- * 1. 2) 等。
	for {
		trimmed := strings.TrimLeft(line, "-*•\t ")
		if trimmed == line {
			break
		}
		line = trimmed
	}
	if i := strings.IndexFunc(line, func(r rune) bool { return !unicode.IsDigit(r) }); i > 0 {
		if i < len(line) && (line[i] == '.' || line[i] == ')') {
			line = strings.TrimSpace(line[i+1:])
		}
	}
	// 成对的强调标记。
	for _, mark := range []string{"**", "__", "*", "_"} {
		for strings.HasPrefix(line, mark) && strings.HasSuffix(line, mark) && len(line) > 2*len(mark) {
			line = strings.TrimSpace(line[len(mark) : len(line)-len(mark)])
		}
	}
	return strings.Trim(strings.TrimSpace(line), "`")
}

// balancedSpan 从 open 位置起返回配对闭合的区间 [open, close]。
// 字符串字面量内的括号与反斜杠转义不参与配对；未闭合时返回 -1。
func balancedSpan(text string, open int, openCh, closeCh byte) int {
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// directiveNameByte 判断字节是否可以出现在工具名里。工具名来自客户端声明
// （Read、workspace_shell、mcp__server__tool 等），只含标识符字符。
func directiveNameByte(ch byte) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	case ch == '_', ch == '-', ch == '.':
		return true
	}
	return false
}

// directiveNameSpan 从 rest 起跳过装饰字符后取出名字 token，返回名字与它的
// 结束下标。装饰字符只含不影响语义的包装（引号、反引号、星号、下划线以外的
// 强调符与空白）。
func directiveNameSpan(rest string) (string, int) {
	i := 0
	// 换行也要跳过：模型会把 "CALL_TOOL:" 与名字拆到两行。跳过集合里只有
	// 空白与装饰符，因此不可能越过实义文字去认名字。
	for i < len(rest) && strings.IndexByte(" \t\r\n`*\"'", rest[i]) >= 0 {
		i++
	}
	start := i
	for i < len(rest) && directiveNameByte(rest[i]) {
		i++
	}
	return rest[start:i], i
}

// decorationOnly 判断名字与参数之间的间隔是否只有装饰：空白、围栏反引号、
// 强调符、以及围栏的 info string（```json）。有实义文字就说明这两段不属于
// 同一次调用，必须放弃，否则会把后文里无关的 JSON 当成参数。
func decorationOnly(gap string) bool {
	gap = strings.ToLower(gap)
	gap = strings.NewReplacer(
		"`", " ", "*", " ", "_", " ", "\n", " ", "\t", " ", ":", " ",
		`"`, " ", "'", " ", "=", " ", "json", " ",
	).Replace(gap)
	return strings.TrimSpace(gap) == ""
}

// bareDirectiveTail 判断指令名之后是否再无内容（只剩装饰与句末标点）。
// 只有这种情况才允许把不带参数的裸名字当成一次空参数调用。
func bareDirectiveTail(rest string) bool {
	line := rest
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return strings.Trim(strings.TrimSpace(line), " \t`*_.。!！;；,，:：)）\"'") == ""
}

// extractDirectiveCandidates 找出所有 CALL_TOOL: name <args> 形式的候选。
// 支持指令前有思考、后有解释，参数跨多行，以及各种装饰包装。
//
// 参数体接受三种真实形态（实测 gpt-5.x 三者都出现过）：
//  1. name({...}) —— 规则原文给的形状；
//  2. name {...} / name\n{...} / name\n```json\n{...}``` —— 省掉括号，参数
//     作为紧随其后的 JSON 对象。省括号是模型最常见的偏离，此前一律
//     parsed=false 并触发修复轮；
//  3. name —— 不带参数的裸名字，按空参数处理（是否合法交给 schema 校验，
//     必填参数的工具会在校验阶段被淘汰）。
func extractDirectiveCandidates(text string) []toolCandidate {
	var out []toolCandidate
	lower := strings.ToLower(text)
	for offset := 0; ; {
		idx := strings.Index(lower[offset:], directiveMarker)
		if idx < 0 {
			break
		}
		at := offset + idx
		offset = at + len(directiveMarker)

		rest := text[offset:]
		// 跳过标记与名字之间的冒号和空白。
		rest = strings.TrimLeft(rest, ": \t")
		name, nameEnd := directiveNameSpan(rest)
		if name == "" {
			continue
		}
		tail := rest[nameEnd:]

		// 形态 1：括号参数。要求名字与 "(" 之间只有装饰。
		if open := strings.IndexByte(tail, '('); open >= 0 && decorationOnly(tail[:open]) {
			if close := balancedSpan(tail, open, '(', ')'); close >= 0 {
				if args, ok := decodeArguments(tail[open+1 : close]); ok {
					out = append(out, toolCandidate{Name: name, Args: args, At: at})
				}
				continue
			}
		}
		// 形态 2：省略括号，参数是紧随其后的 JSON 对象。
		if open := strings.IndexByte(tail, '{'); open >= 0 && decorationOnly(tail[:open]) {
			if close := balancedSpan(tail, open, '{', '}'); close >= 0 {
				if args, ok := decodeArguments(tail[open : close+1]); ok {
					out = append(out, toolCandidate{Name: name, Args: args, At: at})
				}
				continue
			}
		}
		// 形态 3：裸名字，无参数。
		if bareDirectiveTail(tail) {
			out = append(out, toolCandidate{Name: name, Args: map[string]any{}, At: at})
		}
	}
	return out
}

// fencedDecisionBlock 匹配「info string 是工具名」的代码围栏。这正是
// chathub/tool_protocol.go 的 <tools> 约定：定义用 ```name 围栏给出，调用也
// 用同一形状回来。答案轮的 fencedToolCalls 一直认这种形状，路由轮却不认，
// 于是模型按被教过的协议作答反而 parsed=false。
var fencedDecisionBlock = regexp.MustCompile("(?s)```[ \t]*([A-Za-z0-9_.-]+)[ \t]*\r?\n(.*?)```")

// extractFencedDecisions 把「以工具名命名的围栏」识别成决策帧。
//
// 这里必须知道已声明的工具名 —— 不是为了做校验，而是因为边界本身依赖它：
// ```json / ```python 是普通代码块，只有 info string 恰好是一个已声明工具时
// 这段围栏才是一次调用。合法性（choice、schema）仍留在 selectAllValid。
// 因此这里绝不会凭空造出未声明的名字。
func extractFencedDecisions(text string, tools []map[string]any) []envelopeDecision {
	if len(tools) == 0 {
		return nil
	}
	var out []envelopeDecision
	prevEnd := -1
	for _, m := range fencedDecisionBlock.FindAllStringSubmatchIndex(text, -1) {
		at, end := m[0], m[1]
		name, fn := declaredTool(text[m[2]:m[3]], tools)
		if fn == nil {
			continue
		}
		args, ok := decodeArguments(text[m[4]:m[5]])
		// 相邻围栏（中间只有空白）属于同一帧，支持一次并行发起多个调用；
		// 中间有正文说明前一个已被推翻，另起一帧。
		merge := prevEnd >= 0 && len(out) > 0 && strings.TrimSpace(text[prevEnd:at]) == ""
		if !merge {
			out = append(out, envelopeDecision{At: at})
		}
		cur := &out[len(out)-1]
		if !ok {
			cur.Malformed = true
		} else {
			cur.Calls = append(cur.Calls, toolCandidate{Name: name, Args: args, At: at})
		}
		prevEnd = end
	}
	return out
}

// extractEnvelopeDecisions finds every complete {"calls":[...]} decision
// envelope. A malformed calls array is retained as a malformed decision so
// the final-decision selector can fail closed instead of treating it as an
// empty/no-tool response.
func extractEnvelopeDecisions(text string) []envelopeDecision {
	var out []envelopeDecision
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := balancedSpan(text, i, '{', '}')
		if end < 0 {
			continue
		}
		fragment := text[i : end+1]
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(fragment), &raw) != nil {
			i = end
			continue
		}
		callsRaw, has := envelopeCallsField(raw)
		if !has {
			// 裸单调用对象：{"name":...,"arguments":{...}}。修复轮只要求
			// "JSON only"，模型省掉外层 calls 数组的情况实测存在。要求
			// arguments 键同时在场，避免把散文里的 {"name":"x"} 误判成决策。
			if call, ok := parseEnvelopeCall(raw, i); ok && bareCallHasArguments(raw) {
				out = append(out, envelopeDecision{At: i, Calls: []toolCandidate{call}})
				i = end
				continue
			}
			i = end
			continue
		}
		decision := envelopeDecision{At: i}
		trimmedCalls := strings.TrimSpace(string(callsRaw))
		var items []json.RawMessage
		if trimmedCalls == "" || trimmedCalls == "null" || json.Unmarshal(callsRaw, &items) != nil {
			decision.Malformed = true
			out = append(out, decision)
			i = end
			continue
		}
		for _, item := range items {
			var call map[string]json.RawMessage
			if json.Unmarshal(item, &call) != nil {
				decision.Malformed = true
				continue
			}
			parsedCall, ok := parseEnvelopeCall(call, i)
			if !ok {
				decision.Malformed = true
				continue
			}
			decision.Calls = append(decision.Calls, parsedCall)
		}
		out = append(out, decision)
		i = end
	}
	return out
}

// bareCallHasArguments 要求裸单调用对象显式带上参数键。没有外层 calls 数组
// 时这是唯一能把「决策」与「散文里恰好提到 name 的对象」区分开的信号。
func bareCallHasArguments(raw map[string]json.RawMessage) bool {
	for _, key := range []string{"arguments", "args", "parameters", "input", "arguments_json", "function"} {
		if _, ok := raw[key]; ok {
			return true
		}
	}
	return false
}

// envelopeCallsField 取出调用数组字段。修复轮的提示词写的是 "calls"，但模型
// 常按自己熟悉的线上字段名作答（tool_calls 是 OpenAI 的名字）。这些是同一
// 语义的别名，认下来不放松任何校验。注意不含 "tools"：那是工具定义块的键，
// 认它会把提示词回声当成决策。
func envelopeCallsField(raw map[string]json.RawMessage) (json.RawMessage, bool) {
	for _, key := range []string{"calls", "tool_calls", "toolCalls", "function_calls", "functionCalls"} {
		if v, ok := raw[key]; ok {
			return v, true
		}
	}
	return nil, false
}

// parseEnvelopeCall 解析单个调用对象。三种形态：
//   - {"name":..,"arguments":{..}}          —— 提示词要求的形状
//   - {"type":"function","function":{..}}   —— OpenAI 线上形状，模型照抄
//   - arguments 是 JSON 字符串               —— 同样是 OpenAI 线上形状
//
// 名字仍原样返回，不做存在性判断：未声明的名字由 selectDecision /
// selectAllValid 与下游 validateDetectedToolCalls 淘汰。
func parseEnvelopeCall(call map[string]json.RawMessage, at int) (toolCandidate, bool) {
	if nested, ok := call["function"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) == nil {
			if _, hasName := inner["name"]; hasName {
				return parseEnvelopeCall(inner, at)
			}
		}
	}
	var name string
	rawName, ok := call["name"]
	if !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
		return toolCandidate{}, false
	}
	args := map[string]any{}
	for _, key := range []string{"arguments", "args", "parameters", "input", "arguments_json"} {
		rawArgs, has := call[key]
		if !has {
			continue
		}
		body := strings.TrimSpace(string(rawArgs))
		if body == "" || body == "null" {
			break
		}
		decoded, ok := decodeArguments(body)
		if !ok {
			return toolCandidate{}, false
		}
		args = decoded
		break
	}
	return toolCandidate{Name: strings.TrimSpace(name), Args: args, At: at}, true
}

// extractEnvelopeCandidates is retained for callers that only need the legacy
// flattened view. New decision routing must use extractEnvelopeDecisions so a
// malformed final envelope cannot be mistaken for an empty decision.
func extractEnvelopeCandidates(text string) ([]toolCandidate, bool) {
	decisions := extractEnvelopeDecisions(text)
	out := make([]toolCandidate, 0)
	for _, decision := range decisions {
		out = append(out, decision.Calls...)
	}
	return out, len(decisions) > 0
}

// decodeArguments 解析参数体。允许空参数、允许被空白或围栏包裹。
func decodeArguments(body string) (map[string]any, bool) {
	return decodeArgumentsDepth(body, 0)
}

// decodeArgumentsDepth 额外容忍两种真实出现过的形态：
//   - 双重编码：arguments 是一个 JSON 字符串，其内容才是对象。这是 OpenAI
//     线上格式（function.arguments 就是字符串），训练数据里到处都是，模型在
//     修复轮里照抄该形状的概率很高；此前一律 parsed=false。
//   - Python 展开号：read_file(**{...})。JSON 值绝不会以 * 开头，因此剥掉前导
//     星号不会放松任何真实约束。
//
// depth 限制一层解包，避免构造出的深层嵌套字符串把解析拖进递归。
func decodeArgumentsDepth(body string, depth int) (map[string]any, bool) {
	body = strings.TrimSpace(body)
	body = strings.Trim(body, "`")
	body = strings.TrimSpace(body)
	body = strings.TrimLeft(body, "*")
	body = strings.TrimSpace(body)
	if body == "" {
		return map[string]any{}, true
	}
	var args map[string]any
	if json.Unmarshal([]byte(body), &args) == nil {
		if args == nil {
			args = map[string]any{}
		}
		return args, true
	}
	if depth == 0 {
		var encoded string
		if json.Unmarshal([]byte(body), &encoded) == nil {
			return decodeArgumentsDepth(encoded, depth+1)
		}
	}
	return nil, false
}

// trailingNoToolDecision returns the final substantive decision marker. Empty
// lines and markdown fence wrappers are ignored, but any later prose makes
// the marker non-terminal. This avoids a model's quoted reasoning from
// cancelling (or reviving) a later decision.
func trailingNoToolDecision(text string) (int, bool) {
	lines := strings.Split(text, "\n")
	offsets := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		offsets[i] = offset
		offset += len(line) + 1
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.ToLower(stripDecorations(lines[i]))
		line = strings.Trim(line, " .。!！\"'`*_")
		line = strings.ToLower(stripDecorations(line))
		if line == "" {
			continue
		}
		return offsets[i], line == noToolMarker
	}
	return -1, false
}

func hasTrailingNoTool(text string) bool {
	_, ok := trailingNoToolDecision(text)
	return ok
}

// selectDecision 对候选做统一校验，返回最后一个通过的调用。
func selectDecision(candidates []toolCandidate, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if !toolChoiceAllows(choice, c.Name) {
			continue
		}
		fn := toolFunction(c.Name, tools)
		if fn == nil || schemaValid(c.Args, fn) != nil {
			continue
		}
		encoded, err := json.Marshal(c.Args)
		if err != nil {
			continue
		}
		return []detectedToolCall{{
			ID:        callID(c.Name, string(encoded), 0),
			Type:      toolType(c.Name, tools),
			Name:      c.Name,
			Arguments: encoded,
		}}, true
	}
	return nil, false
}

// selectAllValid 保留信封里全部合法调用，顺序不变（一次可返回多个工具）。
func selectAllValid(candidates []toolCandidate, tools []map[string]any, choice any) []detectedToolCall {
	out := make([]detectedToolCall, 0, len(candidates))
	for i, c := range candidates {
		if !toolChoiceAllows(choice, c.Name) {
			continue
		}
		fn := toolFunction(c.Name, tools)
		if fn == nil || schemaValid(c.Args, fn) != nil {
			continue
		}
		encoded, err := json.Marshal(c.Args)
		if err != nil {
			continue
		}
		out = append(out, detectedToolCall{
			ID:        callID(c.Name, string(encoded), i),
			Type:      toolType(c.Name, tools),
			Name:      c.Name,
			Arguments: encoded,
		})
	}
	return out
}
