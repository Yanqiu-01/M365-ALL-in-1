package chathub

import "strings"

// ChatHub 的 writeAtCursor 有两种语义，由同一个 argument 里的 streamingMode 区分。
//
// M365_TRACE=1 抓到的真实帧序（请求：让模型用 write_file 写 hello.py）：
//
//	{"writeAtCursor":"hello","streamingMode":"Delta",...}
//	{"writeAtCursor":".py","streamingMode":"Delta",...}
//	{"writeAtCursor":"](https://kr-prod.asyncgw.teams.microsoft.com/.../hello.py)","streamingMode":"Delta",...}
//
// 这三帧是增量，不是快照。把它们当完整快照会让三段互相替换而不是拼接，最终只
// 剩第三段 `](url)`，`hello` 与 `.py` 被静默丢弃（生产实测 res.Text bytes=123）。
//
// 判定只看 streamingMode 这一个字段，取值大小写不敏感。缺省或其他取值一律保持
// 既有的完整快照语义 —— 上游确实会用完整快照下发正文（同一份日志里 `pong` 那次
// 请求就是 bot messages[].text 全量快照），因此不能反过来把快照当增量。
//
// 注意：请求侧 payload 里的 "streamingMode":"ConciseWithPadding" 是客户端声明的
// 输出风格，与响应帧的这个字段无关，不构成增量标识。
const deltaStreamingMode = "delta"

// isDeltaStreamingMode 报告一个 update argument 是否声明了增量语义。
func isDeltaStreamingMode(arg map[string]any) bool {
	mode, _ := arg["streamingMode"].(string)
	return strings.EqualFold(strings.TrimSpace(mode), deltaStreamingMode)
}

// updateText 是从一个 update argument 里取到的一段正文，并带上它的语义：
// delta 为真表示这是增量片段，必须与前文拼接；为假表示这是完整快照，按组 J
// 建立的「最终文本 = 上游最后一个完整快照」规则处理。
type updateText struct {
	text  string
	delta bool
}

// updateTexts 按上游到达顺序提取一个 type=1 target=update argument 里的正文。
//
// 取材范围与此前完全一致：writeAtCursor（工具/进度帧除外）以及 author=bot 且无
// messageType 的 messages[].text。唯一的新增信息是 delta 标记 —— 它只作用于
// writeAtCursor，因为 streamingMode 是与 writeAtCursor 同级的字段；
// messages[].text 是上游持久化后的整条消息正文，始终是完整快照。
//
// 单独成函数是为了让「增量被当成快照」这类缺陷能在不连接上游 WebSocket 的情况下
// 被回归测试覆盖。
func updateTexts(arg map[string]any, messages []any) []updateText {
	var out []updateText
	toolFrame := false
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		messageType, _ := message["messageType"].(string)
		contentType, _ := message["contentType"].(string)
		if messageType == "Progress" || contentType == "SearchResults" || contentType == "Code" || contentType == "ToolCall" {
			toolFrame = true
		}
	}
	if cursor, ok := arg["writeAtCursor"].(string); ok && cursor != "" && !toolFrame {
		out = append(out, updateText{text: cursor, delta: isDeltaStreamingMode(arg)})
	}
	items, _ := arg["messages"].([]any)
	for _, raw := range items {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(string)
		text, _ := message["text"].(string)
		messageType, _ := message["messageType"].(string)
		// 完整快照与 writeAtCursor 增量可以出现在同一次响应里（增量写入
		// adaptiveCards 的 body，整条消息随后被广播一次）。两者混合时共用
		// snapshotTracker 的一套规则去重，而不是各自维护一份正文。
		if author == "bot" && messageType == "" && text != "" {
			out = append(out, updateText{text: text})
		}
	}
	return out
}

// updateSnapshots 保留只关心「取到哪些正文」的旧视图，语义信息由 updateTexts
// 提供。生产路径走 updateTexts。
func updateSnapshots(arg map[string]any, messages []any) []string {
	texts := updateTexts(arg, messages)
	if len(texts) == 0 {
		return nil
	}
	out := make([]string, 0, len(texts))
	for _, item := range texts {
		out = append(out, item.text)
	}
	return out
}

// cursorAccumulator 把 streamingMode=Delta 的 writeAtCursor 片段还原成完整快照。
//
// 之所以还原成快照、而不是把片段直接丢给 pushDelta：组 J 建立的不变量是「最终
// 文本 = 上游最后一个完整快照」，snapshotTracker、引用标记剥离、行内尾随空格延后
// 下发、非前缀重写的锚点续写全部挂在 pushSnapshot 这一个入口上。增量若绕过它，
// 就会有一部分正文从不进入 canon，混合到达时最终文本要么丢内容要么复读。在入口
// 处把增量归一成快照，两种上游语义就共用同一套规则，可测面也不变。
type cursorAccumulator struct {
	// text 是已还原的光标正文，只由上游真实发过的字节组成，不含任何本地补齐的
	// 内容。
	text string
	// deltas 是并入的增量片段数，仅用于可观测性。
	deltas int
}

// push 并入一个增量片段，返回它等价的完整快照。
//
// base 是已知的最完整正文（snapshotTracker 的 canon）。writeAtCursor 的字面语义是
// 「在光标处写入」，光标落在已写出正文的末尾，因此只要 base 与本地还原一致
// （base 以还原内容为前缀），快照就取 base+delta —— 先到的完整快照因此不会被后续
// 增量截断，也不会被重复下发。base 与还原内容分叉（上游重写过正文）时退化为纯
// 累积，由 pushSnapshot 既有的重写路径校正。
//
// 两条路径都只拼接上游发过的字节，不会凭空造出内容：实测中缺失的那个前导 `[`
// 不在文本流里，这里也不会把它补出来。
//
// 刻意不做「相同片段去重」：增量里重复出现同一串文本是合法的（`ha` + `ha`），
// 去重会真的丢内容。防复读由 pushSnapshot 侧的前缀判定负责。
func (c *cursorAccumulator) push(base, delta string) string {
	if c == nil {
		return delta
	}
	c.deltas++
	// c.text == "" 时 HasPrefix 恒真，于是首个增量接在已有的完整快照之后 ——
	// 这正是「先发完整快照、随后跟光标增量」那种到达顺序。
	if base != "" && strings.HasPrefix(base, c.text) {
		c.text = base + delta
		return c.text
	}
	c.text += delta
	return c.text
}
