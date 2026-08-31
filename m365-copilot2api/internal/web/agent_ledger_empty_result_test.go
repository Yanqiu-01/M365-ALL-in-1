package web

import "testing"

// 用户实测报错：API Error: 409 pending tool results must be returned before
// another turn —— 而客户端确实已经返回了工具结果。
//
// 成因：ledger 用 Result == "" 判定「未应答」。空结果在协议上完全合法 —— 一条没有
// 输出的命令、一次只做写入的调用、或者 content 里只有非文本块（图片）都会得到空串。
// 于是那些调用被当成从未返回，下一轮 CanContinue 抛 409。
//
// 判据必须是「有没有收到这个 id 的 tool 消息」，与内容无关。

func toolCallMsg(id, name, args string) oaiMsg {
	return oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}}}
}

func TestEmptyToolResultCountsAsAnswered(t *testing.T) {
	// 一条没有输出的命令：tool 消息存在，content 是空串。
	messages := []oaiMsg{
		{Role: "user", Content: "touch a file"},
		toolCallMsg("call_1", "run_shell", `{"command":"New-Item x"}`),
		{Role: "tool", ToolCallID: "call_1", Content: ""},
	}
	l := buildAgentLedger(messages)
	if len(l.Pending) != 0 {
		t.Errorf("an empty tool result is still an answer; Pending=%d want 0", len(l.Pending))
	}
	if len(l.Completed) != 1 {
		t.Fatalf("Completed=%d want 1", len(l.Completed))
	}
	if err := l.CanContinue(32); err != nil {
		t.Errorf("the turn must be allowed to continue: %v", err)
	}
}

func TestWhitespaceOnlyToolResultCountsAsAnswered(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "run it"},
		toolCallMsg("call_2", "run_shell", `{"command":"echo"}`),
		{Role: "tool", ToolCallID: "call_2", Content: "   \n\t "},
	}
	l := buildAgentLedger(messages)
	if len(l.Pending) != 0 {
		t.Errorf("whitespace-only result is still an answer; Pending=%d want 0", len(l.Pending))
	}
	if err := l.CanContinue(32); err != nil {
		t.Errorf("CanContinue rejected an answered call: %v", err)
	}
}

// content 里只有图片块时 contentToString 取不到文本，结果同样为空 —— 仍算已应答。
func TestNonTextToolResultCountsAsAnswered(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "screenshot"},
		toolCallMsg("call_3", "capture", `{}`),
		{Role: "tool", ToolCallID: "call_3", Content: []any{
			map[string]any{"type": "image_url", "image_url": "data:image/png;base64,AAAA"},
		}},
	}
	l := buildAgentLedger(messages)
	if len(l.Pending) != 0 {
		t.Errorf("an image-only result is still an answer; Pending=%d want 0", len(l.Pending))
	}
	if err := l.CanContinue(32); err != nil {
		t.Errorf("CanContinue rejected an answered call: %v", err)
	}
}

// 反方向必须保住：真正没有 tool 消息的调用仍要判为 Pending。
//
// 层级说明：这一条断言的是 ledger 层。在 HTTP 上，缺失结果实际由更早的
// validateToolConversation（server.go:1597）以 400 tool_protocol_error 拦下，
// CanContinue（server.go:1615）根本轮不到 —— 见
// TestChatStillRejectsUnansweredToolCall。两道门都要各自成立，所以这里仍然要测。
func TestGenuinelyMissingToolResultStaysPending(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "read it"},
		toolCallMsg("call_4", "read_file", `{"path":"a.go"}`),
	}
	l := buildAgentLedger(messages)
	if len(l.Pending) != 1 {
		t.Fatalf("a call with no tool message must stay pending; Pending=%d want 1", len(l.Pending))
	}
	if err := l.CanContinue(32); err == nil {
		t.Error("CanContinue must refuse while a call has no result at all")
	}
}

// 混合情形：一个空结果 + 一个缺失结果，只有后者算未应答。
func TestMixedEmptyAndMissingResults(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "do two things"},
		toolCallMsg("call_5", "run_shell", `{"command":"a"}`),
		toolCallMsg("call_6", "run_shell", `{"command":"b"}`),
		{Role: "tool", ToolCallID: "call_5", Content: ""},
	}
	l := buildAgentLedger(messages)
	if len(l.Pending) != 1 {
		t.Fatalf("Pending=%d want 1 (only call_6)", len(l.Pending))
	}
	if l.Pending[0].ID != "call_6" {
		t.Errorf("pending call is %q, want call_6", l.Pending[0].ID)
	}
	if len(l.Completed) != 1 || l.Completed[0].ID != "call_5" {
		t.Errorf("completed = %+v, want call_5", l.Completed)
	}
}
