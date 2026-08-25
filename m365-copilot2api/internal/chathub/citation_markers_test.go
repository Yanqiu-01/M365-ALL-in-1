package chathub

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// 生产实测的最后一个快照：25 字节，纯引用标记，没有任何正文。
// 日志原文 [debug] res.Text bytes=25 content="\ue200cite\ue202turn12file12\ue201"
const citeOnlySnapshot = "\ue200cite\ue202turn12file12\ue201"

// 上游在被标记快照覆盖之前发出的真实正文（含附件 markdown 链接）。
const attachmentBody = "已创建文件 hello.py。[hello.py](" + attachmentURL + ")"

func TestCiteOnlySnapshotByteShape(t *testing.T) {
	if len(citeOnlySnapshot) != 25 {
		t.Fatalf("引用标记快照=%d 字节，与生产日志的 25 字节不符：%q", len(citeOnlySnapshot), citeOnlySnapshot)
	}
	for _, r := range []rune{citeMarkerStart, citeMarkerEnd, citeMarkerSep} {
		if !strings.ContainsRune(citeOnlySnapshot, r) {
			t.Errorf("快照缺少私用区分隔符 %U", r)
		}
	}
	if hasBodyContent(citeOnlySnapshot) {
		t.Errorf("纯引用标记被判定成正文：%q", citeOnlySnapshot)
	}
	if !isCiteMarkerOnly(citeOnlySnapshot) {
		t.Errorf("isCiteMarkerOnly 未识别纯引用标记快照：%q", citeOnlySnapshot)
	}
}

// a) 用真实字节序列作为最后一个快照时，最终文本仍是先前那段附件正文。
func TestCiteOnlyFinalSnapshotDoesNotReplaceAttachmentBody(t *testing.T) {
	if len(attachmentBody) < 122 {
		t.Fatalf("对照正文只有 %d 字节，应不小于生产实测的 122 字节", len(attachmentBody))
	}
	emitted, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(attachmentBody) + `}]}`,
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(citeOnlySnapshot) + `}]}`,
	})
	if final == citeOnlySnapshot || final == stripCiteMarkers(citeOnlySnapshot) {
		t.Fatalf("最终文本被引用标记快照覆盖：%q", final)
	}
	if final != attachmentBody {
		t.Fatalf("最终文本=%q\nwant 附件正文=%q", final, attachmentBody)
	}
	// c) 附件链接必须带文件名，不能退化成 `](url)`。
	if !strings.Contains(final, "[hello.py]("+attachmentURL+")") {
		t.Errorf("附件 markdown 链接不完整：%q", final)
	}
	if strings.ContainsAny(final, citeMarkerRunes) {
		t.Errorf("最终文本仍残留私用区标记：%q", final)
	}
	if joined := strings.Join(emitted, ""); joined != final {
		t.Errorf("流式已发送=%q 与最终文本=%q 不一致", joined, final)
	}
}

// messages[].text 形态的同一个缺陷。
func TestCiteOnlyBotMessageSnapshotDoesNotReplaceBody(t *testing.T) {
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(attachmentBody) + `}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":` + mustQuote(citeOnlySnapshot) + `}]}]}`,
	})
	if final != attachmentBody {
		t.Fatalf("最终文本=%q want %q", final, attachmentBody)
	}
}

// 被跳过的引用标记快照仍要记入统计，保证可观测。
func TestCiteOnlySnapshotCounted(t *testing.T) {
	stream := newTextStream(nil)
	if err := stream.pushSnapshot(attachmentBody); err != nil {
		t.Fatal(err)
	}
	if err := stream.pushSnapshot(citeOnlySnapshot); err != nil {
		t.Fatal(err)
	}
	if stream.tracker.citeOnly != 1 {
		t.Errorf("cite_only 计数=%d want 1", stream.tracker.citeOnly)
	}
	if stream.tracker.rewrites != 0 {
		t.Errorf("引用标记快照不应被记成正文重写，rewrites=%d", stream.tracker.rewrites)
	}
	if stream.tracker.canon != attachmentBody {
		t.Errorf("canon 被引用标记覆盖：%q", stream.tracker.canon)
	}
}

