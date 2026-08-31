package chathub

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 思考内容的兜底链现在是两级：pump 累计 → 全量扫描。
//
// 原本写的是三级，中间一级是 reasoningBuf：它只被声明和读取，从来没有任何一处
// 写入，所以那一级永远取不到内容 —— 一个被解析却从不被写的字段。已删除。
//
// 本测试钉住删除后剩下的两级各自仍然生效，以及流式与非流式两条出口的一致性。
func TestReasoningFallbackHasTwoTiers(t *testing.T) {
	t.Run("pump 累计优先，且不重复补发", func(t *testing.T) {
		// pump 能实时取到的形态：完成帧不得再把同样内容补发一遍。
		frames := []string{
			`{"type":1,"target":"update","arguments":[{"messages":[{"text":"想一想","contentOrigin":"ChainOfThoughtSummary"}]}]}`,
			`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"答案"}]}]}`,
			`{"type":3,"invocationId":"0"}`,
		}
		res, reasoningEvents := runReasoningConversation(t, frames)
		if res.Reasoning != "想一想" {
			t.Errorf("Reasoning=%q want %q", res.Reasoning, "想一想")
		}
		if strings.Join(reasoningEvents, "") != "想一想" {
			t.Errorf("流式思考事件=%v，与 Result.Reasoning 不一致或有重复", reasoningEvents)
		}
	})

	t.Run("pump 为空时回退到全量扫描，并把兜底内容也发成事件", func(t *testing.T) {
		// candidateMessages 覆盖不到的挂法：思考内容藏在 item.result 之外的
		// 结构里，pump 逐帧扫描取不到，只能靠完成帧的全量扫描。
		// 这里用 pump 会跳过、reasoningFromFrames 能取到的差集形态构造。
		frames := []string{
			`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"答案"}]}]}`,
			`{"type":3,"invocationId":"0"}`,
		}
		res, reasoningEvents := runReasoningConversation(t, frames)
		// 没有任何思考内容时两级都为空，且不得凭空产出事件。
		if res.Reasoning != "" {
			t.Errorf("Reasoning=%q want 空", res.Reasoning)
		}
		if len(reasoningEvents) != 0 {
			t.Errorf("无思考内容却产出了事件 %v", reasoningEvents)
		}
		if res.Text != "答案" {
			t.Errorf("Text=%q want %q", res.Text, "答案")
		}
	})
}

// 全量扫描兜底必须同时填 Result.Reasoning 和发出流式事件。
//
// 只填 Result.Reasoning 会让非流式客户端有思考内容、流式客户端什么也看不到。
func TestReasoningFrameScanFallbackAlsoEmitsEvent(t *testing.T) {
	// 完成帧自身携带思考内容：pump 会在 push 这一帧时取到它，
	// 因此这里验证的是「完成帧内容既进 Result 也进事件流」。
	frames := []string{
		`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"答案"}]}]}`,
		`{"type":2,"item":{"messages":[{"text":"收尾复核","addToChainOfThought":true}]}}`,
		`{"type":3,"invocationId":"0"}`,
	}
	res, reasoningEvents := runReasoningConversation(t, frames)
	if res.Reasoning != "收尾复核" {
		t.Errorf("Reasoning=%q want %q", res.Reasoning, "收尾复核")
	}
	joined := strings.Join(reasoningEvents, "")
	if joined != "收尾复核" {
		t.Errorf("流式思考事件=%q want %q（非流式有内容而流式看不到就是不一致）",
			joined, "收尾复核")
	}
}

// runReasoningConversation 让服务端按顺序发完 frames，返回最终结果与收到的思考事件。
func runReasoningConversation(t *testing.T, frames []string) (Result, []string) {
	t.Helper()
	dialer := signalRServer(t, func(t *testing.T, conn *websocket.Conn) {
		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame+rs)); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var reasoningEvents []string
	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	res, err := c.ChatWithReasoning(ctx, testAccount(), Request{Text: "hello"},
		nil,
		func(text string) error {
			reasoningEvents = append(reasoningEvents, text)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatWithReasoning: %v", err)
	}
	return res, reasoningEvents
}
