package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// 反斜杠在 Go 源码里写起来极易出错，统一用变量拼，保证测试数据就是线上字节。
const bs = `\`

func TestSalvageWindowsPathEscape(t *testing.T) {
	// 线上真实形态：模型把 Windows 路径直接写进 JSON 字符串，C:\Users 的 \U
	// 不是合法 JSON 转义，严格解析报 invalid character 'U' in string escape code。
	body := `{"file_path":"C:` + bs + `Users` + bs + `ad` + bs + `a.md"}`
	if json.Unmarshal([]byte(body), &map[string]any{}) == nil {
		t.Fatalf("测试前提失效：这段本该是非法 JSON: %s", body)
	}
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("抢救失败: %s", body)
	}
	if got := args["file_path"]; got != `C:\Users\ad\a.md` {
		t.Fatalf("file_path=%q", got)
	}
}

func TestSalvageBareControlChars(t *testing.T) {
	// 字符串内的真实换行：行锚补丁类参数常写成这样，JSON 规范不允许。
	body := "{\"input\":\"CUT 1.=1\nPUT after\"}"
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("裸换行未抢救: %q", body)
	}
	if got, _ := args["input"].(string); got != "CUT 1.=1\nPUT after" {
		t.Fatalf("input=%q", got)
	}
}

func TestSalvageLeavesValidJSONByteIdentical(t *testing.T) {
	// 合法输入必须走严格路径，一个字节都不能改。
	body := `{"file_path":"C:` + bs + bs + `Users","note":"tab` + bs + `tsep","u":"` + bs + `u4e2d"}`
	var strict, tolerant map[string]any
	if err := json.Unmarshal([]byte(body), &strict); err != nil {
		t.Fatalf("测试前提失效，这段应是合法 JSON: %v", err)
	}
	if !unmarshalJSONTolerant(body, &tolerant) {
		t.Fatal("合法 JSON 被判失败")
	}
	if strict["file_path"] != tolerant["file_path"] || strict["note"] != tolerant["note"] || strict["u"] != tolerant["u"] {
		t.Fatalf("容错解析改变了合法输入: %v vs %v", strict, tolerant)
	}
	if _, changed := salvageInvalidJSONEscapes(body); changed {
		t.Fatal("合法 JSON 被改写")
	}
}

func TestSalvageTruncatedUnicodeEscape(t *testing.T) {
	// \u 后面不足四位十六进制同样是非法转义，应还原成字面反斜杠。
	body := `{"s":"100` + bs + `usd"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("截断 \\u 未抢救: %s", body)
	}
	if got := args["s"]; got != `100\usd` {
		t.Fatalf("s=%q", got)
	}
}

func TestSalvageTrailingBackslash(t *testing.T) {
	body := `{"dir":"E:` + bs + `download` + bs + `"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("目录尾反斜杠未抢救: %s", body)
	}
	if got := args["dir"]; got != `E:\download\` {
		t.Fatalf("dir=%q", got)
	}
}

func TestSalvageRejoinsChatHubWrappedLine(t *testing.T) {
	// ChatHub wraps long lines at ~80 chars, inserting raw newlines into
	// JSON string values. The salvage path must rejoin these lines instead
	// of preserving them as \n escapes in the decoded value.
	longLine := strings.Repeat("a", 80)
	body := `{"content":"` + longLine + "\n" + `tail"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("wrapped line not salvaged: %q", body)
	}
	want := longLine + "tail"
	if got, _ := args["content"].(string); got != want {
		t.Fatalf("content=%q, want %q (no embedded newline)", got, want)
	}
}