// b) 内嵌引用标记要剥离，正常内容（含 markdown 链接、代码块）完好。
func TestEmbeddedCiteMarkersStrippedWithoutDamagingContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"句中锚点",
			"这是结论\ue200cite\ue202turn3file1\ue201。后续说明。",
			"这是结论。后续说明。",
		},
		{
			"锚点前有空格且后接标点",
			"结论如上 \ue200cite\ue202turn3file1\ue201。",
			"结论如上。",
		},
		{
			"锚点前有空格且后接文字",
			"see also \ue200cite\ue202turn3file1\ue201 next line",
			"see also next line",
		},
		{
			"markdown 链接不受影响",
			"结果：[hello.py](" + attachmentURL + ")\ue200cite\ue202turn12file12\ue201",
			"结果：[hello.py](" + attachmentURL + ")",
		},
		{
			"代码块缩进不被破坏",
			"示例\ue200cite\ue202turn1file1\ue201：\n```python\ndef f():\n    return 1\n```\n",
			"示例：\n```python\ndef f():\n    return 1\n```\n",
		},
		{
			"缩进行首的锚点保留缩进",
			"步骤：\n    \ue200cite\ue202turn1file1\ue201return 1\n",
			"步骤：\n    return 1\n",
		},
		{
			"多个锚点",
			"甲\ue200cite\ue202a\ue201乙\ue200cite\ue202b\ue201丙",
			"甲乙丙",
		},
		{
			"无标记文本逐字节原样",
			"普通回答，含 `code` 与 [链接](https://example.com/a_b)。",
			"普通回答，含 `code` 与 [链接](https://example.com/a_b)。",
		},
		{
			"杂散分隔符只丢自身",
			"正文A\ue202正文B",
			"正文A正文B",
		},
		{
			"跨行的起始符按杂散处理，正文保留",
			"正文\ue200第一行\n第二行",
			"正文第一行\n第二行",
		},
		{
			"过长的未闭合段按杂散处理，正文保留",
			"正文\ue200" + strings.Repeat("x", citeMarkerMaxSpanBytes+8),
			"正文" + strings.Repeat("x", citeMarkerMaxSpanBytes+8),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCiteMarkers(tc.in); got != tc.want {
				t.Errorf("stripCiteMarkers=%q\nwant %q", got, tc.want)
			}
		})
	}
}

