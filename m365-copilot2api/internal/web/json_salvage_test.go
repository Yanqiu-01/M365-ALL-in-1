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
