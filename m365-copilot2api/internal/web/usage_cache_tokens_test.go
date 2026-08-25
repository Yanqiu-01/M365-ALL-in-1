package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板上 messages / responses 两个端点的「缓存」列永远是 0。
//
// 原因是历史（缓存）token 只有内层 openaiChat 算得出来 —— 那里才有
// body.Messages 和 session 解析结果。内层被 innerAdapterHeader 挡掉记账后，
// 这个值没有任何通路交给外层，外层就只能写 0。
func TestInnerStatsSinkCarriesCacheTokens(t *testing.T) {
	outer := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if innerStatsSink(outer) != nil {
		t.Fatal("外部请求不该带回填槽")
	}
	if innerStatsSink(nil) != nil {
		t.Fatal("nil 请求必须安全返回 nil")
	}

	inner, sink := withInnerStats(outer)
	if sink == nil {
		t.Fatal("withInnerStats 必须返回回填槽")
	}
	got := innerStatsSink(inner)
	if got == nil {
		t.Fatal("内部请求必须能取回回填槽")
	}
	// 必须是同一个对象，否则内层写入外层读不到。
	sink.CacheTokens = 4096
	if got.CacheTokens != 4096 {
		t.Fatalf("回填槽不是同一实例：读到 %d，期望 4096", got.CacheTokens)
	}
	if innerStatsCacheTokens(sink) != 4096 {
		t.Fatalf("innerStatsCacheTokens = %d，期望 4096", innerStatsCacheTokens(sink))
	}
	if innerStatsCacheTokens(nil) != 0 {
		t.Fatal("nil 回填槽必须读成 0，而不是 panic")
	}
}

// 内层必须真的把历史 token 写进回填槽，否则通路是空的。
func TestChatFillsInnerStatsSink(t *testing.T) {
	src := readSourceFile(t, "server.go")
	if !strings.Contains(src, "sink.CacheTokens = historyTokens") {
		t.Fatal("openaiChat 没有把 historyTokens 回填给外层；缓存列会保持为 0")
	}
}

// 三个外层记账点都必须带上 CacheTokens，否则那一侧仍然是 0。
func TestOuterHandlersRecordCacheTokens(t *testing.T) {
	src := readSourceFile(t, "protocol_handlers.go")
	if n := strings.Count(src, "CacheTokens:  innerStatsCacheTokens("); n < 1 {
		t.Fatalf("流式 responses 没有记录缓存 token（命中 %d 次）", n)
	}
	if n := strings.Count(src, "CacheTokens: innerStatsCacheTokens("); n < 2 {
		t.Fatalf("messages / 非流式 responses 未同时记录缓存 token（命中 %d 次，期望 2）", n)
	}
}

// 流式 Responses 此前完全不记账：内层被标记挡掉，外层这条路径也没有
// usage.record，于是走流式的 Codex 请求在用量表里根本不存在。
func TestStreamResponsesRecordsUsage(t *testing.T) {
	src := readSourceFile(t, "protocol_handlers.go")
	body := funcBody(t, src, "streamResponsesAdapter")
	if !strings.Contains(body, "s.usage.record(") {
		t.Fatal("streamResponsesAdapter 不记录用量；流式 Codex 请求会完全不可见")
	}
	if !strings.Contains(body, `Endpoint:     "/v1/responses"`) {
		t.Fatal("流式记录没有归到 /v1/responses")
	}
	// 必须用本次请求的计时。包级 startedAt 是服务启动时间，用它会把耗时
	// 记成整个进程的运行时长 —— 而且因为同名，编译器不会报错。
	if strings.Contains(body, "time.Since(startedAt)") {
		t.Fatal("用了包级 startedAt（服务启动时间）计算单次请求耗时")
	}
	if !strings.Contains(body, "time.Since(requestStartedAt)") {
		t.Fatal("流式记录没有使用本次请求的计时")
	}
}
