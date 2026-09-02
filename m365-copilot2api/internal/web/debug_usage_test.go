package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// 2026-09-02 实测：部署目录的 debug-logs.jsonl 里 101/101 条记录都是
// "inputTokens":null,"tokenSource":"unavailable_from_chathub"，一条数字都没有。
// 而同一批请求返回给客户端的响应里 usage 是齐的（网关自己估的）。
// 中间件本来就把响应体抓在手里，数字一直在，只是记录时被扔掉。

func TestUsageFromNonStreamingChatCompletion(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":188,"completion_tokens":96,"total_tokens":284}}`)
	in, out, ok := usageFromResponseBody(body)
	if !ok {
		t.Fatal("非流式响应里的 usage 没被认出来")
	}
	if in != 188 || out != 96 {
		t.Errorf("in=%d out=%d，want 188/96", in, out)
	}
}

// 流式响应是 SSE，usage 落在某个 data: 块里而不是顶层，而且后面还跟着
// [DONE]。从后往前扫必须能穿过 [DONE] 找到它。
func TestUsageFromStreamingSSEBody(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"h\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]," +
		"\"usage\":{\"prompt_tokens\":12922,\"completion_tokens\":34,\"total_tokens\":12956}}\n\n" +
		"data: [DONE]\n\n")
	in, out, ok := usageFromResponseBody(body)
	if !ok {
		t.Fatal("SSE 响应里的 usage 没被认出来（它在 data: 块里，不在顶层）")
	}
	if in != 12922 || out != 34 {
		t.Errorf("in=%d out=%d，want 12922/34", in, out)
	}
}

// Anthropic 适配器用 input_tokens / output_tokens，且 usage 包在 message 里。
func TestUsageFromAnthropicMessageShape(t *testing.T) {
	body := []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message",` +
		`"role":"assistant","usage":{"input_tokens":4210,"output_tokens":17}}}`)
	in, out, ok := usageFromResponseBody(body)
	if !ok {
		t.Fatal("Anthropic message.usage 没被认出来")
	}
	if in != 4210 || out != 17 {
		t.Errorf("in=%d out=%d，want 4210/17", in, out)
	}
}

func TestUsageAbsentStaysAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"empty":              "",
		"no_usage":           `{"id":"chatcmpl-1","choices":[]}`,
		"not_json":           "web interface unavailable",
		"sse_without_usage":  "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n",
		"usage_wrong_type":   `{"usage":{"prompt_tokens":"lots"}}`,
		"models_list_shaped": `{"data":[{"id":"gpt-5.6"}]}`,
	} {
		if _, _, ok := usageFromResponseBody([]byte(body)); ok {
			t.Errorf("%s：不该报出 usage", name)
		}
	}
}

// 只有补全类端点才解析响应体。/v1/models 的响应体实测有 122444 字节，
// 为它反复 Unmarshal 纯属浪费。
func TestOnlyCompletionPathsAreParsedForUsage(t *testing.T) {
	for _, path := range []string{
		"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions",
	} {
		if !debugPathReportsUsage(path) {
			t.Errorf("%s 应当解析 usage", path)
		}
	}
	for _, path := range []string{
		"/v1/models", "/debug", "/panel", "/api/admin/session", "/favicon.ico", "/",
	} {
		if debugPathReportsUsage(path) {
			t.Errorf("%s 不该解析 usage", path)
		}
	}
}

// 口径不能冒充上游。ChatHub 确实不返回 token 计数，填进去的是网关估算，
// 记录里必须说清是估算，否则读日志的人会把它当成计费数据。
func TestTokenSourceSaysItIsAGatewayEstimate(t *testing.T) {
	if strings.Contains(usageSourceGatewayEstimate, "upstream") {
		t.Errorf("tokenSource=%q 会被读成上游数据", usageSourceGatewayEstimate)
	}
	for _, want := range []string{"estimate", "chathub"} {
		if !strings.Contains(usageSourceGatewayEstimate, want) {
			t.Errorf("tokenSource=%q 缺少 %q，说不清它的来源", usageSourceGatewayEstimate, want)
		}
	}
}

// 记录序列化后必须是数字而不是 null —— 这正是线上那 101 条的症状。
func TestDebugRecordSerialisesNumericTokens(t *testing.T) {
	in, out := 188, 96
	rec := debugRecord{
		InputTokens: &in, OutputTokens: &out, TokenSource: usageSourceGatewayEstimate,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if strings.Contains(got, `"inputTokens":null`) {
		t.Errorf("inputTokens 仍然是 null：%s", got)
	}
	for _, want := range []string{`"inputTokens":188`, `"outputTokens":96`} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %s\n实际: %s", want, got)
		}
	}
}
