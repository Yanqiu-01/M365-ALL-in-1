package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 面向模型的端点都必须有入口请求体上限。
//
// 原先只有 /v1/chat/completions 有（server.go 里的 maxChatRequestBody），
// /v1/messages、/v1/responses、/v1/messages/count_tokens 三个都是裸的
// json.NewDecoder(r.Body)。这三个走的是同一套「先解析成自己的协议结构、再转成
// 内层 oaiReq 交给 openaiChat」的路子（protocol_handlers.go:395 起），所以外层
// 请求体在内层那道 MaxBytesReader 生效之前就已经整个读进内存了 —— 内层拦的是重新
// marshal 出来的 body，对外层一个字节都拦不住。
//
// 这条测试扫源码而不是发请求：要触发上限得造一个超过 10 MiB 的请求体，作为单元
// 测试代价过高，而「解码器有没有套上 MaxBytesReader」在源码上是确定的事实。
// 仓库里已有扫源码的先例（web_assets_test.go、procwin_test.go）。
func TestModelFacingEndpointsCapTheIngressBody(t *testing.T) {
	// 端点 -> 声明它的文件。三个协议端点各自解析自己的请求体。
	for _, target := range []struct {
		file    string
		handler string
	}{
		{"protocol_handlers.go", "anthropicMessages"},
		{"protocol_handlers.go", "responses"},
		{"anthropic_count_tokens.go", "anthropicCountTokens"},
	} {
		src, err := os.ReadFile(target.file)
		if err != nil {
			t.Fatalf("read %s: %v", target.file, err)
		}
		body := handlerBody(t, string(src), target.handler)
		if !strings.Contains(body, "MaxBytesReader") {
			t.Errorf("%s（%s）直接从 r.Body 解码，没有入口上限：\n%s",
				target.handler, target.file, firstLines(body, 8))
			continue
		}
		if !strings.Contains(body, "maxChatRequestBody") {
			t.Errorf("%s（%s）用了自己的上限数字而不是共用的 maxChatRequestBody；"+
				"各端点上限不一致会让客户端行为随端点漂移", target.handler, target.file)
		}
	}
}

// maxChatRequestBody 必须是包级常量。它当过 openaiChat 的函数内常量，那正是
// 另外三个端点漏掉上限的原因 —— 别的文件根本引用不到它。
func TestMaxChatRequestBodyIsShared(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	text := string(src)
	if regexp.MustCompile(`(?m)^\tconst maxChatRequestBody`).MatchString(text) {
		t.Error("maxChatRequestBody 又变回函数内常量了，其他端点会引用不到")
	}
	if !regexp.MustCompile(`(?m)^const maxChatRequestBody`).MatchString(text) {
		t.Error("找不到包级的 maxChatRequestBody")
	}
}

// handlerBody 截出一个 handler 的函数体：从它的 func 行到下一个顶层 func 之前。
func handlerBody(t *testing.T, src, handler string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^func \(s \*Server\) ` + handler + `\(`).FindStringIndex(src)
	if start == nil {
		t.Fatalf("找不到 handler %s，这条测试需要跟着更新", handler)
	}
	rest := src[start[1]:]
	if next := regexp.MustCompile(`(?m)^func `).FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
