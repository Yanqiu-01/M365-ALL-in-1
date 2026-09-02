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

// 四条流式出口的 prompt tokens 必须都按同一个变量算，而那个变量必须是 prompt。
//
// 2026-09-02 实测：主回答出口按 answerPrompt 算，于是续聊时报的是增量而不是全量。
// 同一段三轮对话，客户端发出的字节 5232 → 10453 → 15674，报回来的 prompt_tokens
// 三轮都是 1804（1.00x / 1.00x / 1.00x）；非流式和 /v1/messages 同样三轮是
// 1804 → 3098 → 4391（1.00x / 1.72x / 2.43x），正常累积。
//
// 后果不是数字偏一点。Claude CLI / Codex 每轮重发全部历史，靠 usage 估自己的窗口
// 占用；按增量算的话客户端永远以为上下文是空的，不触发压缩，直到窗口爆掉。
//
// answerPrompt 在续聊时被换成增量（server.go:1827），之后又被换成 answerReq.Text
// （:2022），两次都不再是客户端那段对话。prompt（:1758）才是 body.Messages 压平后
// 的全量，server.go:1818 那段注释明确说全量 prompt 保留下来就是给 token accounting
// 用的。
func TestAllStreamingExitsCountPromptTokensFromTheFullConversation(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	// 抓每个 prompt_tokens 字段所用的变量名：先找赋给它的标识符，再回溯该标识符是
	// 从哪个变量 EstimateTokens 出来的。直接找 EstimateTokens 会连非 usage 的用法
	// 一起抓进来（例如 context budget 的估算）。
	text := string(src)
	assign := regexp.MustCompile(`"prompt_tokens":\s*(\w+)`)
	matches := assign.FindAllStringSubmatch(text, -1)
	if len(matches) < 4 {
		t.Fatalf("只找到 %d 处 prompt_tokens，流式出口的形状变了，这条测试需要跟着更新",
			len(matches))
	}
	for _, m := range matches {
		variable := m[1]
		// 找该变量的赋值：`<v> := EstimateTokens(<src>)`
		def := regexp.MustCompile(regexp.QuoteMeta(variable) +
			`\s*:=\s*EstimateTokens\((\w+(?:\.\w+\(\))?)\)`).FindStringSubmatch(text)
		if def == nil {
			// 该 usage 的 prompt tokens 不是就地 EstimateTokens 出来的（例如从内层
			// 统计里带出来的），不在这条测试的判据内。
			continue
		}
		if got := def[1]; got != "prompt" {
			t.Errorf("prompt_tokens 用的 %s 是按 %s 算的；应当按 prompt（body.Messages 压平后的"+
				"全量）算，否则续聊时报的是增量，客户端的上下文计数会一直停在第一轮的值",
				variable, got)
		}
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
