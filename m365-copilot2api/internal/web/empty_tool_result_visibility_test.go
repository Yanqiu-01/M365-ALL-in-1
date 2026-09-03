package web

import (
	"strings"
	"testing"
)

// 用户实测：让模型建一个文件，它回「工具返回 <nil>，但无法确认文件是否创建」。
//
// 两处成因，都在网关这一侧：
//  1. contentToString 对 nil 走 fmt.Sprint 兜底，得到字符串 "<nil>" —— Go 的调试
//     格式冒充成了正文，一路送进 prompt 让模型当结果读。
//  2. prompt 层把空结果渲染成一个空标题行，模型无法区分「成功但无输出」「失败」和
//     「结果丢了」。
//
// 网关拿不到退出码（那是调用方该带上的），但必须如实说明自己收到的是空结果，而不是
// 假装那里有内容、更不能把内部表示当内容。

func TestContentToStringNeverLeaksNilAsText(t *testing.T) {
	if got := contentToString(nil); got != "" {
		t.Errorf("contentToString(nil) = %q, want empty; a Go debug rendering must never become content", got)
	}
	// 这个判断问的是「有没有内容」，nil 必须答「没有」。
	//
	// messageHasContent 本来就有一道 c == nil 守卫，注释里还写着「"<nil>" 是五个不
	// 是内容的字符」—— 说明这个坑在那一处早被知道并局部绕过了，却没在源头
	// contentToString 修掉。于是任何没有自带守卫的调用点仍然会拿到 "<nil>"。
	if messageHasContent(nil) {
		t.Error("nil content must not count as having text content")
	}
	// 源头修好之后，那道局部守卫应当成为冗余而非必需：去掉它也仍然正确。
	if strings.TrimSpace(contentToString(nil)) != "" {
		t.Error("contentToString(nil) must be empty on its own, without relying on a caller-side nil guard")
	}
}

// 兜底分支仍要保留：非 nil 的意外类型仍然要能被读出来，否则会静默丢内容。
func TestContentToStringStillRendersUnexpectedTypes(t *testing.T) {
	if got := contentToString(42); got != "42" {
		t.Errorf("contentToString(42) = %q, want \"42\"", got)
	}
	if got := contentToString(true); got != "true" {
		t.Errorf("contentToString(true) = %q, want \"true\"", got)
	}
}

func TestEmptyToolResultIsExplicitInThePrompt(t *testing.T) {
	cases := []struct {
		name    string
		content any
	}{
		{"empty string", ""},
		{"whitespace only", "   \n\t "},
		{"nil", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prompt, _ := flattenPromptMessages([]oaiMsg{
				{Role: "user", Content: "create a file"},
				{Role: "assistant", ToolCalls: []map[string]any{{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "run_shell", "arguments": "{}"},
				}}},
				{Role: "tool", ToolCallID: "call_1", Content: c.content},
			}, nil)

			if strings.Contains(prompt, "<nil>") {
				t.Errorf("prompt leaked a Go nil rendering:\n%s", prompt)
			}
			if !strings.Contains(prompt, "no content") {
				t.Errorf("an empty tool result must be stated, not left blank:\n%s", prompt)
			}
			// 关键：必须告诉模型这既不代表成功也不代表失败，否则它会替我们猜。
			if !strings.Contains(prompt, "does not indicate success or failure") {
				t.Errorf("the prompt must not let the model infer an outcome from emptiness:\n%s", prompt)
			}
			// 反方向：不能因为结果为空就把整条 tool 记录省掉，那样模型会以为调用没发生。
			if !strings.Contains(prompt, "tool result id=call_1") {
				t.Errorf("the tool result header must still be present:\n%s", prompt)
			}
		})
	}
}

// 有内容时不得被这条分支影响。
func TestNonEmptyToolResultIsUnchanged(t *testing.T) {
	prompt, _ := flattenPromptMessages([]oaiMsg{
		{Role: "tool", ToolCallID: "call_2", Content: "exit_code=0\nhello"},
	}, nil)
	if !strings.Contains(prompt, "exit_code=0") || !strings.Contains(prompt, "hello") {
		t.Errorf("real content was altered:\n%s", prompt)
	}
	if strings.Contains(prompt, "no content") {
		t.Errorf("a non-empty result was described as empty:\n%s", prompt)
	}
}

func TestImageOnlyToolResultIsNotDescribedAsEmpty(t *testing.T) {
	prompt, files := flattenPromptMessages([]oaiMsg{
		{Role: "user", Content: "read the png and tell me its size"},
		{Role: "assistant", ToolCalls: []map[string]any{{
			"id": "call_img", "type": "function",
			"function": map[string]any{"name": "Read", "arguments": `{"file_path":"x.png"}`},
		}}},
		{Role: "tool", ToolCallID: "call_img", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "AAAA",
			}},
		}},
	}, nil)
	if len(files) == 0 {
		t.Fatal("image-only tool result must still produce a ChatHub attachment")
	}
	if strings.Contains(prompt, "no content") {
		t.Fatalf("image-only tool result must not be described as empty:\n%s", prompt)
	}
	if !strings.Contains(prompt, "image attachment") {
		t.Fatalf("prompt must say the image was returned:\n%s", prompt)
	}
	if !strings.Contains(prompt, "not an empty result") {
		t.Fatalf("prompt must tell the model the call succeeded with an image:\n%s", prompt)
	}
	// contentToString stays the generic extractor: history byte accounting and
	// similarity hashing depend on "" for non-text content.
	if got := contentToString([]any{map[string]any{"type": "image_url", "image_url": "data:image/png;base64,AAAA"}}); got != "" {
		t.Fatalf("contentToString must stay empty for image-only content, got %q", got)
	}
	// The evidence ledger is where an image-only result must not read as empty.
	ledger := buildAgentLedger([]oaiMsg{
		{Role: "user", Content: "read the png"},
		{Role: "assistant", ToolCalls: []map[string]any{{
			"id": "call_img2", "type": "function",
			"function": map[string]any{"name": "Read", "arguments": `{"file_path":"x.png"}`},
		}}},
		{Role: "tool", ToolCallID: "call_img2", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "AAAA",
			}},
		}},
	})
	if len(ledger.Completed) != 1 {
		t.Fatalf("Completed=%d want 1", len(ledger.Completed))
	}
	if !strings.Contains(ledger.Completed[0].Result, "image attachment") {
		t.Fatalf("ledger evidence must record the returned image, got %q", ledger.Completed[0].Result)
	}
	if ledger.Completed[0].Failed {
		t.Error("an image-only result must not be recorded as a failure")
	}
	if !strings.Contains(ledger.RouterContext(), "image attachment") {
		t.Error("router/answer evidence must carry the image note")
	}
}
