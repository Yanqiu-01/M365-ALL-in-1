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
	// End 是候选在原文中的结束下标（参数体闭合之后）。只有指令路径的抽取器
	// 填它：末帧合并（directivesAreAdjacent）需要用它判断两条指令之间是否
	// 只隔空白。信封/围栏路径的帧边界已由 envelopeDecision 自身表达，无需此值。
	End int
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

// directiveTarget 是「CALL_TOOL 标记之后到底跟没跟一个名字」的唯一判据。
// rest 是紧跟标记之后的原文。返回名字与名字之后的剩余文本。
//
// 抽取（extractDirectiveCandidates）与定位（lastToolDirectiveIndex）必须共用
// 同一个判据：两处各写一份时会漂移 —— 定位侧曾要求标记后紧跟半角冒号，抽取侧
// 只要求「装饰之后有名字」，于是 "CALL_TOOL Read({...})" / "CALL_TOOL\nRead(...)"
// 这类无冒号写法能被抽出候选却在位置上不可见：整段回复退化成 parsed=false，
// 或者更糟 —— 选中思考里那条带冒号的旧指令。
func directiveTarget(rest string) (string, string) {
	// 冒号与强调符可以任意顺序出现：CALL_TOOL:、**CALL_TOOL:**、**CALL_TOOL**:。
	// 这个集合里只有装饰与冒号，不含任何实义字符，因此不可能越过正文去认名字。
	// 下划线不在集合里：它是合法的工具名首字符，剥掉会改名字。
	trimmed := strings.TrimLeft(rest, ": \t*")
	if strings.HasPrefix(strings.TrimLeft(rest, " \t"), "__") {
		trimmed = strings.TrimLeft(strings.TrimLeft(strings.TrimLeft(rest, " \t")[2:], "_"), ": \t*")
	}
	name, nameEnd := directiveNameSpan(trimmed)
	if name == "" {
		return "", ""
	}
	return name, trimmed[nameEnd:]
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

// bareDirectiveHead 判断指令标记是否处在行首（前面只允许装饰：空白、列表符、
// 引用符、强调符、反引号）。
//
// 只有裸名字形态需要这道判据。形态 1、2 自带 JSON 参数体，那本身就是「我在
// 发起调用」的证据，行内出现也算（TestParseToolDecisionUsesLastDirective 里
// 复述的那条就在行内）。裸名字没有任何这类证据，它唯一的凭据是规则原文要求
// 的 "end with EXACTLY one line: CALL_TOOL:" —— 真调用必然独占一行。
//
// 少了这道判据，散文里提到标记就会真的派发。实测除句点以外的所有标点、反引号
// 包裹、以及后面紧跟一个换行的写法，都会让 "Do not output CALL_TOOL: run_tests"
// 派发 run_tests；句点能逃掉只是因为 '.' 是合法名字字节（点号 MCP 名字需要它），
// 它被吞进名字变成 run_tests. 从而校验失败，属于偶然。
//
// 判据只看位置，不猜语义。不去识别否定词：那既漏（换种说法就绕过）又误伤
// （"如果测试没过，CALL_TOOL: run_tests" 是真调用）。行内的裸名字改为落空并
// 触发修复轮，是 fail-closed 的那一侧。
func bareDirectiveHead(text string, at int) bool {
	start := strings.LastIndexByte(text[:at], '\n') + 1
	return strings.Trim(text[start:at], " \t`*_>-+") == ""
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

		// 跳过标记与名字之间的冒号与装饰。判据与 lastToolDirectiveIndex 共用。
		name, tail := directiveTarget(text[offset:])
		if name == "" {
			continue
		}

		// tail 是剥掉名字之后的剩余文本，它在原文中的起始下标是：
		base := len(text) - len(tail)

		// 形态 1：括号参数。要求名字与 "(" 之间只有装饰。
		if open := strings.IndexByte(tail, '('); open >= 0 && decorationOnly(tail[:open]) {
			if close := balancedSpan(tail, open, '(', ')'); close >= 0 {
				if args, ok := decodeArguments(tail[open+1 : close]); ok {
					// base + close + 1 = 参数体 ")" 之后的原文下标。
					out = append(out, toolCandidate{Name: name, Args: args, At: at, End: base + close + 1})
				}
				continue
			}
		}
		// 形态 2：省略括号，参数是紧随其后的 JSON 对象。
		if open := strings.IndexByte(tail, '{'); open >= 0 && decorationOnly(tail[:open]) {
			if close := balancedSpan(tail, open, '{', '}'); close >= 0 {
				if args, ok := decodeArguments(tail[open : close+1]); ok {
					out = append(out, toolCandidate{Name: name, Args: args, At: at, End: base + close + 1})
				}
				continue
			}
		}
		// 形态 3：裸名字，无参数。必须独占一行：行首 + 名字后无内容。
		if bareDirectiveHead(text, at) && bareDirectiveTail(tail) {
			// 整个指令行就是一帧：End 取到行尾（含换行）。
			end := len(text)
			if nl := strings.IndexByte(text[base:], '\n'); nl >= 0 {
				end = base + nl + 1
			}
			out = append(out, toolCandidate{Name: name, Args: map[string]any{}, At: at, End: end})
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
		// 体解析不出参数时，要分两种情况，不能一律跳过、也不能一律判畸形。
		//
		// 体根本不是 JSON：这不是一次调用尝试，而是一段普通代码块。declaredTool
		// 大小写不敏感，散文里示意用的 ```bash 会命中客户端声明的 Bash，旧代码把
		// 它判成「畸形决策帧」，于是同一条消息里真正的调用被一起失败关闭（实测
		// raw_candidates=0 的一条来源）。这种要跳过。
		//
		// 体以 { 开头却解析不了：模型确实在写参数，只是写坏了（多余逗号、被截断、
		// 单引号）。这种必须记成畸形帧失败关闭 —— 跳过会让前面那一帧、也就是模型
		// 自己已经推翻的旧决策，当成末帧被真的发出去。
		// 体是合法 JSON 但不合 schema 的那种畸形，仍由下游校验失败关闭。
		if !ok {
			if !jsonObjectAttempt(text[m[4]:m[5]]) {
				continue
			}
			if prevEnd < 0 || len(out) == 0 || strings.TrimSpace(text[prevEnd:at]) != "" {
				out = append(out, envelopeDecision{At: at})
			}
			out[len(out)-1].Malformed = true
			prevEnd = end
			continue
		}
		// 相邻围栏（中间只有空白）属于同一帧，支持一次并行发起多个调用；
		// 中间有正文说明前一个已被推翻，另起一帧。
		merge := prevEnd >= 0 && len(out) > 0 && strings.TrimSpace(text[prevEnd:at]) == ""
		if !merge {
			out = append(out, envelopeDecision{At: at})
		}
		cur := &out[len(out)-1]
		cur.Calls = append(cur.Calls, toolCandidate{Name: name, Args: args, At: at})
		prevEnd = end
	}
	return out
}

// xmlNamedCall 匹配把名字放在标签属性上的 XML 形态：
// <tool_call name="Read">{...}</tool_call>（function_call / invoke 同形）。
// 必须同时出现 name= 属性与闭合标签，散文里提到 <tool_call> 字样不会命中。
var xmlNamedCall = regexp.MustCompile(`(?is)<\s*(?:tool_call|function_call|invoke|tool)\s[^>]*?\bname\s*=\s*["']([A-Za-z0-9_.-]+)["'][^>]*>(.*?)<\s*/\s*(?:tool_call|function_call|invoke|tool)\s*>`)

// extractXMLNamedDecisions 认「名字在属性里、参数是标签体 JSON」的形态。
// Hermes 风格的 <tool_call>{"name":..,"arguments":{..}}</tool_call> 走信封那条路
// 已经能解析；带 name 属性的这一变体此前无人认领，整轮 parsed=false。
//
// 名字原样交出，不做存在性判断：与其他抽取器一致，由校验阶段淘汰。
func extractXMLNamedDecisions(text string) []envelopeDecision {
	var out []envelopeDecision
	for _, m := range xmlNamedCall.FindAllStringSubmatchIndex(text, -1) {
		at := m[0]
		name := text[m[2]:m[3]]
		// 标签体只接受 JSON 对象。<arg name="k">v</arg> 这类逐项形态无法在不
		// 猜测类型的前提下还原参数值（schema 要 number 时字符串会被判非法），
		// 因此不在这里认，留给修复轮。
		args, ok := decodeArguments(text[m[4]:m[5]])
		if !ok {
			// 标签是显式的调用意图，体不可用即畸形帧，失败关闭去修复轮。
			out = append(out, envelopeDecision{At: at, Malformed: true})
			continue
		}
		out = append(out, envelopeDecision{At: at, Calls: []toolCandidate{{Name: name, Args: args, At: at}}})
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
			// 未闭合：上游文本被截断（token 上限、流中断）。这仍然是一次决策
			// 尝试，必须记成畸形帧交给修复轮，否则前面那一帧（已被模型推翻的
			// 旧决策）会被当成末帧执行。
			if envelopeShaped(text[i:]) {
				out = append(out, envelopeDecision{At: i, Malformed: true})
				break
			}
			continue
		}
		fragment := text[i : end+1]
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(fragment), &raw) != nil {
			// 解析失败但形状像信封（多余逗号、单引号、注释……）同样是畸形帧。
			// 只有「看起来就是信封」才这样记：散文里随手写的花括号不是决策，
			// 把它记成末帧会把本来能解析的一轮反过来打成 raw_candidates=0。
			if envelopeShaped(fragment) {
				out = append(out, envelopeDecision{At: i, Malformed: true})
			}
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

// jsonObjectAttempt 判断一段围栏体「本来是不是想写一个 JSON 参数对象」。
//
// 这是把散文代码块与写坏了的参数分开的唯一信号，而且必须比 json.Unmarshal 宽：
// 判断的前提正是这段文本解析不了。以 { 开头就算 —— shell、Python、SQL、纯文本
// 都不这样开头，而多余逗号、单引号、被截断的对象都还留着这个开头。
func jsonObjectAttempt(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "{")
}

// envelopeShapeKey 匹配决策信封的键：calls 系列别名，或 name 与参数键同时在场。
var envelopeShapeKey = regexp.MustCompile(`"(calls|tool_calls|toolCalls|function_calls|functionCalls)"\s*:`)
var envelopeShapeName = regexp.MustCompile(`"name"\s*:`)
var envelopeShapeArgs = regexp.MustCompile(`"(arguments|args|parameters|input|arguments_json)"\s*:`)

// envelopeShaped 判断一段解析失败的花括号文本是否「本来想当一个决策信封」。
//
// 这是把「畸形末帧」与「散文里的花括号」分开的唯一信号。不加这层判断而把每一段
// 解析不了的花括号都记成畸形帧，会让末尾随手一句带花括号的说明反过来把本来
// 成功的一轮打成失败；只看是否解析成功而完全不记，则畸形末帧会被跳过，让前面
// 那条已被推翻的旧决策执行。
func envelopeShaped(fragment string) bool {
	if envelopeShapeKey.MatchString(fragment) {
		return true
	}
	return envelopeShapeName.MatchString(fragment) && envelopeShapeArgs.MatchString(fragment)
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
		// "parameters" 在 JSON-Schema 里是 schema 外壳，不是参数值。模型复述
		// 一个工具定义时（提示词的 Available tools 就是这个形状）参数会从错误的
		// 嵌套层级取出：带必填参数的工具因此整帧失败关闭、把同一条消息里真正的
		// 指令一起丢掉；无必填参数的工具更糟 —— schema 关键字会作为参数被真的
		// 发出去。定义回声不是一次调用，整个候选作废。
		if key == "parameters" && schemaShaped(decoded) {
			return toolCandidate{}, false
		}
		args = decoded
		break
	}
	return toolCandidate{Name: strings.TrimSpace(name), Args: args, At: at}, true
}

