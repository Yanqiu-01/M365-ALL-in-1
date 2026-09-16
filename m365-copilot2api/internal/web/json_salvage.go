package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatHubWrapMinLine is the minimum line length in bytes for a raw newline
// inside a JSON string to be treated as a ChatHub wrapping artifact rather
// than an intentional newline. ChatHub wraps long lines at ~80 characters;
// legitimate short lines (< 60 chars) with raw newlines are preserved.
const chatHubWrapMinLine = 60

// salvageInvalidJSONEscapes 修复 JSON 字符串里「只有一种正确读法」的两类非法写法。
//
//  1. 非法转义序列。Windows 路径写进 JSON 字符串时，C:\Users 里的 \U 不是合法
//     转义（JSON 只认 \" \\ \/ \b \f \n \r \t \uXXXX）。encoding/json 报
//     "invalid character 'U' in string escape code"，整个调用随之被丢弃。
//  2. 字符串里的裸控制字符。真实换行、回车、制表符出现在字符串内，JSON 规范
//     不允许；行锚补丁类工具（edit 的 CUT/PUT 语法）的参数很容易写成这样。
//
// 这两类都不是「选错工具」或「参数含义不明」——意图完全清楚，只是转义写法不
// 合规范。此前一律静默丢弃：调用漏成可见正文，客户端收不到 tool_use，用户看到
// 的是「模型说要改文件，但什么都没发生」（2026-09-09 实测：带 C:\Users 路径的
// edit 调用被打断）。
//
// 只在严格解析已经失败之后调用，并且调用方必须重新解析修复结果——修不出合法
// JSON 就照旧算失败。这条路径不放松任何 schema 校验，只是把「网关自己看不懂
// 的转义」从静默丢弃改成尽力还原。
//
// C:\temp 里的 \t 恰好是合法 JSON 转义；仅靠本函数无法区分路径和控制字符。
// unmarshalJSONTolerant 先选择盘符字段的字面读法，再在保留该解释的候选上
// 抢救其它非法转义/裸控制字符，避免 fallback 又把路径解成控制字符。
func salvageInvalidJSONEscapes(body string) (string, bool) {
	return salvageJSONEscapes(body, false)
}

// salvageJSONEscapes 的 literalBackslashBeforeQuote 处理一处真歧义：
// {"dir":"E:\download\"} 里结尾的 \" 既可读成「转义的引号」（于是字符串没有
// 收尾，整段 JSON 报错），也可读成「字面反斜杠 + 字符串结束」——目录路径以
// 反斜杠结尾时就是后者。
//
// 保守读法（false）优先；只有它连抢救后都解析不出合法 JSON，才退一步试
// 后者，并且只在引号后的下一个非空白字符是 , } ] : 时才这样读。顺序很重要：
// "he said \"hi\"" 这类真转义引号在第一遍就解析成功，永远走不到第二遍。
func salvageJSONEscapes(body string, literalBackslashBeforeQuote bool) (string, bool) {
	var b strings.Builder
	b.Grow(len(body) + 16)
	changed := false
	inString := false
	// lineStart tracks the body offset where the current line within the
	// current JSON string began. ChatHub wraps long lines at ~80 characters,
	// inserting raw newlines into JSON string values. A raw newline after a
	// long line is a wrapping artifact; a raw newline after a short line is
	// more likely intentional (e.g. Edit's CUT/PUT syntax).
	lineStart := -1
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if !inString {
			if ch == '"' {
				inString = true
				lineStart = i + 1
			}
			b.WriteByte(ch)
			continue
		}
		switch {
		case ch == '"':
			inString = false
			b.WriteByte(ch)
		case ch == '\\':
			if i+1 >= len(body) {
				// 输入以孤立反斜杠结束：它只能是一个字面反斜杠。
				b.WriteString(`\\`)
				changed = true
				continue
			}
			next := body[i+1]
			if literalBackslashBeforeQuote && next == '"' && closesStringValue(body[i+2:]) {
				// 字面反斜杠：引号留给下一轮，由它给字符串收尾。
				b.WriteString(`\\`)
				changed = true
				continue
			}
			if validJSONEscapeByte(next) {
				// \uXXXX 必须跟四位十六进制，否则同样是非法转义。
				if next == 'u' && !hasFourHexDigits(body[i+2:]) {
					b.WriteString(`\\`)
					changed = true
					continue
				}
				b.WriteByte(ch)
				b.WriteByte(next)
				i++
				continue
			}
			// 非法转义：把反斜杠自身转义掉，后一个字符留给下一轮原样写出。
			b.WriteString(`\\`)
			changed = true
		case ch < 0x20:
			switch ch {
			case '\n', '\r':
				// ChatHub wraps long lines at ~80 chars. If the line
				// since the last raw newline is longer than the wrap
				// threshold, this raw newline is a wrapping artifact:
				// remove it to rejoin the line instead of escaping it.
				if lineStart >= 0 && i-lineStart > chatHubWrapMinLine {
					changed = true
					if ch == '\r' && i+1 < len(body) && body[i+1] == '\n' {
						i++
					}
					lineStart = i + 1
					continue
				}
				lineStart = i + 1
				if ch == '\n' {
					b.WriteString(`\n`)
				} else {
					b.WriteString(`\r`)
				}
				changed = true
			case '\t':
				lineStart = i + 1
				b.WriteString(`\t`)
				changed = true
			default:
				lineStart = i + 1
				b.WriteString(fmt.Sprintf(`\u%04x`, ch))
				changed = true
			}
		default:
			b.WriteByte(ch)
		}
	}
	if !changed {
		return body, false
	}
	return b.String(), true
}

