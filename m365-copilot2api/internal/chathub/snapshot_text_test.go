package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实复现形状。上游先把附件链接发成残缺的 `](url)`，随后重写成完整的
// `[hello.py](url)`。后者不是前者的前缀，旧实现在 client.go 里把整个快照丢掉
// （生产日志 6439 次 "[emitSnapshot] skip"），最终 content 就是开头被吃掉的
// `](url)` —— 很容易被误判成模型拒答或输出截断。
const (
	attachmentURL     = "https://kr-prod.asyncgw.teams.microsoft.com/v1/objects/0-ea-d7-9f2c4b1a8e5d/views/original/hello.py"
	brokenAttachment  = "已创建文件。](" + attachmentURL + ")"
	rewrittenSnapshot = "已创建文件。[hello.py](" + attachmentURL + ")"
)

// replayUpdateFrames 把一串 SignalR type=1 target=update 帧喂给 textStream，
// 走的是 client.go 生产路径上同一个 updateSnapshots + pushSnapshot 组合。
// 返回：客户端按顺序收到的增量，以及完成帧收敛后的最终文本。
func replayUpdateFrames(t *testing.T, frames []string) ([]string, string) {
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
			for _, snapshot := range updateSnapshots(argument, messages) {
				if err := stream.pushSnapshot(snapshot); err != nil {
					t.Fatalf("push snapshot: %v", err)
				}
			}
		}
	}
	if err := stream.finalize(); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return emitted, stream.finalText()
}

// assertNoDuplicateText 证明「不复读」：把客户端收到的增量顺序拼接后，任何
// 一段足够长的增量都不得在结果里出现两次。
func assertNoDuplicateText(t *testing.T, emitted []string) {
	t.Helper()
	joined := strings.Join(emitted, "")
	for i, delta := range emitted {
		if len(delta) < 8 {
			continue
		}
		if first := strings.Index(joined, delta); first != strings.LastIndex(joined, delta) {
			t.Errorf("增量[%d]=%q 在已发送文本里出现多次（复读）：%q", i, delta, joined)
		}
	}
}

// a) 非前缀重写后，最终文本必须等于上游最后一个完整快照。
func TestNonPrefixRewriteFinalTextMatchesLastUpstreamSnapshot(t *testing.T) {
	emitted, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(brokenAttachment) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(rewrittenSnapshot) + `}]}`,
	})
	if final != rewrittenSnapshot {
		t.Fatalf("最终文本=%q\n期望上游最后的完整快照=%q\n（旧实现丢弃重写后的快照，只留下残缺的 `](url)`）", final, rewrittenSnapshot)
	}
	if !strings.Contains(final, "[hello.py](") {
		t.Errorf("附件链接开头的 `[hello.py` 仍被吃掉：%q", final)
	}
	assertNoDuplicateText(t, emitted)
}

// 同一个缺陷的 messages[].text 形态：author=bot 的快照被重写。
func TestNonPrefixRewriteThroughBotMessageSnapshot(t *testing.T) {
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(brokenAttachment) + `}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(rewrittenSnapshot) + `}]}]}`,
	})
	if final != rewrittenSnapshot {
		t.Fatalf("最终文本=%q want %q", final, rewrittenSnapshot)
	}
}

// claude-sonnet-reasoning 上观察到的同类残留：引用标记被后补进正文。
func TestCitationRewriteKeepsFullText(t *testing.T) {
	partial := "根据检索结果，答案是 42。<cite>turn5file5</cite>该结论已复核，且与文档一致。"
	rewritten := "根据检索结果，答案是 42。[1]该结论已复核，且与文档一致。<cite>turn5file5</cite>"
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(partial) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(rewritten) + `}]}`,
	})
	if final != rewritten {
		t.Fatalf("最终文本=%q want %q", final, rewritten)
	}
}

// b) 正常前缀增量不得产生任何重复文本（防复读回归）。
func TestPrefixIncrementsProduceNoDuplicateText(t *testing.T) {
	full := "第一段内容足够长以越过抖动窗口，因此会被立即下发给客户端。第二段继续追加。第三段收尾。"
	runes := []rune(full)
	var frames []string
	// 逐步增长的快照，每一帧都是前一帧的前缀扩展。
	for cut := 40; cut <= len(runes); cut += 10 {
		frames = append(frames, `{"type":1,"target":"update","arguments":[{"writeAtCursor":`+mustQuote(string(runes[:cut]))+`}]}`)
	}
	frames = append(frames, `{"type":1,"target":"update","arguments":[{"writeAtCursor":`+mustQuote(full)+`}]}`)

	emitted, final := replayUpdateFrames(t, frames)
	if final != full {
		t.Fatalf("最终文本=%q want %q", final, full)
	}
	if joined := strings.Join(emitted, ""); joined != full {
		t.Fatalf("已发送增量拼接=%q want %q（出现丢失或复读）", joined, full)
	}
	assertNoDuplicateText(t, emitted)
}

// 完全相同的快照重复到达时不得重复下发。
func TestRepeatedIdenticalSnapshotEmitsOnce(t *testing.T) {
	snapshot := "这是一段长度足以越过抖动窗口的正文，用来验证重复快照不会被重复下发给客户端。"
	frame := `{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(snapshot) + `}]}`
	emitted, final := replayUpdateFrames(t, []string{frame, frame, frame})
	if final != snapshot {
		t.Fatalf("最终文本=%q want %q", final, snapshot)
	}
	if joined := strings.Join(emitted, ""); joined != snapshot {
		t.Fatalf("已发送=%q want %q（重复快照造成复读）", joined, snapshot)
	}
}