// schemaShaped 判断一个对象是不是 JSON-Schema 外壳而不是参数值。
// 判据保持窄：只有 type:"object" 与 properties/required 同时在场才算，
// 这样 {"parameters":{"path":"a.py"}} 这类真实参数不受影响。
func schemaShaped(obj map[string]any) bool {
	if _, ok := obj["$schema"]; ok {
		return true
	}
	if typ, _ := obj["type"].(string); typ != "object" {
		return false
	}
	if _, ok := obj["properties"].(map[string]any); ok {
		return true
	}
	_, ok := obj["required"]
	return ok
}

// extractEnvelopeCandidates is retained for callers that only need the legacy
// flattened view. New decision routing must use extractEnvelopeDecisions so a

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
	// 容错解析：非法转义（Windows 路径 C:\Users 里的 \U 不是合法 JSON 转义）与
	// 字符串内的裸控制字符先抢救一次再解析。合法输入走严格路径，字节不变。
	// 见 json_salvage.go —— 修的是「网关看不懂的转义写法」，不放松 schema 校验。
	if unmarshalJSONTolerant(body, &args) {
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

// selectDecision 对候选做统一校验，返回最后一个通过的调用。
var (
	// powerShellEatenAccelerators 是 2026-09-07 直连 4141 逐字符探测确认的
	// 「会被上游 Markdown 管线吃掉前缀」的单 token 加速器方法集：方法名 →
	// 应恢复的完整前缀。上游吃的是 [xxx]:: 这类单 token 方括号字面量
	// （[string]::IsNullOrWhiteSpace 到客户端只剩 :IsNullOrWhiteSpace），带点
	// 的完整限定名 [System.String]:: 原样通过。逐个登记而不是泛匹配 :word(，
	// 避免把合法 PowerShell 冒号语法（$env:、label:）或无关残缺误伤。
	powerShellEatenAccelerators = map[string]string{
		"IsNullOrWhiteSpace": "[string]::",
		"IsNullOrEmpty":      "[string]::",
		"Format":             "[string]::",
		"Join":               "[string]::",
		"Ceiling":            "[math]::",
		"Floor":              "[math]::",
		"Max":                "[math]::",
		"Min":                "[math]::",
		"Round":              "[math]::",
		"Sqrt":               "[math]::",
		"Abs":                "[math]::",
		"GetFolderPath":      "[System.Environment]::",
		"NewGuid":            "[System.Guid]::",
		"Now":                "[System.DateTime]::",
		"Today":              "[System.DateTime]::",
		"WriteLine":          "[System.Console]::",
		"ReadLine":           "[System.Console]::",
		"ReadAllText":        "[System.IO.File]::",
		"WriteAllText":       "[System.IO.File]::",
		"Exists":             "[System.IO.File]::",
		"Combine":            "[System.IO.Path]::",
		"FromSeconds":        "[System.TimeSpan]::",
		"FromMinutes":        "[System.TimeSpan]::",
		"IsMatch":            "[System.Text.RegularExpressions.Regex]::",
		"Escape":             "[System.Text.RegularExpressions.Regex]::",
		"Replace":            "[regex]::",
	}
)

// repairPowerShellArguments restores PowerShell type accelerators eaten by the
// upstream's Markdown pipeline, which strips [xxx]:: down to : for accelerator
// literals like [string], [math], [int], [Console] ([System.String]:: with its
// dot survives — the stripper only matches single-token names). It changes only
// the command field for a case-insensitive PowerShell tool name.
//
// 两个安全阀：
//   - 前缀恢复前先查 [System.X]::Method( 是否已完整在场（前一个正则可能已
//     修过一遍 / 命令本来就是完整写法），避免拼出双重前缀；
//   - 只有前缀表里登记过的方法才恢复，不在表里的 :word( 原样保留 —— 那可能
//     是合法语法（$env:、label:）或与加速器无关的残缺，瞎补反而制造错误。
func repairPowerShellArguments(name string, args map[string]any) map[string]any {
	if !strings.EqualFold(strings.TrimSpace(name), "powershell") {
		return args
	}
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return args
	}
	repaired := pinRelativeNpmToCommandDirectory(command)
	for method, accelerator := range powerShellEatenAccelerators {
		if accelerator == "" {
			continue // 属性型（MaxValue/MinValue）暂不恢复：吃法未实测确认。
		}
		if strings.Contains(repaired, accelerator+method) {
			continue // 该方法的完整写法已在场，不重复处理。
		}
		// :Method( 前一个字符不能是 ':'（排除 [System.String]::Method( 的
		// 完整写法与 $env: 形态），也不能是标识符字符（排除 foo:Method(）。
		repaired = restoreEatenAccelerator(repaired, method, accelerator)
	}
	if repaired == command {
		return args
	}
	clone := make(map[string]any, len(args))
	for key, value := range args {
		clone[key] = value
	}
	clone["command"] = repaired
	return clone
}

