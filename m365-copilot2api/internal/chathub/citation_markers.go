package chathub

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Microsoft Copilot 用 Unicode 私用区（Private Use Area）码点包裹引用锚点。
// 生产实测：上游先发 122 字节的真实正文（含附件 markdown 链接），随后用一个
// 25 字节的纯标记快照把它整段覆盖：
//
//	\ue200cite\ue202turn12file12\ue201
//
// 这三个码点是元数据分隔符，不是正文：
//
//	U+E200 引用包裹段起始
//	U+E202 段内字段分隔符（锚点名 / 锚点值）
//	U+E201 引用包裹段结束
//
// 私用区码点没有标准语义，正常散文、markdown、代码都不会使用它们，所以这里的
// 判定只看分隔符结构，不去匹配 cite 这个英文单词 —— 换语言、换锚点名同样成立。
// 对 OpenAI 兼容客户端来说这些字符是乱码（方块或问号），因此交付文本必须剥离。
const (
	citeMarkerStart = '\ue200'
	citeMarkerEnd   = '\ue201'
	citeMarkerSep   = '\ue202'

	// citeMarkerRunes 用于「这段文本里到底有没有标记」的快速判定。不含标记的
	// 文本一个字节都不会被改动（上游内容过滤定型句因此逐字节原样返回）。
	citeMarkerRunes = "\ue200\ue201\ue202"

	// citeMarkerMaxSpanBytes 限定一个包裹段的最大字节数。超过它、或者段内出现
	// 换行，就不再按包裹段处理：此时只丢分隔符本身、保留正文。这把剥离的影响
	// 半径钉死，避免一个杂散分隔符吃掉真实内容。
	citeMarkerMaxSpanBytes = 256
)

// citeSpanKind 是从一个 U+E200 起向后扫描得到的结构判定。
type citeSpanKind int

const (
	// citeSpanStray 结构不成立：这个分隔符只是落在正文里的杂散字符。
	citeSpanStray citeSpanKind = iota
	// citeSpanClosed 完整的 U+E200 … U+E201 包裹段。
	citeSpanClosed
	// citeSpanOpen 到文本末尾仍未闭合。上游快照是逐步增长的，中途某一帧正好切
	// 在标记中间就是这种形状。
	citeSpanOpen
)

