package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude CLI 报 "502 model returned an invalid tool routing decision" 的根因：
// 路由器返回自然语言（而非 JSON 决策信封）时，parseModelToolDecision 的
// parsed=false 被当成致命错误直接 502。而 parsed=false 的绝大多数来源恰恰是
// 模型用散文回答了问题 —— 那是「本轮不调用工具」，不是请求失败。
func TestProseRouterOutputIsUndecidableNotFatal(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "Read",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
				"required":   []any{"file_path"},
			},
		},
	}}
	prose := []string{
		"I will read the file for you.",
		"Sure, let me help with that. First I need to look at the code.",
		"抱歉，我无法完成这个请求。",
	}
	for _, text := range prose {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if parsed {
			t.Fatalf("prose unexpectedly parsed as a decision: %q", text)
		}
		if len(calls) != 0 {
			t.Fatalf("prose produced %d calls: %q", len(calls), text)
		}
		// auto 模式下这不是必须失败的情形，网关应降级为普通回答。
		if toolChoiceRequiresToolCall("auto") {
			t.Fatal("auto must not require a tool call")
		}
	}
	// 只有明确要求必须调用工具时，无法决策才算真失败。
	if !toolChoiceRequiresToolCall("required") {
		t.Fatal("required must require a tool call")
	}
}

// Anthropic 的 base64 source 里 data 是裸 base64，不带 "data:" 前缀。
// 之前它被原样当作远程 URL 交给 chathub，触发 SSRF 校验并报
// "upload attachment: attachment download requires https" —— 整个请求 502。
func TestAnthropicBase64SourceBecomesDataURL(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "look at this"},
		map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       "iVBORw0KGgoAAAANSUhEUg==",
			},
		},
	}
	text, files := parseContent(content)
	if !strings.Contains(text, "look at this") {
		t.Fatalf("text lost: %q", text)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(files))
	}
	got := files[0].URL
	want := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	if got != want {
		t.Fatalf("attachment URL not normalized\n got: %q\nwant: %q", got, want)
	}
	if !strings.HasPrefix(got, "data:") {
		t.Fatal("attachment must be a data URL so chathub skips the remote download path")
	}
}

// tool_result 内嵌的 image 块不经过顶层 anthropicRequest.openAI() 转换，
// 会带着原始 source 落到 parseContent。这是 Claude CLI 下的必经路径。
func TestNestedToolResultImageSourceNormalized(t *testing.T) {
	raw := []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]`)
	var content any
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatal(err)
	}
	_, files := parseContent(content)
	if len(files) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(files))
	}
	if files[0].URL != "data:image/jpeg;base64,AAAA" {
		t.Fatalf("unexpected URL: %q", files[0].URL)
	}
}

// url 形式的 source 必须保持原样，不能被包成 data URL。
func TestURLSourcePassesThrough(t *testing.T) {
	content := []any{map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": "https://example.com/a.png"},
	}}
	_, files := parseContent(content)
	if len(files) != 1 || files[0].URL != "https://example.com/a.png" {
		t.Fatalf("url source mangled: %+v", files)
	}
}

// 同一张图不能被解析成两个附件。顶层 anthropicRequest.openAI() 会把 Anthropic
// 的 image 块转成 {"type":"input_image","image_url":"data:..."}，而 parseContent
// 里的前置兜底分支和 case "input_image" 都会消费 image_url —— 于是 1 张图变成
// attachments=2，UploadFile 被调用两次（日志实测）。
func TestSingleImageProducesSingleAttachment(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "describe"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
	}
	_, files := parseContent(content)
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 attachment, got %d: %+v", len(files), files)
	}
	if files[0].URL != "data:image/png;base64,AAAA" {
		t.Fatalf("unexpected URL: %q", files[0].URL)
	}
}

// 走完整的 Anthropic -> OpenAI -> parseContent 链路，确认端到端只有一个附件。
func TestAnthropicImageEndToEndSingleAttachment(t *testing.T) {
	r := anthropicRequest{
		Model: "m365-copilot",
		Messages: []anthropicMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "describe"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "AAAA",
				}},
			},
		}},
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	_, files := flattenPromptMessages(o.Messages, nil)
	if len(files) != 1 {
		t.Fatalf("expected 1 attachment end-to-end, got %d: %+v", len(files), files)
	}
	if files[0].URL != "data:image/png;base64,AAAA" {
		t.Fatalf("unexpected URL: %q", files[0].URL)
	}
}

// image_url 作为裸字符串出现在 type=image_url 里时不能被丢掉。
func TestImageURLAsPlainStringKept(t *testing.T) {
	content := []any{map[string]any{"type": "image_url", "image_url": "https://example.com/x.png"}}
	_, files := parseContent(content)
	if len(files) != 1 || files[0].URL != "https://example.com/x.png" {
		t.Fatalf("string image_url mishandled: %+v", files)
	}
}