// pinRelativeNpmToCommandDirectory keeps npm from inheriting the caller's
// reset shell cwd. Claude Code reports "Shell cwd was reset to ..." after each
// tool call, so a later "npm run ..." looks for package.json in the gateway
// launch folder. If the command already cds into a project, rewrite a relative
// npm invocation to --prefix <that directory>. Absolute --prefix/--cwd and
// npm -C stay untouched.
func pinRelativeNpmToCommandDirectory(command string) string {
	dir := commandChangeDirectory(command)
	if dir == "" {
		return command
	}
	return rewriteRelativeNpm(command, dir)
}

func commandChangeDirectory(command string) string {
	lower := strings.ToLower(command)
	markers := []string{"cd ", "set-location ", "push-location "}
	best := -1
	kind := ""
	for _, marker := range markers {
		idx := strings.LastIndex(lower, marker)
		if idx > best {
			best, kind = idx, marker
		}
	}
	if best < 0 {
		return ""
	}
	rest := strings.TrimSpace(command[best+len(kind):])
	if rest == "" {
		return ""
	}
	if end := indexCommandSeparator(rest); end >= 0 {
		rest = rest[:end]
	}
	dir, _ := splitPowerShellToken(rest)
	dir = strings.Trim(dir, `"'`)
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." || dir == ".." {
		return ""
	}
	if looksLikeAbsolutePath(dir) || strings.ContainsAny(dir, `/\`) {
		return dir
	}
	return ""
}

func indexCommandSeparator(command string) int {
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case ';', '|', '&', '\n', '\r':
			return i
		}
	}
	return -1
}
func splitPowerShellToken(rest string) (string, string) {
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return "", ""
	}
	if rest[0] == '\'' || rest[0] == '"' {
		quote := rest[0]
		for i := 1; i < len(rest); i++ {
			if rest[i] == quote {
				return rest[:i+1], rest[i+1:]
			}
		}
		return rest, ""
	}
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case ' ', '\t', ';', '|', '&', '\n', '\r':
			return rest[:i], rest[i:]
		}
	}
	return rest, ""
}

func looksLikeAbsolutePath(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		c := path[0]
		return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
	return false
}

func rewriteRelativeNpm(command, dir string) string {
	lower := strings.ToLower(command)
	var out strings.Builder
	out.Grow(len(command) + len(dir) + 16)
	i := 0
	changed := false
	for i < len(command) {
		idx := strings.Index(lower[i:], "npm")
		if idx < 0 {
			out.WriteString(command[i:])
			break
		}
		idx += i
		if !npmTokenStart(command, idx) {
			out.WriteString(command[i : idx+3])
			i = idx + 3
			continue
		}
		rest := command[idx+3:]
		trimmed := strings.TrimLeft(rest, " \t")
		if !npmSubcommand(trimmed) || npmHasExplicitPrefix(trimmed) {
			out.WriteString(command[i : idx+3])
			i = idx + 3
			continue
		}
		out.WriteString(command[i:idx])
		out.WriteString("npm --prefix ")
		out.WriteString(quotePowerShellPath(dir))
		out.WriteByte(' ')
		out.WriteString(trimmed)
		changed = true
		i = len(command)
	}
	if !changed {
		return command
	}
	return out.String()
}

func npmTokenStart(command string, idx int) bool {
	if idx > 0 {
		prev := command[idx-1]
		if isIdentByte(prev) || prev == '-' || prev == '.' || prev == '\\' || prev == '/' {
			return false
		}
	}
	if idx+3 < len(command) {
		next := command[idx+3]
		if isIdentByte(next) || next == '-' || next == '.' {
			return false
		}
	}
	return true
}

func npmSubcommand(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return false
	}
	cmd, _ := splitPowerShellToken(rest)
	cmd = strings.ToLower(strings.Trim(cmd, `"'`))
	switch cmd {
	case "run", "test", "start", "ci", "install", "i", "pack", "publish", "exec", "run-script":
		return true
	default:
		return false
	}
}

