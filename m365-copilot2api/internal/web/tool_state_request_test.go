package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The poisoned pair must clear the tool-protocol gate on the real request path,
// not merely in a unit test of the sanitizer. The request still fails later --
// there is no upstream account in a test process -- so this asserts only that
// it is no longer rejected as a protocol error.
func TestChatSanitizesPoisonedToolHistory(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"","type":"function","function":{"name":"","arguments":""}}]},
		{"role":"tool","tool_call_id":"","content":""},
		{"role":"user","content":"carry on"}
	]}`
	s := &Server{sessionResolver: openSessionResolver()}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if strings.Contains(w.Body.String(), "tool_protocol_error") {
		t.Fatalf("poisoned history still rejected as protocol error: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "missing id at index") {
		t.Fatalf("the 400 is still reachable: %s", w.Body.String())
	}
}

// 用户实测报错：API Error: 409 pending tool results must be returned before
// another turn —— 而客户端确实返回了结果，只是结果为空。
//
// 空结果在协议上合法：没有输出的命令、纯写入的调用、或 content 里只有图片块，
// 扁平化后都是空串。ledger 曾以 Result == "" 判「未应答」，于是这些请求在
// CanContinue（server.go:1615）被 409 拦下。这几条在真实请求路径上钉住它们能通过
// 工具协议门 —— 请求随后仍会因测试进程里没有上游账号而失败，所以只断言不再被判
// 成协议错误。
func TestChatAcceptsEmptyToolResults(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty string", `""`},
		{"whitespace only", `"   \n\t "`},
		{"image-only block array", `[{"type":"image_url","image_url":"data:image/png;base64,AAAA"}]`},
		{"explicit null", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"gpt-5.6-sol","messages":[
				{"role":"user","content":"run it"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_shell","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":` + c.content + `},
				{"role":"user","content":"carry on"}
			]}`
			s := &Server{sessionResolver: openSessionResolver()}
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			w := httptest.NewRecorder()
			s.openaiChat(w, r)
			if w.Code == 409 {
				t.Fatalf("an answered call with empty content was refused as pending: body=%s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "pending tool results") {
				t.Fatalf("still reported as pending: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// Sanitizing must not blunt the real guard: a well-formed call with no matching
// result is a genuine client bug and must still be refused.
//
// Note the layer: this is refused at 400 by validateToolConversation
// (server.go:1597), before CanContinue is ever reached. The 409 path guards a
// different case -- a call answered in an earlier turn but still open in this one.
func TestChatStillRejectsUnansweredToolCall(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"user","content":"carry on"}
	]}`
	s := &Server{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "tool_protocol_error") {
		t.Fatalf("unanswered tool call was not refused: status=%d body=%s", w.Code, w.Body.String())
	}
}