// 交付文本里绝不允许残留私用区标记。
func TestDeliveredTextNeverContainsPrivateUseMarkers(t *testing.T) {
	body := "根据文档，答案是 42。\ue200cite\ue202turn7file3\ue201 详见附件 [a.md](" + attachmentURL + ")\ue200cite\ue202turn7file4\ue201"
	_, final := replayUpdateFrames(t, []string{
		`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(body) + `}]}`,
	})
	if strings.ContainsAny(final, citeMarkerRunes) {
		t.Fatalf("最终文本残留私用区标记：%q", final)
	}
	if !strings.Contains(final, "[a.md]("+attachmentURL+")") {
		t.Errorf("markdown 链接被破坏：%q", final)
	}
	if !strings.Contains(final, "答案是 42。") {
		t.Errorf("正文被破坏：%q", final)
	}
}

// c) 上游内容过滤定型句必须逐字节原样返回，剥离逻辑不得改动它。
func TestContentFilterSentenceUntouchedByCiteStripping(t *testing.T) {
	for _, sentence := range []string{
		"很抱歉，我似乎无法对此做出响应。",
		"很抱歉，我无法帮助处理该请求。",
		"I'm sorry, but I can't help with that.",
	} {
		if got := stripCiteMarkers(sentence); got != sentence {
			t.Errorf("剥离逻辑改动了内容过滤定型句：got %q want %q", got, sentence)
		}
		emitted, final := replayUpdateFrames(t, []string{
			`{"type":1,"target":"update","arguments":[{"writeAtCursor":` + mustQuote(sentence) + `}]}`,
		})
		if final != sentence {
			t.Errorf("最终文本=%q want 原样 %q", final, sentence)
		}
		if joined := strings.Join(emitted, ""); joined != sentence {
			t.Errorf("流式已发送=%q want 原样 %q", joined, sentence)
		}
	}
}

// d) 流式与非流式必须逐字节一致：无论上游把正文切成什么形状的递增快照，客户端
// 逐段收到的拼接结果都等于非流式最终文本。
//
// 上游的正文以「完整快照」而不是任意字节切片到达（updateSnapshots 的取材就是
// writeAtCursor / messages[].text），所以这里按快照边界切，覆盖的正是生产形状：
// 标记可能刚好被某一帧切在中间。
func TestStreamingAndNonStreamingStripResultsMatch(t *testing.T) {
	bodies := []string{
		attachmentBody,
		"这是结论\ue200cite\ue202turn3file1\ue201。后续说明，这句要足够长以越过抖动窗口。",
		"结论如上，这句要足够长以越过抖动窗口才能进入流式下发 \ue200cite\ue202turn3file1\ue201。",
		"见下文，这句要足够长以越过抖动窗口才能进入流式下发 \ue200cite\ue202turn3file1\ue201 下一段继续。",
		"示例说明足够长以越过抖动窗口\ue200cite\ue202turn1file1\ue201：\n```python\ndef f():\n    return 1\n```\n",
		"步骤说明足够长以越过抖动窗口，请看下面这行：\n    \ue200cite\ue202turn1file1\ue201return 1\n",
		"甲乙丙丁戊己庚辛壬癸这段前缀足够长以越过抖动窗口\ue200cite\ue202a\ue201中间\ue200cite\ue202b\ue201结尾",
		"很抱歉，我似乎无法对此做出响应。",
	}
	for _, body := range bodies {
		want := stripCiteMarkers(body)
		// 按递增快照重放：每个前缀都是上游可能发出的一帧，标记会被切在中间。
		for step := 1; step <= 5; step++ {
			var snapshots []string
			for cut := step; cut < len(body); cut += step {
				if !utf8.RuneStart(body[cut]) {
					continue
				}
				snapshots = append(snapshots, body[:cut])
			}
			snapshots = append(snapshots, body)

			var emitted []string
			stream := newTextStream(func(delta string) error {
				emitted = append(emitted, delta)
				return nil
			})
			for _, snapshot := range snapshots {
				if err := stream.pushSnapshot(snapshot); err != nil {
					t.Fatalf("push snapshot: %v", err)
				}
			}
			if err := stream.finalize(); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			if got := strings.Join(emitted, ""); got != want {
				t.Fatalf("step=%d 流式拼接=%q\nwant 非流式=%q\nbody=%q", step, got, want, body)
			}
			if got := stream.finalText(); got != want {
				t.Fatalf("step=%d 最终文本=%q\nwant %q\nbody=%q", step, got, want, body)
			}
		}
	}
}

// 同一条不变量走完整的 textStream 出口：流式增量拼接 == finalText，且两者都不含
// 私用区标记。
func TestTextStreamStreamingOutputMatchesFinalText(t *testing.T) {
	bodies := [][]string{
		{attachmentBody, citeOnlySnapshot},
		{"这段前缀足够长以越过抖动窗口，会先发给客户端，随后上游补上引用锚点。",
			"这段前缀足够长以越过抖动窗口，会先发给客户端，随后上游补上引用锚点。\ue200cite\ue202turn2file2\ue201"},
		{"部分正文足够长以越过抖动窗口，这里先给出结论。",
			"部分正文足够长以越过抖动窗口，这里先给出结论\ue200cite\ue202turn4file4\ue201。补充：[a.md](" + attachmentURL + ")",
			citeOnlySnapshot},
		{"检索结果如下，这段前缀足够长以越过抖动窗口 ",
			"检索结果如下，这段前缀足够长以越过抖动窗口 \ue200cite\ue202turn5file5\ue201。"},
	}
	for _, snapshots := range bodies {
		var emitted []string
		stream := newTextStream(func(delta string) error {
			emitted = append(emitted, delta)
			return nil
		})
		for _, snapshot := range snapshots {
			if err := stream.pushSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.finalize(); err != nil {
			t.Fatal(err)
		}
		final := stream.finalText()
		joined := strings.Join(emitted, "")
		if joined != final {
			t.Errorf("流式=%q\n非流式=%q\nsnapshots=%q", joined, final, snapshots)
		}
		if strings.ContainsAny(joined, citeMarkerRunes) {
			t.Errorf("流式输出残留私用区标记：%q", joined)
		}
		assertNoDuplicateText(t, emitted)
	}
}

// 随机化不变量：任意递增快照序列下，流式拼接恒等于 finalText，输出不含私用区
// 标记，且不复读。剥离必须是「切分无关」的，否则流式客户端会看到与非流式不同的
// 文本。
func TestCiteStrippingSplitInvarianceUnderRandomSnapshots(t *testing.T) {
	source := rand.New(rand.NewSource(20260823))
	pieces := []string{
		"abc", "中文正文", " ", "\t", "\n", "。", ",", "[a](https://x/y)", "```", "    ",
		string(citeMarkerStart), string(citeMarkerEnd), string(citeMarkerSep),
		"cite", "turn12file12", "\ue200cite\ue202turn1file1\ue201",
	}
	for iteration := 0; iteration < 3000; iteration++ {
		var b strings.Builder
		for n := 0; n < 1+source.Intn(14); n++ {
			b.WriteString(pieces[source.Intn(len(pieces))])
		}
		body := b.String()
		if !utf8.ValidString(body) {
			t.Fatalf("iteration %d 构造出非法 UTF-8：%q", iteration, body)
		}
		want := stripCiteMarkers(body)
		if strings.ContainsAny(want, citeMarkerRunes) {
			t.Fatalf("iteration %d 剥离后仍有标记：body=%q got=%q", iteration, body, want)
		}
		// 幂等性：交付文本再剥一次必须零改动（finalText 会再兜一层）。
		if again := stripCiteMarkers(want); again != want {
			t.Fatalf("iteration %d 剥离不幂等：%q -> %q", iteration, want, again)
		}

		var snapshots []string
		index := 0
		for index < len(body) {
			index += 1 + source.Intn(6)
			if index >= len(body) {
				break
			}
			if !utf8.RuneStart(body[index]) {
				continue
			}
			snapshots = append(snapshots, body[:index])
		}
		snapshots = append(snapshots, body)

		var emitted []string
		stream := newTextStream(func(delta string) error {
			emitted = append(emitted, delta)
			return nil
		})
		for _, snapshot := range snapshots {
			if err := stream.pushSnapshot(snapshot); err != nil {
				t.Fatalf("push snapshot: %v", err)
			}
		}
		if err := stream.finalize(); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		joined := strings.Join(emitted, "")
		// 退化类：整段剥离后只剩空白。这类快照按规则不算正文（纯元数据）而被跳过，
		// 最终文本取决于此前是否已有非标记快照建立过正文，因此不做等值断言 ——
		// 只压两条必须成立的性质：两条路径不分叉、输出无标记残留。
		if strings.TrimSpace(want) == "" {
			if joined != stream.finalText() {
				t.Fatalf("iteration %d 流式与非流式分叉\n body=%q\n stream=%q\n final=%q",
					iteration, body, joined, stream.finalText())
			}
			if strings.ContainsAny(joined, citeMarkerRunes) {
				t.Fatalf("iteration %d 输出残留私用区标记：%q", iteration, joined)
			}
			continue
		}
		if got := stream.finalText(); got != want {
			t.Fatalf("iteration %d 最终文本不一致\n body=%q\n got=%q\n want=%q", iteration, body, got, want)
		}
		if joined != want {
			t.Fatalf("iteration %d 流式与非流式分叉\n body=%q\n stream=%q\n whole=%q\n snapshots=%q",
				iteration, body, joined, want, snapshots)
		}
		var before strings.Builder
		for i, delta := range emitted {
			if i > 0 && len(delta) >= snapshotMinAnchorBytes && strings.Contains(before.String(), delta) {
				t.Fatalf("iteration %d 复读：增量[%d]=%q 已在 %q 中\n body=%q", iteration, i, delta, before.String(), body)
			}
			before.WriteString(delta)
		}
	}
}

