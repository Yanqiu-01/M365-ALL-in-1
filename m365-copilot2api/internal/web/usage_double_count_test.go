package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 面板实测到的重复记账：一次 /v1/messages 请求产生两行用量，时间戳相同，
// 端点一个是 messages、一个是 chat/completions，token 被算两遍。
//
// 原因是 /v1/messages 与 /v1/responses 都把请求转成 OpenAI 形态后回调
// openaiChat，而 openaiChat 自己就会记一条 /v1/chat/completions，外层再按
// 真实端点记一条。
func TestInnerAdapterCallDetection(t *testing.T) {
	plain := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if isInnerAdapterCall(plain) {
		t.Fatal("一个普通外部请求不能被当成内部委派，否则它的用量会被整条丢掉")
	}

	marked := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	marked.Header.Set(innerAdapterHeader, "1")
	if !isInnerAdapterCall(marked) {
		t.Fatal("带内部委派标记的请求必须被识别")
	}

	if isInnerAdapterCall(nil) {
		t.Error("nil 请求不能 panic，也不能被当成内部委派")
	}
}

// 两个委派入口都必须在克隆出的内部请求上打标记。这里直接读源码断言，
// 因为给生产代码加一个只为测试存在的钩子，本身就是把测试成本转嫁给实现。
func TestBothAdapterEntrypointsSetInnerHeader(t *testing.T) {
	src := readSourceFile(t, "protocol_handlers.go")
	for _, fn := range []string{"runOpenAIAdapter", "streamResponsesAdapter"} {
		body := funcBody(t, src, fn)
		if !strings.Contains(body, "r2.Header.Set(innerAdapterHeader") {
			t.Fatalf("%s 没有给内部请求打 %s 标记；这一侧的用量会被重复统计",
				fn, innerAdapterHeader)
		}
	}
}

// 记账处必须真的检查这个标记，否则打了标记也没用。
func TestChatAccountingChecksInnerAdapterMarker(t *testing.T) {
	src := readSourceFile(t, "server.go")
	if !strings.Contains(src, "if isInnerAdapterCall(r) {") {
		t.Fatal("server.go 的记账路径没有检查内部委派标记")
	}
	// 必须在 usage.record 之前判断，否则一样会写进去。
	guard := strings.Index(src, "if isInnerAdapterCall(r) {")
	record := strings.Index(src, "s.usage.record(UsageRecord{\n\t\tTime:         time.Now(),\n\t\tAPIKeyPrefix: apiKey,")
	if record >= 0 && guard > record {
		t.Fatal("内部委派判断出现在 usage.record 之后，重复记账依然会发生")
	}
}

// readSourceFile 读取本包的一个源文件。用于断言那些「必须存在于某处」的
// 结构性约束，避免为测试在生产代码里开钩子。
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody 截取一个顶层函数从签名到下一个顶层 func 之间的正文。
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	needle := "func (s *Server) " + name + "("
	start := strings.Index(src, needle)
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	rest := src[start+len(needle):]
	if next := strings.Index(rest, "\nfunc "); next >= 0 {
		return rest[:next]
	}
	return rest
}
