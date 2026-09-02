package web

import (
	"bytes"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 流式 intent retry 此前一条终局 disposition 都不记：成功时（server.go 的
// writeToolResponse 之前）没记 emitted_tool_calls，失败落回答路径时也没记
// retry_exhausted。非流式那侧两条都在（intent_retry→emitted_tool_calls、
// intent_retry→retry_exhausted）。
//
// 后果不是功能坏了，而是这条路径不可观测：日志里只剩一条
// `disposition=intent_retry` 然后没有下文，成功和失败长得一模一样。
// 2026-09-02 我本人就是据此误判「流式重试是死代码」——实际它跑了。
//
// 这一组守的就是这个对称性。它扫源码而不是跑请求：驱动真正的流式重试需要
// 一个会先表达工具意图、再在 required 重试里给出合法调用的上游，测试内造不出
// 来；而「两侧各自都记了终局」这条不变量本身是纯结构的，扫源码就够钉住。
// 同类做法仓库里已有先例（web_assets_test.go 扫 HTML、procwin_test.go 扫源码）。
func serverSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	return string(b)
}

// retryBlock 取出 `if body.Stream {` 之后到 buildAnswerRequest 之前那一段，
// 也就是流式 intent retry 的全部代码。
func streamRetryBlock(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "if body.Stream {")
	if start < 0 {
		t.Fatal("找不到 `if body.Stream {`：流式 intent retry 块的入口变了，这组测试需要跟着更新")
	}
	rest := src[start:]
	end := strings.Index(rest, "buildAnswerRequest(")
	if end < 0 {
		t.Fatal("找不到 buildAnswerRequest(：流式块的出口变了")
	}
	return rest[:end]
}

func TestStreamIntentRetryRecordsBothTerminalDispositions(t *testing.T) {
	block := streamRetryBlock(t, serverSource(t))

	for _, want := range []string{"emitted_tool_calls", "retry_exhausted"} {
		if !strings.Contains(block, want) {
			t.Errorf("流式 intent retry 没有记 %q：这条路径的成败在日志里无法区分，"+
				"只会留下一条 intent_retry 然后没有下文（非流式那侧两条都记了）", want)
		}
	}
}

// 成功那条必须记在 writeToolResponse 之前 —— 之后才记就等于「提前 return 掉了
// 就不记」，正是原来的形状。
func TestStreamIntentRetrySuccessIsRecordedBeforeItReturns(t *testing.T) {
	block := streamRetryBlock(t, serverSource(t))
	emitted := strings.Index(block, "emitted_tool_calls")
	write := strings.Index(block, "writeToolResponse(")
	if emitted < 0 || write < 0 {
		t.Fatalf("emitted_tool_calls=%d writeToolResponse=%d：至少一个不在流式重试块里", emitted, write)
	}
	if emitted > write {
		t.Errorf("emitted_tool_calls 记在 writeToolResponse 之后（%d > %d）："+
			"该分支写完响应就 return，记在后面等于不记", emitted, write)
	}
}

// 非流式侧的两条终局是这条不变量的参照。它们要是没了，上面两条测试就失去了
// 「对称」的对象，所以一并守住。
func TestNonStreamIntentRetryStillRecordsBothTerminalDispositions(t *testing.T) {
	src := serverSource(t)
	for _, want := range []string{
		`record("intent_retry", "emitted_tool_calls")`,
		`record("intent_retry", "retry_exhausted")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("非流式 intent retry 少了 %s", want)
		}
	}
}

// http_start 的 stream 字段读的是 URL query，而 OpenAI / Anthropic 客户端把
// stream 放在 JSON body 里，所以它对真实流量恒为 false。字段名必须写清它是
// query，且真正的 body.Stream 要在 body_parsed 里报 —— 否则看日志的人（包括我）
// 会把流式请求当成非流式，进而误判哪条分支跑过。
func TestStreamFlagIsLoggedFromTheBodyNotTheQueryString(t *testing.T) {
	src := serverSource(t)

	// 锚在 `[req-trace] id=%s ` 上：否则会命中源码注释里出现的同名字样，
	// 于是测试断言的是散文而不是日志格式串（第一版就踩了这个）。
	httpStart := regexp.MustCompile(`\[req-trace\] id=%s stage=http_start [^"]*`).FindString(src)
	if httpStart == "" {
		t.Fatal("找不到 http_start 的 req-trace 日志")
	}
	if !strings.Contains(httpStart, "stream_query=") {
		t.Errorf("http_start 记成 %q：它只能读 query（body 还没解析），"+
			"字段名不写清就会被当成 body.Stream 读", httpStart)
	}

	bodyParsed := regexp.MustCompile(`\[req-trace\] id=%s stage=body_parsed [^"]*`).FindString(src)
	if bodyParsed == "" {
		t.Fatal("找不到 body_parsed 的 req-trace 日志")
	}
	if !strings.Contains(bodyParsed, "stream=%t") {
		t.Errorf("body_parsed 没报 stream：%q —— 这是 stream 第一次真正可知的位置，"+
			"而它决定了走 stream-router 还是 router", bodyParsed)
	}
}

// record 的输出必须带上 substage，否则新加的 stream-intent-retry 终局在日志里
// 和别的 disposition 混在一起认不出来。
func TestRecordEmitsSubstageAndDisposition(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	o := newRouterOutcome("req-1", "stream-router", 9, "auto", true)
	o.observeParsed(true, 0)
	o.record("stream-intent-retry", "retry_exhausted")

	got := buf.String()
	for _, want := range []string{
		"id=req-1", "stage=stream-router", "substage=stream-intent-retry",
		"disposition=retry_exhausted", "raw_candidates=0", "tool_intent=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("router-outcome 日志缺 %q\n实际: %s", want, strings.TrimSpace(got))
		}
	}
}
