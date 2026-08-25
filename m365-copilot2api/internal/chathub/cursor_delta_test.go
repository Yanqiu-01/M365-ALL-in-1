package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

// M365_TRACE=1 抓到的真实上游帧序（请求：让模型用 write_file 写 hello.py）。
// 三帧都带 "streamingMode":"Delta"，是增量而不是快照。
const (
	cursorDeltaURL = "https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/0-ea-d11-cce1b6ad1f33bf3fa745ca1809fcc4b0/views/original/hello.py"
	// 生产实测的三段增量。注意前导 `[` 不在文本流里：它只出现在更早一个
	// {"cursor":{"j":"$['998e9128-…'].adaptiveCards[0].body…"}} 帧的写入路径上。
	// 因此正确的拼接结果是 `hello.py](url)`，测试不为了凑出 `[` 补任何内容。
	cursorDeltaOne   = "hello"
	cursorDeltaTwo   = ".py"
	cursorDeltaThree = "](" + cursorDeltaURL + ")"
	// 三段按到达顺序拼接后的完整正文。
	cursorDeltaJoined = cursorDeltaOne + cursorDeltaTwo + cursorDeltaThree
)

// deltaFrame 构造一个 streamingMode=Delta 的 writeAtCursor 帧，字段布局与生产
// 帧一致（含 nonce，确保多出来的同级字段不影响判定）。
func deltaFrame(text string) string {
	return `{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(text) +
		`,"streamingMode":"Delta","nonce":"3uYP9lKlUikOTt0YP17tuA=="}]}`
}

// replayUpdateTexts 走生产路径：updateTexts + pushUpdateText，即 client.go 里
// 同一个组合。返回客户端按顺序收到的增量与完成帧收敛后的最终文本。
func replayUpdateTexts(t *testing.T, frames []string) ([]string, string) {
	t.Helper()
	var emitted []string
	stream := newTextStream(func(delta string) error {
		emitted = append(emitted, delta)
		return nil
	})
	for _, raw := range frames {
		var frame map[string]any
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		arguments, _ := frame["arguments"].([]any)
		for _, rawArgument := range arguments {
			argument, ok := rawArgument.(map[string]any)
			if !ok {
				continue
			}
			messages, _ := argument["messages"].([]any)
			for _, item := range updateTexts(argument, messages) {
				if err := stream.pushUpdateText(item); err != nil {
					t.Fatalf("push update text: %v", err)
				}
			}
		}
	}
	if err := stream.finalize(); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return emitted, stream.finalText()
}

// a) 生产实测的三个 Delta 帧必须拼接，不能互相替换。
//
// 旧行为：三段都被当成完整快照，后一段替换前一段，最终只剩 `](url)`
// （日志原文 [debug] res.Text bytes=123 content="](https://…/hello.py)"）。
func TestCursorDeltaFramesConcatenateInsteadOfReplacing(t *testing.T) {
	emitted, final := replayUpdateTexts(t, []string{
		deltaFrame(cursorDeltaOne),
		deltaFrame(cursorDeltaTwo),
		deltaFrame(cursorDeltaThree),
	})

	if final != cursorDeltaJoined {
		t.Fatalf("最终文本=%q\nwant 三段增量的拼接=%q", final, cursorDeltaJoined)
	}
	// 每一段都不能丢。`hello` 与 `.py` 正是旧实现丢掉的两段。
	for _, part := range []string{cursorDeltaOne, cursorDeltaTwo, cursorDeltaThree} {
		if !strings.Contains(final, part) {
			t.Errorf("增量片段 %q 被丢弃：%q", part, final)
		}
	}
	if !strings.Contains(final, "hello.py](") {
		t.Errorf("文件名与链接没有拼上：%q", final)
	}
	// 前导 `[` 不在文本流里，不得由本地补出来。
	if strings.Contains(final, "[hello.py") {
		t.Errorf("凭空补出了不存在于文本流的前导 `[`：%q", final)
	}
	assertNoDuplicateText(t, emitted)
	if joined := strings.Join(emitted, ""); joined != final {
		t.Errorf("流式已发送=%q 与最终文本=%q 不一致", joined, final)
	}
}

// 增量重复出现同一串文本是合法的，不得被去重吃掉。
func TestRepeatedIdenticalCursorDeltasAllRetained(t *testing.T) {
	_, final := replayUpdateTexts(t, []string{
		deltaFrame("ha"), deltaFrame("ha"), deltaFrame("ha"),
	})
	if final != "hahaha" {
		t.Fatalf("最终文本=%q want %q（相同增量被误判成重复快照）", final, "hahaha")
	}
}

// 缺省/其他 streamingMode 保持既有的完整快照语义：后一个快照替换前一个，
// 不得被当成增量拼接。
func TestWriteAtCursorWithoutDeltaModeKeepsSnapshotSemantics(t *testing.T) {
	first := "这是一段足够长的正文快照，用来越过抖动窗口并验证快照语义没有被改成增量。"
	second := first + "后续追加的内容。"
	_, final := replayUpdateTexts(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(first) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(second) + `}]}`,
	})
	if final != second {
		t.Fatalf("最终文本=%q want %q", final, second)
	}
	if strings.Count(final, "这是一段足够长的正文快照") != 1 {
		t.Errorf("快照被当成增量拼接，正文重复：%q", final)
	}
}

// b) 完整快照与 Delta 增量混合到达：既不丢内容也不复读。
//
// 这是最容易引入复读的形状：上游先广播一条完整消息，随后继续用光标增量往同一段
// 正文后面写。
func TestSnapshotThenCursorDeltasNeitherLoseNorDuplicate(t *testing.T) {
	snapshot := "已创建文件。这段完整快照足够长，会立即下发给客户端。"
	emitted, final := replayUpdateTexts(t, []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(snapshot) + `}]}]}`,
		deltaFrame(cursorDeltaOne),
		deltaFrame(cursorDeltaTwo),
		deltaFrame(cursorDeltaThree),
	})

	want := snapshot + cursorDeltaJoined
	if final != want {
		t.Fatalf("最终文本=%q\nwant 快照后接三段增量=%q", final, want)
	}
	if strings.Count(final, snapshot) != 1 {
		t.Errorf("完整快照在最终文本里出现多次（复读）：%q", final)
	}
	if strings.Count(final, cursorDeltaURL) != 1 {
		t.Errorf("附件 URL 出现多次（复读）：%q", final)
	}
	assertNoDuplicateText(t, emitted)
	if joined := strings.Join(emitted, ""); joined != final {
		t.Errorf("流式已发送=%q 与最终文本=%q 不一致", joined, final)
	}
}

// 反向到达顺序：先来 Delta 增量，随后上游把整条消息作为完整快照广播一次。
// 快照涵盖了增量已发出的内容，因此不得再发一遍。
func TestCursorDeltasThenFullSnapshotDoesNotDuplicate(t *testing.T) {
	emitted, final := replayUpdateTexts(t, []string{
		deltaFrame(cursorDeltaOne),
		deltaFrame(cursorDeltaTwo),
		deltaFrame(cursorDeltaThree),
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(cursorDeltaJoined) + `}]}]}`,
	})
	if final != cursorDeltaJoined {
		t.Fatalf("最终文本=%q want %q", final, cursorDeltaJoined)
	}
	if strings.Count(final, cursorDeltaURL) != 1 {
		t.Errorf("附件 URL 被重复：%q", final)
	}
	assertNoDuplicateText(t, emitted)
}

// 快照在增量之间到达（上游边写边广播）：仍然不丢不重。
//
// 用真实帧序断言不重时不能数 "hello" 的出现次数 —— 它在正确结果里本来就出现两次
// （文件名一次、URL 路径 .../original/hello.py 一次）。因此这里断言整体等值、
// 流式与最终一致，并另用带唯一标记的变体做严格计数。
func TestSnapshotInterleavedWithCursorDeltas(t *testing.T) {
	emitted, final := replayUpdateTexts(t, []string{
		deltaFrame(cursorDeltaOne),
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(cursorDeltaOne) + `}]}]}`,
		deltaFrame(cursorDeltaTwo),
		deltaFrame(cursorDeltaThree),
	})
	if final != cursorDeltaJoined {
		t.Fatalf("最终文本=%q want %q", final, cursorDeltaJoined)
	}
	if strings.Count(final, cursorDeltaURL) != 1 {
		t.Errorf("附件 URL 出现多次（复读）：%q", final)
	}
	assertNoDuplicateText(t, emitted)
	if joined := strings.Join(emitted, ""); joined != final {
		t.Errorf("流式已发送=%q 与最终文本=%q 不一致", joined, final)
	}
}

// 与上一个用例同形状，但每段带唯一标记，可以对每一段做严格的出现次数断言。
// 交错重复广播同一段快照是最容易触发复读的形状。
func TestSnapshotInterleavedWithCursorDeltasUniqueTokens(t *testing.T) {
	first := "【壹】这段快照足够长会立即下发给客户端，"
	second := "【贰】随后由光标增量继续写入，"
	third := "【叁】最后一段增量收尾。"

	emitted, final := replayUpdateTexts(t, []string{
		deltaFrame(first),
		// 上游把已写入的正文作为完整消息广播一次。
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(first) + `}]}]}`,
		deltaFrame(second),
		// 再广播一次，这次涵盖前两段。
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(first+second) + `}]}]}`,
		deltaFrame(third),
	})

	want := first + second + third
	if final != want {
		t.Fatalf("最终文本=%q\nwant %q", final, want)
	}
	for _, part := range []string{first, second, third} {
		if got := strings.Count(final, part); got != 1 {
			t.Errorf("片段 %q 在最终文本里出现 %d 次，want 1（丢内容或复读）：%q", part, got, final)
		}
	}
	assertNoDuplicateText(t, emitted)
	if joined := strings.Join(emitted, ""); joined != final {
		t.Errorf("流式已发送=%q 与最终文本=%q 不一致", joined, final)
	}
}

// Delta 之后到达纯引用标记快照时，组 K 的行为必须继续成立：标记不覆盖正文。
func TestCiteOnlySnapshotAfterCursorDeltasKeepsBody(t *testing.T) {
	_, final := replayUpdateTexts(t, []string{
		deltaFrame(cursorDeltaOne),
		deltaFrame(cursorDeltaTwo),
		deltaFrame(cursorDeltaThree),
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(citeOnlySnapshot) + `}]}]}`,
	})
	if final != cursorDeltaJoined {
		t.Fatalf("引用标记快照覆盖了增量正文：%q want %q", final, cursorDeltaJoined)
	}
	if strings.ContainsAny(final, citeMarkerRunes) {
		t.Errorf("最终文本残留私用区标记：%q", final)
	}
}

// 工具/进度帧即使带 Delta 标记也不产出正文，取材范围不因本次改动放宽。
func TestDeltaCursorStillSkippedOnToolFrames(t *testing.T) {
	arg := map[string]any{
		"writeAtCursor": "不应作为正文",
		"streamingMode": "Delta",
		"messages":      []any{map[string]any{"text": "搜索中", "messageType": "Progress"}},
	}
	if got := updateTexts(arg, arg["messages"].([]any)); len(got) != 0 {
		t.Fatalf("工具/进度帧产出了正文：%#v", got)
	}
}

// updateTexts 必须把语义如实带出来：writeAtCursor 随 streamingMode 变化，
// messages[].text 恒为完整快照。
func TestUpdateTextsCarriesStreamingSemantics(t *testing.T) {
	arg := map[string]any{
		"writeAtCursor": "光标增量",
		"streamingMode": "Delta",
		"messages":      []any{map[string]any{"author": "bot", "text": "机器人快照"}},
	}
	got := updateTexts(arg, arg["messages"].([]any))
	want := []updateText{{text: "光标增量", delta: true}, {text: "机器人快照"}}
	if len(got) != len(want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("texts[%d]=%#v want %#v", i, got[i], want[i])
		}
	}

	// 缺省 streamingMode 是快照语义。
	arg = map[string]any{"writeAtCursor": "完整快照"}
	got = updateTexts(arg, nil)
	if len(got) != 1 || got[0].delta {
		t.Fatalf("缺省 streamingMode 被当成增量：%#v", got)
	}

	// 其他取值同样保持快照语义。请求侧的 ConciseWithPadding 不是增量标识。
	arg = map[string]any{"writeAtCursor": "完整快照", "streamingMode": "ConciseWithPadding"}
	got = updateTexts(arg, nil)
	if len(got) != 1 || got[0].delta {
		t.Fatalf("ConciseWithPadding 被当成增量：%#v", got)
	}
}

// streamingMode 判定大小写与空白不敏感。
func TestIsDeltaStreamingModeCaseAndSpaceInsensitive(t *testing.T) {
	for _, mode := range []string{"Delta", "delta", "DELTA", " Delta "} {
		if !isDeltaStreamingMode(map[string]any{"streamingMode": mode}) {
			t.Errorf("streamingMode=%q 未被识别为增量", mode)
		}
	}
	for _, mode := range []string{"", "Final", "ConciseWithPadding", "DeltaFinal"} {
		if isDeltaStreamingMode(map[string]any{"streamingMode": mode}) {
			t.Errorf("streamingMode=%q 被误判为增量", mode)
		}
	}
	if isDeltaStreamingMode(map[string]any{}) {
		t.Error("缺省 streamingMode 被误判为增量")
	}
}

// 短增量累积出的短回复（含上游内容过滤定型句）必须原样返回：抖动窗口按住的
// 内容由 finalize 补齐。
func TestContentFilterSentenceArrivingAsCursorDeltasVerbatim(t *testing.T) {
	sentence := "很抱歉，我似乎无法对此做出响应。"
	runes := []rune(sentence)
	var frames []string
	for _, r := range runes {
		frames = append(frames, deltaFrame(string(r)))
	}
	emitted, final := replayUpdateTexts(t, frames)
	if final != sentence {
		t.Fatalf("最终文本=%q want 原样返回 %q", final, sentence)
	}
	if joined := strings.Join(emitted, ""); joined != sentence {
		t.Fatalf("流式已发送=%q want %q", joined, sentence)
	}
}

// cursorAccumulator 的核心性质：返回值恒以本次增量结尾，且只由上游字节组成。
func TestCursorAccumulatorOnlyAppendsUpstreamBytes(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		delta string
		want  string
	}{
		{"首个增量无基准", "", "hello", "hello"},
		{"基准为已收到的完整快照", "已创建文件。", "hello", "已创建文件。hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var acc cursorAccumulator
			if got := acc.push(tc.base, tc.delta); got != tc.want {
				t.Fatalf("push=%q want %q", got, tc.want)
			}
			if acc.deltas != 1 {
				t.Errorf("deltas=%d want 1", acc.deltas)
			}
		})
	}

	// 连续累积：每一步都以本次增量结尾，且此前所有增量按序保留。
	var acc cursorAccumulator
	base := ""
	for _, delta := range []string{cursorDeltaOne, cursorDeltaTwo, cursorDeltaThree} {
		got := acc.push(base, delta)
		if !strings.HasSuffix(got, delta) {
			t.Fatalf("累积结果 %q 未以本次增量 %q 结尾", got, delta)
		}
		base = got
	}
	if acc.text != cursorDeltaJoined {
		t.Fatalf("累积结果=%q want %q", acc.text, cursorDeltaJoined)
	}
}