func TestSalvagePreservesShortLineRawNewline(t *testing.T) {
	// A raw newline after a short line is more likely intentional (e.g.
	// Edit's CUT/PUT syntax). It must be escaped to \n, not removed.
	body := `{"input":"short\nput"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("short line not salvaged: %q", body)
	}
	if got, _ := args["input"].(string); got != "short\nput" {
		t.Fatalf("input=%q", got)
	}
}

func TestSalvageKeepsRealEscapedQuotes(t *testing.T) {
	// 真转义引号必须原样保留。第一遍（保守读法）就该解析成功，
	// 「反斜杠算字面量」那一遍根本不该被用上。
	cases := []struct{ body, want string }{
		// 合法输入，走严格路径。
		{`{"s":"he said ` + bs + `"hi` + bs + `""}`, `he said "hi"`},
		// 非法转义与真转义引号同时出现：路径要修，引号不能动。
		{`{"s":"C:` + bs + `Users said ` + bs + `"hi` + bs + `""}`, `C:\Users said "hi"`},
	}
	for _, c := range cases {
		var args map[string]any
		if !unmarshalJSONTolerant(c.body, &args) {
			t.Fatalf("解析失败: %s", c.body)
		}
		if got := args["s"]; got != c.want {
			t.Fatalf("s=%q want %q (输入 %s)", got, c.want, c.body)
		}
	}
}

func TestSalvageTabPathLiteralReread(t *testing.T) {
	// P1 实锤回归：C:\temp\x.md 的 \t 是合法 JSON 转义，严格解析"成功"但结果
	// 是损坏的 C:<TAB>emp/x.md。字面读法必须胜出。
	cases := []struct{ body, want string }{
		{`{"file_path":"C:` + bs + `temp` + bs + `x.md"}`, `C:\temp\x.md`},
		{`{"file_path":"C:` + bs + `new` + bs + `x.md"}`, `C:\new\x.md`},
		{`{"file_path":"E:` + bs + `repo` + bs + `x.go"}`, `E:\repo\x.go`},
		{`{"file_path":"C:` + bs + `bin` + bs + `x.exe"}`, `C:\bin\x.exe`},
	}
	for _, c := range cases {
		var args map[string]any
		if !unmarshalJSONTolerant(c.body, &args) {
			t.Fatalf("解析失败: %s", c.body)
		}
		got, _ := args["file_path"].(string)
		// 逐字符比对并显示不可见字符，方便下次排查
		if got != c.want {
			show := func(v string) string {
				r := ""
				for i := 0; i < len(v); i++ {
					switch {
					case v[i] == 0x5c:
						r += "<BS>"
					case v[i] == 0x09:
						r += "<TAB>"
					default:
						r += string(v[i])
					}
				}
				return r
			}
			t.Fatalf("file_path=%s want %s (输入 %s)", show(got), show(c.want), c.body)
		}
	}
}

func TestSalvageWriteContentsNewlinesNotFlattened(t *testing.T) {
	// 回归：同一条 Write 调用里，file_path 是 C:\temp\x.go（\t 必须字面读），
	// contents 是合法 JSON 的 "a\nb\nc"（\n 必须是真换行）。字面读法如果扫
	// 整段 JSON，会把 contents 里的 \n 一并改成两个字符「\n」，写到磁盘就是
	// 一整行，随后 Edit 因 old_string 对不上而失败。
	body := `{"file_path":"C:` + bs + `temp` + bs + `x.go","contents":"package main` + bs + `n` + bs + `nfunc main() {}"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("解析失败: %s", body)
	}
	if got, _ := args["file_path"].(string); got != `C:\temp\x.go` {
		t.Fatalf("file_path=%q", got)
	}
	got, _ := args["contents"].(string)
	if got != "package main\n\nfunc main() {}" {
		t.Fatalf("contents 换行被压扁: %q", got)
	}
}