// scanCiteSpan 判定 text[start:]（必须以 citeMarkerStart 开头）是不是一个标记
// 包裹段。第二个返回值在 citeSpanClosed 时是包裹段的结束下标（不含）。
func scanCiteSpan(text string, start int) (citeSpanKind, int) {
	index := start + utf8.RuneLen(citeMarkerStart)
	for index < len(text) {
		if index-start > citeMarkerMaxSpanBytes {
			return citeSpanStray, start
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		switch {
		case r == citeMarkerEnd:
			return citeSpanClosed, index + size
		case r == citeMarkerStart:
			// 嵌套起始符：结构不合法，按杂散字符处理。
			return citeSpanStray, start
		case r == '\n' || r == '\r':
			// 引用锚点不跨行，跨行说明这是正文里的杂散分隔符。
			return citeSpanStray, start
		}
		index += size
	}
	if index-start > citeMarkerMaxSpanBytes {
		return citeSpanStray, start
	}
	return citeSpanOpen, start
}

// stripCiteMarkers 剥离一段完整正文里的私用区引用标记包裹段。
//
// 只动「分隔符结构」本身：包裹段整段删除，包裹段之外的杂散分隔符只删自己，
// 其余字节逐字节保留 —— 普通文本、markdown、代码块都不会被改写。函数幂等：
// 输出不含标记，再跑一遍走零改动快路径。
func stripCiteMarkers(text string) string {
	if text == "" || !strings.ContainsAny(text, citeMarkerRunes) {
		return text
	}
	buf := make([]byte, 0, len(text))
	index := 0
	for index < len(text) {
		r, size := utf8.DecodeRuneInString(text[index:])
		switch r {
		case citeMarkerStart:
			kind, end := scanCiteSpan(text, index)
			switch kind {
			case citeSpanClosed:
				buf = trimSpaceBeforeCiteRemoval(buf, text[end:])
				index = end
			case citeSpanOpen:
				// 尾部未闭合：这是被上游快照切断的标记（长度有界、不跨行），
				// 整段剥掉。后续快照补全后结果一致。
				buf = trimSpaceBeforeCiteRemoval(buf, "")
				index = len(text)
			default:
				// 杂散起始符：只丢它本身，正文保留。
				index += size
			}
		case citeMarkerEnd, citeMarkerSep:
			// 包裹段之外的分隔符同样是元数据残渣，只丢它本身。
			index += size
		default:
			buf = append(buf, text[index:index+size]...)
			index += size
		}
	}
	return string(buf)
}

// trimSpaceBeforeCiteRemoval 是剥离后的最小空白规范化：标记前的行内空格，若其后
// 紧邻空白、句读或收尾标点，就一起去掉（`结论 \ue200…\ue201。` → `结论。`）。
//
// 刻意保守：行首缩进（空格之前就是换行或文本开头）一律保留，否则会破坏代码块和
// 列表缩进；后继是普通字符时也保留空格，不改排版语义。
func trimSpaceBeforeCiteRemoval(buf []byte, rest string) []byte {
	cut := len(buf)
	for cut > 0 && (buf[cut-1] == ' ' || buf[cut-1] == '\t') {
		cut--
	}
	if cut == len(buf) {
		return buf
	}
	// 行首缩进保留。
	if cut == 0 || buf[cut-1] == '\n' {
		return buf
	}
	if rest == "" {
		// 行尾残留空格。
		return buf[:cut]
	}
	r, _ := utf8.DecodeRuneInString(rest)
	switch {
	case r == ' ' || r == '\t' || r == '\n' || r == '\r':
		return buf[:cut]
	case unicode.In(r, unicode.Pe, unicode.Pf):
		// 收尾括号 / 收尾引号。
		return buf[:cut]
	case strings.ContainsRune("。，、；：！？…,.;:!?", r):
		return buf[:cut]
	}
	return buf
}

// visibleSnapshotText 把一个上游快照转成交付用文本，并报告它是否只是引用标记。
//
// markerOnly 为真表示这个快照剥离标记后没有任何实质内容：它是元数据，不是正文，
// 不得覆盖已经收到的更完整正文。判定基于私用区分隔符结构，与锚点名无关；不含
// 标记的文本（包括纯空白）不属于本缺陷，一律按正文对待以保持既有行为。
func visibleSnapshotText(raw string) (text string, markerOnly bool) {
	if !strings.ContainsAny(raw, citeMarkerRunes) {
		return raw, false
	}
	clean := stripCiteMarkers(raw)
	return clean, strings.TrimSpace(clean) == ""
}

// hasBodyContent 报告一个候选快照剥离引用标记后是否还有实质内容。
func hasBodyContent(raw string) bool {
	text, markerOnly := visibleSnapshotText(raw)
	return !markerOnly && strings.TrimSpace(text) != ""
}

// isCiteMarkerOnly 报告一个候选快照是否只是引用标记。
func isCiteMarkerOnly(raw string) bool {
	_, markerOnly := visibleSnapshotText(raw)
	return markerOnly
}

// trimTrailingInlineSpace 去掉文本末尾的行内空格（只含空格与制表符，不含换行）。
//
// 它保证「剥离」不会回收已经发给客户端的字节。反例：上游先发 `结论如上 `，随后
// 发 `结论如上 \ue200cite\ue202x\ue201。`；后者剥离规范化成 `结论如上。`，比前者
// 少了一个空格，于是新快照不再是已发送内容的前缀 —— 已发出的空格收不回来，紧跟
// 其后的句号也会因为找不到锚点而永远补不上，流式与非流式就此分叉。
//
// 因此行内尾随空格一律延后一帧下发：它是唯一可能被后续剥离回收的字节。被按住的
// 空格由下一个快照或 finalize 交出，不会丢。换行不做保留：剥离逻辑从不删换行。
func trimTrailingInlineSpace(text string) string {
	cut := len(text)
	for cut > 0 && (text[cut-1] == ' ' || text[cut-1] == '\t') {
		cut--
	}
	return text[:cut]
}
