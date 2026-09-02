package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 网关有四条流式出口。2026-09-02 实测其中三条发 usage、主回答那条不发：
// 部署目录的 debug-logs.jsonl 里，一条 /v1/chat/completions 的 SSE 正文以
// finish_reason:"stop" 收尾后直接 [DONE]，全程没有 usage；同一批里工具轮那条
// 有 usage（in=115 out=60）。
//
// 后果是客户端拿工具轮有账、拿普通文字回答没账，同一个端点两套行为。
// 靠 usage 记上下文的客户端会把这一轮当成零消耗。
//
// stream_options.include_usage 这个开关本仓库不处理（全仓无引用），
// 所以四条出口的既定约定就是「一律发」，主回答那条属于漏发而非按需不发。
func TestEveryStreamingExitEmitsUsage(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	text := string(src)

	// 每个 `data: [DONE]` 之前都必须有一个带 usage 的 chunk。按 [DONE] 切段，
	// 检查每段结尾附近是否出现 usage —— 比数 usageChunk 的个数更贴近实际行为，
	// 因为它盯的是「收尾之前有没有发」而不是「代码里写了几个」。
	segments := strings.Split(text, `data: [DONE]`)
	if len(segments) < 4 {
		t.Fatalf("只找到 %d 处 [DONE]，流式出口的形状变了，这条测试需要跟着更新", len(segments)-1)
	}

	// 末段是最后一个 [DONE] 之后的代码，没有对应出口，不检查。
	missing := 0
	for i := 0; i < len(segments)-1; i++ {
		// 只看每个出口紧邻的上文；取足够大的窗口覆盖 finishChunk + usageChunk。
		window := segments[i]
		if len(window) > 1400 {
			window = window[len(window)-1400:]
		}
		// 错误出口不要求 usage：OpenAI 协议在错误响应上同样不带 usage。
		// 判据是这一段有没有发 error chunk，而不是有没有 err 变量 —— 后者在
		// 正常出口的上文里也常出现（`if err != nil { log... return }`）。
		if strings.Contains(window, `"error": map[string]any{`) {
			continue
		}
		if !strings.Contains(window, `"usage"`) {
			missing++
			t.Errorf("第 %d 处 data: [DONE] 之前没有 usage chunk；\n出口上文尾部:\n%s",
				i+1, lastLines(window, 6))
		}
	}
	if missing > 0 {
		t.Logf("%d 处流式出口漏发 usage：客户端在这些出口上会把该轮当成零消耗", missing)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// 主回答出口的 usage 必须按本轮真正发出的正文算，不能用别的变量顶替
// （用 res.Text 就会把被过滤掉或被 eject 改写的部分也算进去）。
func TestPrimaryStreamUsageCountsTheTextItActuallySent(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	found := regexp.MustCompile(`streamCT := EstimateTokens\((\w+(?:\.\w+\(\))?)\)`).FindStringSubmatch(string(src))
	if found == nil {
		t.Fatal("找不到主流式出口的 completion token 计算")
	}
	if got := found[1]; got != "text.String()" {
		t.Errorf("completion tokens 按 %s 算；应当按 text.String()（emitText 的同源缓冲）算，"+
			"否则算进了没真正发出去的内容", got)
	}
}

// 补 usage 的时候不能另起一帧。2026-09-02 对 gateway-c27dfe0 打一次真实流式请求，
// 抓到的 SSE 尾部是这样：
//
//	data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],...}
//	data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],...,"usage":{...}}
//	data: [DONE]
//
// 一条流里 finish_reason 出现了两次。另外三条出口都是把 usage 挂在收尾帧上、
// 只发一帧（server.go:2479、:2652、tool_response.go:49），严格按 finish_reason
// 判定回答结束的客户端会把第二帧读成又一次完成。
func TestTerminalChunkCarriesFinishReasonExactlyOnce(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	segments := strings.Split(string(src), `data: [DONE]`)
	for i := 0; i < len(segments)-1; i++ {
		window := segments[i]
		if len(window) > 1400 {
			window = window[len(window)-1400:]
		}
		// 窗口不能跨过流式分支的起点。同一个函数里常常先有一个非流式的 JSON
		// 返回（它自己也带 finish_reason:"stop"），再往下才是 SSE 分支；固定
		// 长度的回看窗口会读到那条 return 里的帧，而它和本出口不在同一条流上。
		// 以 text/event-stream 这一行为界，只留流式分支自己的部分。
		if at := strings.LastIndex(window, `"text/event-stream"`); at >= 0 {
			window = window[at:]
		}
		// 只数真正被 sseRaw 发出去、且 finish_reason 有值的帧：
		//   - 注释里的字样不算，否则这条测试会去断言自己上方的说明文字；
		//   - `"finish_reason": nil` 不算。正文增量帧按 OpenAI 协议就该带
		//     finish_reason:null，那是正确形状，不是重复收尾。
		emitted := 0
		for _, line := range strings.Split(window, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			emitted += strings.Count(trimmed, `"finish_reason": "`)
		}
		if emitted > 1 {
			t.Errorf("第 %d 处 data: [DONE] 之前有 %d 帧带 finish_reason；"+
				"应当只有收尾那一帧带，usage 挂在同一帧上而不是另发一帧。\n出口上文尾部:\n%s",
				i+1, emitted, lastLines(window, 8))
		}
	}
}

// include_usage 一旦将来被支持，上面「一律发」的前提就变了。这条测试守住
// 「现在确实没有这个开关」，将来加了会红，提醒同时更新上面的判据。
func TestStreamOptionsIncludeUsageIsStillUnsupported(t *testing.T) {
	for _, name := range []string{"server.go", "openai_types.go", "protocol_compat.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "include_usage") {
			t.Errorf("%s 出现了 include_usage：流式 usage 变成按需发送后，"+
				"TestEveryStreamingExitEmitsUsage 的「一律发」判据需要改", name)
		}
	}
}