// stringLooksLikeCorruptedWindowsPath inspects the raw JSON spelling of a
// drive-prefixed string for single-backslash \t/\n/\r/\b/\f escapes. A doubled
// backslash already represents a literal separator and is left alone.
//
// This is a heuristic, not proof of model intent: an unescaped C:\temp is much
// more likely to be a path than a string intentionally containing C:<TAB>emp.
// The caller reparses each candidate and excludes known Edit/Write text fields
// from path reinterpretation even when their contents start with a drive prefix.
func stringLooksLikeCorruptedWindowsPath(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	if !isLetter || s[1] != ':' || s[2] != '\\' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			continue
		}
		switch s[i+1] {
		case 't', 'n', 'r', 'b', 'f':
			return true
		}
		// A doubled backslash is already a literal separator. Its second
		// byte cannot also start a control escape (e.g. JSON "C:\\temp").
		i++
	}
	return false
}

// looksLikeWindowsPathAt 报告 body[i:] 是否以盘符路径开头（X:\）。
func looksLikeWindowsPathAt(body string, i int) bool {
	if i+2 >= len(body) {
		return false
	}
	c := body[i]
	isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	return isLetter && body[i+1] == ':' && body[i+2] == '\\'
}

// toolTextStringAt recognises a file-text member at the opening value quote.
// A content/old_string/new_string value can itself start with "C:" followed by
// a real newline or tab. That is still file content, not a path to reinterpret.
// Read the syntactic key without requiring the whole (possibly invalid) JSON to
// parse, and honour escaped key spellings such as "old_\u0073tring".
func toolTextStringAt(body string, quote int) bool {
	i := quote - 1
	skipSpace := func() {
		for i >= 0 && (body[i] == ' ' || body[i] == '\t' || body[i] == '\r' || body[i] == '\n') {
			i--
		}
	}
	skipSpace()
	if i < 0 || body[i] != ':' {
		return false
	}
	i--
	skipSpace()
	if i < 0 || body[i] != '"' {
		return false
	}
	end := i + 1
	for i--; i >= 0; i-- {
		if body[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && body[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			continue
		}
		var key string
		if json.Unmarshal([]byte(body[i:end]), &key) != nil {
			return false
		}
		switch strings.ToLower(key) {
		case "old_string", "new_string", "content", "contents":
			return true
		}
		return false
	}
	return false
}

// reinterpretWindowsPathStrings 只把「以盘符开头」的 JSON 字符串按字面语义
// 重写（\t → 字面反斜杠 + t）。同一段 JSON 里其它字段保持 JSON 转义语义——
// 否则 Write/Edit 的 contents/old_string/new_string 里真正的 \n 会被一并改成
// 两个字符「\n」，写到磁盘就是一整行，随后 Edit 因 old_string 对不上而失败。
// 改写后的文本必须由调用方重新解析验证——写出来不是合法 JSON 就丢弃。
func reinterpretWindowsPathStrings(body string) (string, bool) {
	var b strings.Builder
	b.Grow(len(body))
	changed := false
	inString := false
	pathString := false
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if !inString {
			if ch == '"' {
				inString = true
				pathString = looksLikeWindowsPathAt(body, i+1) && !toolTextStringAt(body, i)
			}
			b.WriteByte(ch)
			continue
		}
		switch {
		case ch == '"':
			inString = false
			pathString = false
			b.WriteByte(ch)
		case ch == '\\':
			if i+1 >= len(body) {
				b.WriteByte(ch)
				continue
			}
			next := body[i+1]
			if !pathString {
				// 非路径字符串：\n 就是换行，原样拷贝，不改语义。
				b.WriteByte(ch)
				b.WriteByte(next)
				i++
				continue
			}
			switch next {
			case '"', '\\', '/', 'u':
				// 这些保持转义语义：\" 是真引号，\\ 是真反斜杠，
				// \uXXXX 是真 Unicode。字面读法不碰它们。
				b.WriteByte(ch)
				b.WriteByte(next)
				i++
			case 't', 'n', 'r', 'b', 'f':
				// 字面读法：这五个「半转义」在字面语义下是反斜杠+字母。
				// 输出双反斜杠+字母（\\t 解析回反斜杠、t 两个字符）。
				b.WriteString(`\\`)
				b.WriteByte(next)
				i++
				changed = true
			default:
				// \x、\y 等其余一切反斜杠引入的字节：字面读法下它们同样是
				// 反斜杠+字母，同样必须双写才能在重解析后还原成字面量。
				b.WriteString(`\\`)
				b.WriteByte(next)
				i++
				changed = true
			}
		default:
			b.WriteByte(ch)
		}
	}
	if !changed {
		return body, false
	}
	return b.String(), true
}