// 迟到的更短快照不得截断已知内容。
func TestStaleShorterSnapshotDoesNotTruncateFinalText(t *testing.T) {
	full := "这是一段完整的回复正文，长度足以越过抖动窗口，后面还有更多内容需要保留下来。"
	runes := []rune(full)
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(full) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(string(runes[:20])) + `}]}`,
	})
	if final != full {
		t.Fatalf("迟到的更短快照截断了最终文本：%q want %q", final, full)
	}
}

// c) 上游内容过滤定型句必须完整原样返回。这类响应很短（25-28 字），会落在
// 抖动窗口内被按住不发；finalize 必须把它补齐，否则用户看到空回复。
func TestUpstreamContentFilterSentenceReturnedVerbatim(t *testing.T) {
	// 这句由 Microsoft 上游内容过滤器返回，本地不做任何判定、重写或重试。
	sentence := "很抱歉，我似乎无法对此做出响应。"
	emitted, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(sentence) + `}]}`,
	})
	if final != sentence {
		t.Fatalf("最终文本=%q want 原样返回 %q", final, sentence)
	}
	if joined := strings.Join(emitted, ""); joined != sentence {
		t.Fatalf("流式已发送=%q want %q（短回复被吞掉或复读）", joined, sentence)
	}
}

// 内容过滤定型句以 messages[].text 形态到达时同样原样返回。
func TestShortBotMessageReturnedVerbatim(t *testing.T) {
	sentence := "很抱歉，我无法帮助处理该请求。"
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(sentence) + `}]}]}`,
	})
	if final != sentence {
		t.Fatalf("最终文本=%q want %q", final, sentence)
	}
}

// d) 流式策略验证：非前缀重写时已发送内容不撤回、不复读，新增后缀照常送达，
// 最终文本由完整快照校正。
func TestStreamingRewriteDeliversNewSuffixWithoutRepeating(t *testing.T) {
	prefix := "正在处理你的请求，这段前缀足够长以越过抖动窗口，会先发给客户端。"
	// 上游重写了尾部标点，同时在后面追加了新内容。
	firstSnapshot := prefix + "结果如下：](" + attachmentURL + ")"
	secondSnapshot := prefix + "结果如下：[hello.py](" + attachmentURL + ")\n文件已写入工作目录。"

	emitted, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(prefix) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(firstSnapshot) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(secondSnapshot) + `}]}`,
	})

	if final != secondSnapshot {
		t.Fatalf("最终文本=%q want 上游最后的完整快照 %q", final, secondSnapshot)
	}
	assertNoDuplicateText(t, emitted)

	joined := strings.Join(emitted, "")
	// 重写之后新长出来的内容必须真的送到客户端，不能只在最终文本里出现。
	if !strings.Contains(joined, "文件已写入工作目录。") {
		t.Errorf("重写后新增的内容没有下发给流式客户端：%q", joined)
	}
	// 已发送内容必须是最终文本的前缀语义上的子序列拼接，不能凭空多出一段
	// 已经发过的文本。
	if strings.Count(joined, attachmentURL) > 1 {
		t.Errorf("附件 URL 被重复下发：%q", joined)
	}
}