// 剥离不得丢掉标记之外的任何非空白字符：这是「只删元数据」的强约束。
func TestStripCiteMarkersKeepsAllNonMarkerRunes(t *testing.T) {
	body := "前\ue200cite\ue202turn1\ue201中[链接](https://example.com/a?b=c&d=e)后\n\tTab缩进保留\n"
	got := stripCiteMarkers(body)
	want := "前中[链接](https://example.com/a?b=c&d=e)后\n\tTab缩进保留\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// hasBodyContent 的判定必须基于分隔符结构，不依赖 cite 这个英文锚点名。
func TestHasBodyContentIsAnchorNameAgnostic(t *testing.T) {
	for _, marker := range []string{
		"\ue200cite\ue202turn12file12\ue201",
		"\ue200引用\ue202第12轮文件12\ue201",
		"\ue200ref\ue202abc\ue201\ue200ref\ue202def\ue201",
		"  \ue200x\ue202y\ue201\n",
		"\ue200cite\ue202turn12file12",
	} {
		if hasBodyContent(marker) {
			t.Errorf("纯标记被判成正文：%q", marker)
		}
	}
	for _, body := range []string{
		attachmentBody,
		"答案\ue200cite\ue202turn1\ue201",
		"很抱歉，我似乎无法对此做出响应。",
		"a",
	} {
		if !hasBodyContent(body) {
			t.Errorf("正文被判成纯标记：%q", body)
		}
	}
	// 不含标记的文本不属于本缺陷，行为保持不变。
	if isCiteMarkerOnly("   ") {
		t.Error("纯空白（无标记）不应被判成引用标记快照")
	}
}
