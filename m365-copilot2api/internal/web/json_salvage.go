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
// 已知无法修复的歧义：C:\temp 里的 \t 恰好是合法 JSON 转义，会被解析成制表符。
// 那种输入是合法 JSON，严格解析就成功了，根本走不到这里；要正确表达只能靠
// 模型写双反斜杠或正斜杠，网关无从分辨。
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

// unmarshalJSONTolerant 先按严格 JSON 解析；失败时用 salvageInvalidJSONEscapes
// 修一次转义再解析。合法输入永远走第一条路径，字节不变。
//
// 返回 true 只代表「解析出了合法 JSON」，与参数是否满足工具 schema 无关：
// schema 校验仍由 validateDetectedToolCalls 负责。
func unmarshalJSONTolerant(body string, out any) bool {
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
