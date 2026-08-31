package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

// 多张思考卡片交错增长时，增量必须按卡片记账。
//
// 修复前 reasoningPump 只有一个扁平累加器 total，把所有卡片的文本首尾相接。
// ChatHub 会交错重投多张各自增长的卡片，一旦交错，total 就不再是任何一张卡片的
// 前缀，于是每次重投都被判成「全新内容」而整段重发：客户端看到成倍重复的思考
// 文本，total 还在无界增长，前缀判断此后永远命中不了。
//
// 交错序列 A1 / B1 / A1A2 / B1B2：修复前第三帧会把整个 "甲步一甲步二" 当新内容
// 发出去，"甲步一" 被重复；修复后只发新增的 "甲步二"。
func TestReasoningPumpTracksInterleavedChunksSeparately(t *testing.T) {
	var emitted []string
	pump := newReasoningPump(func(ev StreamEvent) error {
		emitted = append(emitted, ev.Text)
		return nil
	})

	frames := []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"甲步一","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"乙步一","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"甲步一甲步二","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"乙步一乙步二","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"甲步一甲步二甲步三","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
	}
	for _, frame := range frames {
		if err := pump.push(json.RawMessage(frame)); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"甲步一", "乙步一", "甲步二", "乙步二", "甲步三"}
	if len(emitted) != len(want) {
		t.Fatalf("emitted=%v want %v", emitted, want)
	}
	for i := range want {
		if emitted[i] != want[i] {
			t.Errorf("increment[%d]=%q want %q（交错时增长被记到了别的卡片上）",
				i, emitted[i], want[i])
		}
	}

	// 每一段思考文本在累计结果里只能出现一次。
	accumulated := pump.text()
	for _, fragment := range []string{"甲步一", "甲步二", "甲步三", "乙步一", "乙步二"} {
		if n := strings.Count(accumulated, fragment); n != 1 {
			t.Errorf("%q 在累计文本中出现 %d 次，want 1；accumulated=%q",
				fragment, n, accumulated)
		}
	}
}

// 上游带 messageId 时按 id 记账，两张卡片的文本即使互不为前缀也不会串账。
func TestReasoningPumpKeysChunksByMessageID(t *testing.T) {
	var emitted []string
	pump := newReasoningPump(func(ev StreamEvent) error {
		emitted = append(emitted, ev.Text)
		return nil
	})

	frames := []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-1","text":"读题","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-2","text":"查表","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-1","text":"读题、列式","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		// m-2 的重投没有增长，不得再发。
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-2","text":"查表","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
	}
	for _, frame := range frames {
		if err := pump.push(json.RawMessage(frame)); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"读题", "查表", "、列式"}
	if len(emitted) != len(want) {
		t.Fatalf("emitted=%v want %v", emitted, want)
	}
	for i := range want {
		if emitted[i] != want[i] {
			t.Errorf("increment[%d]=%q want %q", i, emitted[i], want[i])
		}
	}
}

// 同一张卡片被整段改写（新文本不是旧文本的延长）时，只能算这一张卡片的事，
// 不得因为「和别的卡片拼起来不成前缀」而反复整段重发。
func TestReasoningPumpRewriteDoesNotCascade(t *testing.T) {
	var emitted []string
	pump := newReasoningPump(func(ev StreamEvent) error {
		emitted = append(emitted, ev.Text)
		return nil
	})

	frames := []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-1","text":"初版结论","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-1","text":"改写后的结论","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"messageId":"m-1","text":"改写后的结论，补充","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
	}
	for _, frame := range frames {
		if err := pump.push(json.RawMessage(frame)); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"初版结论", "改写后的结论", "，补充"}
	if len(emitted) != len(want) {
		t.Fatalf("emitted=%v want %v", emitted, want)
	}
	for i := range want {
		if emitted[i] != want[i] {
			t.Errorf("increment[%d]=%q want %q", i, emitted[i], want[i])
		}
	}
}

// 更短的旧快照重投必须丢弃，不能被当成一张新卡片。
func TestReasoningPumpDropsShorterRedelivery(t *testing.T) {
	var emitted []string
	pump := newReasoningPump(func(ev StreamEvent) error {
		emitted = append(emitted, ev.Text)
		return nil
	})

	frames := []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"第一步第二步","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
		`{"type":1,"target":"update","arguments":[{"messages":[{"text":"第一步","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
	}
	for _, frame := range frames {
		if err := pump.push(json.RawMessage(frame)); err != nil {
			t.Fatal(err)
		}
	}

	if len(emitted) != 1 || emitted[0] != "第一步第二步" {
		t.Fatalf("emitted=%v want 单个 %q", emitted, "第一步第二步")
	}
	if pump.text() != "第一步第二步" {
		t.Errorf("accumulated=%q", pump.text())
	}
}