func TestSalvageEditOldStringNewlinesNotFlattened(t *testing.T) {
	body := `{"file_path":"C:` + bs + `new` + bs + `a.go","old_string":"func a() {` + bs + `n}","new_string":"func a() {` + bs + `n` + bs + `treturn` + bs + `n}"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("解析失败: %s", body)
	}
	if got, _ := args["file_path"].(string); got != `C:\new\a.go` {
		t.Fatalf("file_path=%q", got)
	}
	if got, _ := args["old_string"].(string); got != "func a() {\n}" {
		t.Fatalf("old_string 换行被压扁: %q", got)
	}
	if got, _ := args["new_string"].(string); got != "func a() {\n\treturn\n}" {
		t.Fatalf("new_string 换行被压扁: %q", got)
	}
}

func TestSalvageRealTabIntentNotCorrupted(t *testing.T) {
	// 对照组：真想表达制表符的合法 JSON 不得被改写。普通字符串没有盘符前缀，
	// 字面重读的判据不命中，原始解析结果保持不变。
	cases := []string{
		`{"text":"a` + bs + `tb"}`,                 // 非盘符
		`{"text":"value` + bs + `temp"}`,           // \t 在词中，但无盘符前缀
		`{"text":"C:` + bs + bs + `temp` + bs + bs + `x"}`, // 双反斜杠=真反斜杠，不损坏
	}
	for _, body := range cases {
		var strict map[string]any
		if json.Unmarshal([]byte(body), &strict) != nil {
			t.Fatalf("测试前提失效，应为合法 JSON: %s", body)
		}
		var tolerant map[string]any
		if !unmarshalJSONTolerant(body, &tolerant) {
			t.Fatalf("解析失败: %s", body)
		}
		if strict["text"] != tolerant["text"] {
			t.Fatalf("合法转义被改写: strict=%v tolerant=%v (%s)", strict["text"], tolerant["text"], body)
		}
	}
}

// P2 回归：同一逻辑调用以 canonical 与 inline 两种形态出现时，必须去重成一条。
// 修复前 inline 路径不走参数补齐，字节与 canonical 不同，dedup 未命中 —— 命令
// 被执行两次（2026-09-09 审计实锤）。
func TestDoubleShapeDeduplicates(t *testing.T) {
	tools := ompShellTools()
	bt := "```"
	text := bt + "bash\n{\"command\":\"ls\"}\n" + bt + "\n中间话\n" + bt + "bash({\"command\":\"ls\"})\n" + bt
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("双形态产生 %d 个调用（应为 1）: %+v", len(calls), calls)
	}
	if string(calls[0].Arguments) != `{"command":"ls","i":"run: ls"}` {
		t.Fatalf("参数补齐丢失: %s", calls[0].Arguments)
	}
}

// P3 回归：答案轮不再静默吞调用。
func TestWrongKeyShellFenceStillDispatched(t *testing.T) {
	// shell 围栏的 body 是 JSON 但没有 command 键（键名漂移）——修复前静默
	// continue，客户端只看到空回合；现在把 body 原文当命令派发，错误由调用方
	// 回传给模型。
	tools := ompShellTools()
	calls := fencedToolCalls("```bash\n{\"i\":\"list files\"}\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("wrong-key shell 围栏被吞: %d", len(calls))
	}
}

func TestTruncatedNonShellFenceFailClosed(t *testing.T) {
	// 与路由轮 fail-closed 对齐：body 解析不出的非 shell 围栏照样派发原文，
	// 让 schema 校验拒收并触发修复轮，而不是凭空消失。
	tools := editTools()
	text := "```edit\n{\"file_path\":\"broken\n```"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("截断围栏被吞: %d", len(calls))
	}
	if calls[0].Name != "Edit" || calls[0].Arguments == nil {
		t.Fatalf("fail-closed 派发形状错误: %+v", calls[0])
	}
}

func TestSalvageDoesNotInventValidJSON(t *testing.T) {
	// 转义之外的语法错误不在抢救范围内，必须照旧算失败，不能骗过调用方。
	for _, body := range []string{`{"a":}`, `{"a" "b"}`, `{`, `not json at all`} {
		var out map[string]any
		if unmarshalJSONTolerant(body, &out) {
			t.Fatalf("语法错误被误判为可解析: %q", body)
		}
	}
}

// 端到端：2026-09-09 用户实测被打断的那次 edit 调用形态 —— 围栏内联参数 +
// 单反斜杠 Windows 路径 + 字符串内真实换行。此前 JSON 解析失败后整个调用被
// 静默丢弃，漏成可见正文，客户端收不到 tool_use。
func TestBrokenEditCallNowDelivered(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "edit",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"i":     map[string]any{"type": "string"},
					"input": map[string]any{"type": "string"},
				},
				"required": []any{"i", "input"},
			},
		}},
	}
	bt := "```"
	args := `{"i":"续写第15章","input":"[C:` + bs + `Users` + bs + `ad` + bs + `a.md#B771]` + bs + `nCUT 1.=1"}`
	text := "我来续写这一章：\n" + bt + "edit(" + args + ")\n" + bt
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d (want 1)，调用仍被丢弃；text=%q", len(calls), text)
	}
	if calls[0].Name != "edit" {
		t.Fatalf("name=%q", calls[0].Name)
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
		t.Fatalf("派发出去的参数不是合法 JSON: %v", err)
	}
	input, _ := got["input"].(string)
	if !strings.Contains(input, `C:\Users\ad\a.md#B771`) {
		t.Fatalf("路径未完整还原: %q", input)
	}
	if got["i"] != "续写第15章" {
		t.Fatalf("i=%v", got["i"])
	}
}
