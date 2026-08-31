package web

import (
	"strings"
	"testing"
)

// 用户实测报错（Codex CLI → CC Switch → 本网关 /v1/responses，
// model=gpt-5.6-reasoning）：
//
//	upstream_status: HTTP 400
//	cause: tool results missing before assistant message at index 467
//
// 下面几条测试把这条报错的成因和修复固定住。

// 先钉住因果链：一轮并行调用如果被拆成多条 assistant 消息，校验器就会用用户
// 看到的那句原话拒掉整个请求。这条测试不依赖转换器，直接构造那个形状，所以
// 即使以后有人重写 openAI() 也仍然成立。
func TestSplitParallelCallsAreRejectedByValidator(t *testing.T) {
	split := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_a", "type": "function"}}},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_b", "type": "function"}}},
		{Role: "tool", ToolCallID: "call_a", Content: "1"},
		{Role: "tool", ToolCallID: "call_b", Content: "2"},
	}
	err := validateToolConversation(split)
	if err == nil {
		t.Fatal("splitting one parallel round across two assistant messages must be rejected")
	}
	if !strings.Contains(err.Error(), "tool results missing before assistant message at index 1") {
		t.Fatalf("unexpected error text %q; the reported 400 came from this branch", err)
	}
}

// 修复本体：连续的 function_call 必须并入同一条 assistant 消息。
func TestResponsesParallelFunctionCallsCoalesce(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "read_file", "arguments": `{"path":"a.go"}`},
		map[string]any{"type": "function_call", "call_id": "call_b", "name": "read_file", "arguments": `{"path":"b.go"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "package a"},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "package b"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 {
		t.Fatalf("want 1 assistant + 2 tool = 3 messages, got %d: %#v", len(o.Messages), o.Messages)
	}
	if o.Messages[0].Role != "assistant" || len(o.Messages[0].ToolCalls) != 2 {
		t.Fatalf("parallel calls not coalesced into one assistant turn: %#v", o.Messages[0])
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("parallel tool history still rejected: %v", err)
	}
}

// 反方向同样重要：串行调用不能被合并。若把 B 并进 A 那一轮，A 的结果就会先于
// 第二个调用出现，最终报 missing tool result —— 换一个错，不是修好。
func TestResponsesSequentialFunctionCallsStaySeparate(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "1"},
		map[string]any{"type": "function_call", "call_id": "call_b", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "2"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 4 {
		t.Fatalf("want 4 alternating messages, got %d: %#v", len(o.Messages), o.Messages)
	}
	for _, i := range []int{0, 2} {
		if o.Messages[i].Role != "assistant" || len(o.Messages[i].ToolCalls) != 1 {
			t.Fatalf("message %d must hold exactly one call: %#v", i, o.Messages[i])
		}
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("sequential tool history rejected: %v", err)
	}
}

// function_call_progress 是执行中的传输元数据，不产生消息，因此不能打断同一轮
// 并行调用的归组。
func TestResponsesProgressBetweenParallelCallsStillCoalesces(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_progress", "call_id": "call_a", "message": "working"},
		map[string]any{"type": "function_call", "call_id": "call_b", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "1"},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "2"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 || len(o.Messages[0].ToolCalls) != 2 {
		t.Fatalf("progress broke the parallel round: %#v", o.Messages)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("history with progress rejected: %v", err)
	}
}

// 普通消息必须打断归组：user 之后的调用属于新一轮。
func TestResponsesUserMessageBreaksToolCallRun(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "1"},
		map[string]any{"role": "user", "content": "now do the other one"},
		map[string]any{"type": "function_call", "call_id": "call_b", "name": "f", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "2"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d: %#v", len(o.Messages), o.Messages)
	}
	if o.Messages[3].Role != "assistant" || len(o.Messages[3].ToolCalls) != 1 {
		t.Fatalf("post-user call must start a fresh assistant turn: %#v", o.Messages[3])
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("history with an interleaved user message rejected: %v", err)
	}
}

// 混合类型的并行轮（Codex 同时发 function_call 与 custom_tool_call）也要归并。
func TestResponsesMixedParallelCallTypesCoalesce(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "read_file", "arguments": `{}`},
		map[string]any{"type": "custom_tool_call", "call_id": "call_b", "name": "exec", "input": "uname -s"},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "x"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_b", "output": "Linux"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 || len(o.Messages[0].ToolCalls) != 2 {
		t.Fatalf("mixed parallel round not coalesced: %#v", o.Messages)
	}
	if o.Messages[0].ToolCalls[0]["type"] != "function" || o.Messages[0].ToolCalls[1]["type"] != "custom" {
		t.Fatalf("call types not preserved: %#v", o.Messages[0].ToolCalls)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("mixed parallel history rejected: %v", err)
	}
}
