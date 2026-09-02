package web

import (
	"strings"
	"testing"
)

// eject 路径已经额外花了一轮请求，绝不能再交出一个空的 assistant 轮：
// 客户端那时既没有 tool_calls 帧，也没有正文，连校验为什么拒了都看不到。
//
// 这条此前会被触发，而且是被大小写修复「揭出来」的：declaredFenceStart 恒返回
// -1 的时候，围栏根本没被认成围栏，于是整段作为可见正文漏了出去 —— 症状是
// 正文难看，不是空轮。等大小写修好、围栏真的被认出来之后，剥离才开始生效，
// 而这一轮的调用早已被 schema 校验拒掉，剥完就什么都不剩了。
func TestEjectDeliveryNeverReturnsAnEmptyTurn(t *testing.T) {
	tools := capitalizedClientTools()
	fence := "```" + "Read\n" + `{"path":"E:\\Temp\\notes.txt"}` + "\n" + "```" + "\n"

	for _, tc := range []struct {
		name     string
		text     string
		rejected int
	}{
		// 参数名写错（path 而不是 file_path）：整段就是一个围栏，
		// 剥离会把它剥空。
		{"schema-rejected fence is kept verbatim", fence, 1},
		// 纠正轮真的用散文回答：正常走剥离，正文本身留下。
		{"prose correction survives the strip", "已经跑完了，测试全过。", 0},
		// 散文 + 围栏，且围栏被拒：整体保留，调用方能看到模型试了什么。
		{"prose plus rejected fence keeps both", "我来读这个文件。\n\n" + fence, 1},
	} {
		got := ejectDelivery(tc.text, tc.rejected, tools, "auto")
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: delivered an empty turn for %q (rejected=%d)", tc.name, tc.text, tc.rejected)
		}
	}
}

// 被拒的调用必须原样保留，不能被剥离吃掉：调用方要能看出模型试了哪个工具、
// 参数错在哪。
func TestEjectDeliveryKeepsARejectedCallVisible(t *testing.T) {
	tools := capitalizedClientTools()
	fence := "```" + "Read\n" + `{"path":"E:\\Temp\\notes.txt"}` + "\n" + "```" + "\n"
	got := ejectDelivery(fence, 1, tools, "auto")
	for _, want := range []string{"Read", "path"} {
		if !strings.Contains(got, want) {
			t.Errorf("delivered text lost %q: %q", want, got)
		}
	}
}

// 空输入照旧返回空：没有内容可发的时候不要凭空造正文。
func TestEjectDeliveryStaysEmptyForEmptyInput(t *testing.T) {
	tools := capitalizedClientTools()
	for _, text := range []string{"", "   ", "\n\t"} {
		if got := ejectDelivery(text, 0, tools, "auto"); got != "" {
			t.Errorf("ejectDelivery(%q) = %q, want empty", text, got)
		}
	}
}