func npmHasExplicitPrefix(rest string) bool {
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		f := strings.ToLower(strings.Trim(fields[i], `"'`))
		switch {
		case f == "--prefix" || f == "--cwd" || f == "-c" || strings.HasPrefix(f, "--prefix=") || strings.HasPrefix(f, "--cwd="):
			return true
		}
	}
	return false
}

func quotePowerShellPath(path string) string {
	if strings.ContainsAny(path, " \t;'\"") {
		return "'" + strings.ReplaceAll(path, "'", "''") + "'"
	}
	return path
}

// restoreEatenAccelerator 把 command 里「:Method(」残迹恢复成
// 「accelerator + Method(」。逐段扫描而不是纯正则替换，因为需要逐位置看
// 冒号前一个字符；返回恢复后的完整命令。
func restoreEatenAccelerator(command, method, accelerator string) string {
	var out strings.Builder
	out.Grow(len(command) + 32)
	i := 0
	for i < len(command) {
		j := strings.Index(command[i:], ":"+method+"(")
		if j < 0 {
			out.WriteString(command[i:])
			break
		}
		j += i
		out.WriteString(command[i:j])
		// 冒号前一个字符若是 ':'（完整 :: 写法）或标识符字符（foo:Method()），
		// 这不是被吃掉的加速器，原样保留。
		if j > 0 {
			prev := command[j-1]
			if prev == ':' || isIdentByte(prev) {
				out.WriteString(":" + method + "(")
				i = j + len(":"+method+"(")
				continue
			}
		}
		out.WriteString(accelerator + method + "(")
		i = j + len(":"+method+"(")
	}
	return out.String()
}

// isIdentByte 报告 b 是否可出现在标识符中间（字母/数字/下划线），用于排除
// foo:Method( 这类与加速器无关的冒号。
func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func marshalToolArguments(name string, args map[string]any) ([]byte, error) {
	return json.Marshal(repairPowerShellArguments(name, args))
}

// selectDecision 对候选做统一校验,返回最后一个通过的调用。
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
		encoded, err := marshalToolArguments(c.Name, c.Args)
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
		encoded, err := marshalToolArguments(c.Name, c.Args)
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