// 流式场景：重写只改动了已发送内容的尾部而没有新增内容时，不得追补任何东西
// （否则就是复读），但最终文本仍需被校正。
func TestStreamingRewriteWithoutGrowthEmitsNothingExtra(t *testing.T) {
	prefix := "这段前缀足够长以越过抖动窗口，会被立即下发给客户端，随后上游重写了它的开头。"
	rewritten := "【已完成】" + prefix[len(prefix)/2:]

	emitted, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(prefix) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(rewritten+"补充说明。") + `}]}`,
	})

	if final != rewritten+"补充说明。" {
		t.Fatalf("最终文本=%q want %q", final, rewritten+"补充说明。")
	}
	assertNoDuplicateText(t, emitted)
}

// undelivered 的核心安全性质：返回值恒为 canon 的后缀，且拼接后不重复任何
// 已发送片段。这里用表格覆盖各类分叉形状。
func TestUndeliveredNeverRepeatsDeliveredText(t *testing.T) {
	cases := []struct {
		name      string
		delivered string
		canon     string
	}{
		{"prefix growth", "abcdefghijklmnop", "abcdefghijklmnopqrstuvwxyz"},
		{"identical", "abcdefghijklmnop", "abcdefghijklmnop"},
		{"delivered longer", "abcdefghijklmnopqrst", "abcdefghijklmnop"},
		{"leading rewrite", brokenAttachment, rewrittenSnapshot},
		{"leading rewrite with growth", brokenAttachment, rewrittenSnapshot + "\n后续内容"},
		{"unrelated", "完全不相关的已发送内容长度也够", "上游给出的完整快照与之毫无重合"},
		{"empty delivered", "", rewrittenSnapshot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := snapshotTracker{canon: tc.canon}
			suffix := tracker.undelivered(tc.delivered)
			if suffix == "" {
				return
			}
			if !strings.HasSuffix(tc.canon, suffix) {
				t.Fatalf("追补内容 %q 不是上游快照的后缀", suffix)
			}
			combined := tc.delivered + suffix
			if strings.Contains(suffix, tc.delivered) && tc.delivered != "" {
				t.Fatalf("追补内容重复了已发送文本：delivered=%q suffix=%q", tc.delivered, suffix)
			}
			if !strings.HasSuffix(combined, tc.canon) && !strings.HasSuffix(tc.canon, suffix) {
				t.Fatalf("拼接结果与上游快照结构不一致：%q", combined)
			}
		})
	}
}

// canonicalText 必须保持「正常路径原样、分叉路径采用完整快照」。
func TestCanonicalTextPrefersUpstreamSnapshotOnlyWhenDiverged(t *testing.T) {
	tracker := snapshotTracker{canon: "abcdef"}
	if got := tracker.canonicalText("abcdefghi"); got != "abcdefghi" {
		t.Errorf("已发送内容涵盖快照时应原样保留，got %q", got)
	}
	if got := tracker.canonicalText("xbcdef"); got != "abcdef" {
		t.Errorf("分叉时应采用上游完整快照，got %q", got)
	}
	empty := snapshotTracker{}
	if got := empty.canonicalText("only-deltas"); got != "only-deltas" {
		t.Errorf("没有任何快照时不得改写已发送内容，got %q", got)
	}
}

// updateSnapshots 必须与拆分前的内联实现取材一致：工具/进度帧不产出正文快照。
func TestUpdateSnapshotsSkipsToolAndProgressFrames(t *testing.T) {
	arg := map[string]any{
		"writeAtCursor": "不应作为正文",
		"messages":      []any{map[string]any{"text": "搜索中", "messageType": "Progress"}},
	}
	if got := updateSnapshots(arg, arg["messages"].([]any)); len(got) != 0 {
		t.Fatalf("工具/进度帧产出了正文快照：%#v", got)
	}

	arg = map[string]any{
		"writeAtCursor": "光标快照",
		"messages":      []any{map[string]any{"author": "bot", "text": "机器人快照"}},
	}
	got := updateSnapshots(arg, arg["messages"].([]any))
	want := []string{"光标快照", "机器人快照"}
	if len(got) != len(want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("snapshot[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func mustQuote(text string) string {
	b, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(b)
}
