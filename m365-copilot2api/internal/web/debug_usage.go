package web

import (
	"bytes"
	"encoding/json"
	"strings"
)

// 调试记录此前把 tokenSource 硬编码成 "unavailable_from_chathub"，
// inputTokens / outputTokens 一律留 nil —— 101/101 条实测全是 null。
//
// 但这个中间件本来就把响应体抓在手里（debug.go 的 cw.body），而响应体里就带着
// 网关刚算出来、刚发给客户端的 usage。也就是说数字一直在，只是记录时被扔掉，
// 然后声明「拿不到」。面板和 /debug 于是长期显示不出任何 token 计数。
//
// 上游 ChatHub 确实不返回 token 计数（compat_metadata.go:22 那句是对的），
// 所以口径必须写清是网关估算，不能冒充上游数据 —— 见 usageSourceGatewayEstimate。
const usageSourceGatewayEstimate = "gateway_estimate_chathub_reports_none"

// usageFromResponseBody 从捕获的响应体里取出 usage。
//
// 两种形状都要认：
//   - 非流式是一个 JSON 对象，usage 在顶层；
//   - 流式是 SSE，usage 落在某个 `data: {...}` 块里（server.go 的 usageChunk），
//     而且通常是最后几块之一，所以从后往前扫。
//
// Anthropic 适配器用的是 input_tokens / output_tokens，OpenAI 侧是
// prompt_tokens / completion_tokens，两套键名都收。
func usageFromResponseBody(body []byte) (inputTokens, outputTokens int, ok bool) {
	if len(body) == 0 {
		return 0, 0, false
	}
	if in, out, found := usageFromJSONObject(body); found {
		return in, out, true
	}
	// SSE：从后往前找带 usage 的那一块。
	lines := bytes.Split(body, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if in, out, found := usageFromJSONObject(payload); found {
			return in, out, true
		}
	}
	return 0, 0, false
}

// usageFromJSONObject 只认顶层 usage 和 Anthropic 的 message.usage。
func usageFromJSONObject(b []byte) (inputTokens, outputTokens int, ok bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(b, &envelope); err != nil {
		return 0, 0, false
	}
	if raw, exists := envelope["usage"]; exists {
		if in, out, found := readUsageFields(raw); found {
			return in, out, true
		}
	}
	// Anthropic message_start 把 usage 包在 message 里。
	if raw, exists := envelope["message"]; exists {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if u, exists := inner["usage"]; exists {
				if in, out, found := readUsageFields(u); found {
					return in, out, true
				}
			}
		}
	}
	return 0, 0, false
}

func readUsageFields(raw json.RawMessage) (inputTokens, outputTokens int, ok bool) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return 0, 0, false
	}
	in, foundIn := firstNumericField(fields, "prompt_tokens", "input_tokens")
	out, foundOut := firstNumericField(fields, "completion_tokens", "output_tokens")
	if !foundIn && !foundOut {
		return 0, 0, false
	}
	return in, out, true
}

func firstNumericField(fields map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		v, exists := fields[name]
		if !exists {
			continue
		}
		// json.Unmarshal 把数字解成 float64。
		if n, isNumber := v.(float64); isNumber {
			return int(n), true
		}
	}
	return 0, false
}

// debugPathReportsUsage 限定只有真正带 usage 的补全类端点才去解析响应体。
// /v1/models 之类的响应体可以到百 KB，没必要为它们反复 Unmarshal。
func debugPathReportsUsage(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	for _, suffix := range []string{
		"/chat/completions", "/completions", "/messages", "/responses",
	} {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}
