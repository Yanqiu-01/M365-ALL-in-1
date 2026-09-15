package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
// 已知无法修复的歧义：C:\temp 里的 \t 恰好是合法 JSON 转义。那种输入是合法
// JSON，严格解析就成功，走不到这里——它由 unmarshalJSONTolerant 的第三条路径
// （reinterpretWindowsPathStrings）处理，不在这条函数的职责内。
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
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if !inString {
			if ch == '"' {
				inString = true
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
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteString(fmt.Sprintf(`\u%04x`, ch))
			}
			changed = true
		default:
			b.WriteByte(ch)
		}
	}
	if !changed {
		return body, false
	}
	return b.String(), true
}

// windowsPathControlEscape 识别「合法转义吃掉 Windows 路径」这一类损坏。
//
// C:\temp 里的 \t 是合法 JSON 转义，严格解析会成功——但解析出的字符串里出现
// 了制表符，路径已经损坏。实测（2026-09-09 审计）：C:\temp\x.md 被解析成
// C:<TAB>emp\x.md 交付给工具，工具报 file not found，模型不知道为什么。同类：
// \n → C:<LF>ew，\r → C:<CR>epo，\b → C:<BS>in，\f → C:<FF>ile。
//
// 判据（三条同时满足才算）：
//  1. 字符串以盘符开头（X:\，大小写均可）；
//  2. 字符串里含有由单反斜杠引入的 \t \n \r \b \f（真 Windows 路径的目录
//     分隔符是字面反斜杠，合法 JSON 必须写成 \\）；
//  3. 按字面读法重解析后，控制字符消失且反斜杠保留 —— 只有这样才证明「字面
//     读法」是模型的意图，避免把真要表达制表符的正常 JSON 误改成路径。
//
// 这是启发式：C:\temp 作为"字面路径"的先验远高于"字符串字面量里恰好以
// C: 开头且含真制表符"的先验，两遍解析给了我们验证的机会。
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
				pathString = looksLikeWindowsPathAt(body, i+1)
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

// unmarshalJSONTolerant 按三条路径解析，全失败才返回 false：
//
//  1. 严格 JSON —— 合法输入永远走这条，字节不变；
//  2. 抢救非法转义后重解析（两遍歧义读法：保守优先，只有保守读法也失败才试
//     「反斜杠在引号前算字面量」）；
//  3. 「合法转义损坏了 Windows 路径」的启发式重读：字符串以盘符开头、含由单
//     反斜杠引入的 \t\n\r\b\f、且按字面读法重解析后控制字符消失——此时严格
//     解析虽然"成功"，但结果是损坏的路径，字面读法才是模型意图。实测
//     C:\temp\x.md 会被严格解析成 C:<TAB>emp\x.md 交付给工具。
//
// 返回 true 只代表「解析出了合法 JSON」，与参数是否满足工具 schema 无关：
// schema 校验仍由 validateDetectedToolCalls 负责。
func unmarshalJSONTolerant(body string, out any) bool {
	// 先试「盘符路径被合法转义损坏」的字面读法。这个判定只看输入字节形状，
	// 与严格解析成败无关：C:\temp\x.md（\x 非法，严格失败）和 C:\temp（\t
	// 合法，严格"成功"但损坏）是同一类输入，必须走同一条字面路径。
	if stringLooksLikeCorruptedWindowsPath(windowsPathProbe(body)) {
		if literal, changed := reinterpretWindowsPathStrings(body); changed {
			if json.Unmarshal([]byte(literal), out) == nil {
				return true
			}
		}
	}
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
				return rest[:end]
			}
			return rest
		}
	}
	return ""
}