// closesStringValue 判断紧跟引号之后的字节是否是 JSON 结构分隔符——是的话，
// 那个引号更像字符串的收尾，而不是被转义的引号字面量。
func closesStringValue(rest string) bool {
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ',', '}', ']', ':':
			return true
		default:
			return false
		}
	}
	// 输入到此结束：引号只能是收尾。
	return true
}

func validJSONEscapeByte(ch byte) bool {
	switch ch {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
		return true
	}
	return false
}

func hasFourHexDigits(rest string) bool {
	if len(rest) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		ch := rest[i]
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'a' && ch <= 'f':
		case ch >= 'A' && ch <= 'F':
		default:
			return false
		}
	}
	return true
}

// unmarshalJSONTolerant first tries the existing drive-path literal heuristic,
// then ordinary JSON. Each candidate retains its interpretation through strict
// parsing and both conservative/trailing-backslash salvage passes. Edit/Write
// text fields keep their JSON escape semantics. Success here only means valid JSON;
// validateDetectedToolCalls still enforces the declared tool schema.
func unmarshalJSONTolerant(body string, out any) bool {
	// 先试「盘符路径被合法转义损坏」的字面读法。这个判定只看输入字节形状，
	// 与严格解析成败无关：C:\temp\x.md（\x 非法，严格失败）和 C:\temp（\t
	// 合法，严格"成功"但损坏）是同一类输入，必须走同一条字面路径。
	if stringLooksLikeCorruptedWindowsPath(windowsPathProbe(body)) {
		if literal, changed := reinterpretWindowsPathStrings(body); changed {
			if unmarshalJSONWithSalvage(literal, out) {
				return true
			}
		}
	}
	return unmarshalJSONWithSalvage(body, out)
}

// Keep the selected path interpretation while escaping raw control characters
// or a trailing directory separator. Restarting salvage from the original body
// would turn a path's \r/\t back into control bytes.
func unmarshalJSONWithSalvage(body string, out any) bool {
	if json.Unmarshal([]byte(body), out) == nil {
		return true
	}
	// 保守读法优先；只有它也解析不出来，才试「反斜杠在引号前算字面量」那一遍。
	for _, literalBackslashBeforeQuote := range []bool{false, true} {
		repaired, changed := salvageJSONEscapes(body, literalBackslashBeforeQuote)
		if !changed {
			continue
		}
		if json.Unmarshal([]byte(repaired), out) == nil {
			return true
		}
	}
	return false
}

// windowsPathProbe 判断 body 里的任意 JSON 字符串值是否形如「盘符开头 + 单
// 反斜杠转义」。它扫的是原始字节（解码前），逐个检查引号包裹的片段：
// 只要有一个值命中盘符+转义字母的形状，整个 body 就按字面读法重试。
func windowsPathProbe(body string) string {
	for i := 0; i+2 < len(body); i++ {
		// 盘符形状：引号 + 字母 + 冒号 + 反斜杠，例如 "C:\t…
		if body[i] != '"' {
			continue
		}
		c := body[i+1]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if isLetter && body[i+2] == ':' && i+3 < len(body) && body[i+3] == '\\' {
			// 返回从引号后的内容开始、到下一个引号（或结尾）为止的片段。
			rest := body[i+1:]
			if end := strings.IndexByte(rest, '"'); end >= 0 {
				rest = rest[:end]
			}
			if stringLooksLikeCorruptedWindowsPath(rest) {
				return rest
			}
			// Another field may contain the damaged path. An earlier,
			// properly escaped path must not mask it.
		}
	}
	return ""
}
