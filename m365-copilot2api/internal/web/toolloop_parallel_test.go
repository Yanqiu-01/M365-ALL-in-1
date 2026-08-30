package web

import "testing"

// codex_catalog.go 向客户端宣告 supports_parallel_tool_calls=true。宣告了一项能
// 力，就必须同样实现它的关闭语义：Codex 关掉并行后一次只执行一个调用，多余的
// 调用不会有结果返回，下一轮历史里就成了「有调用无结果」，被
// validateToolConversation 判为非法。

func twoCalls() []detectedToolCall {
	return []detectedToolCall{
		{ID: "call_a", Type: "function", Name: "read_file", Arguments: []byte(`{}`)},
		{ID: "call_b", Type: "function", Name: "list_dir", Arguments: []byte(`{}`)},
	}
}

func TestEnforceParallelToolCallsUnsetAllowsParallel(t *testing.T) {
	// 未指定时不能擅自降级：并行是宣告过的默认能力。
	calls, dropped := enforceParallelToolCalls(twoCalls(), nil)
	if len(calls) != 2 || len(dropped) != 0 {
		t.Fatalf("calls=%d dropped=%d, want 2/0", len(calls), len(dropped))
	}
}

func TestEnforceParallelToolCallsTrueAllowsParallel(t *testing.T) {
	yes := true
	calls, dropped := enforceParallelToolCalls(twoCalls(), &yes)
	if len(calls) != 2 || len(dropped) != 0 {
		t.Fatalf("calls=%d dropped=%d, want 2/0", len(calls), len(dropped))
	}
}

func TestEnforceParallelToolCallsFalseKeepsFirstOnly(t *testing.T) {
	no := false
	calls, dropped := enforceParallelToolCalls(twoCalls(), &no)
	if len(calls) != 1 {
		t.Fatalf("want exactly one call, got %d: %+v", len(calls), calls)
	}
	// 保留第一个而非整批拒绝：字段语义是「降为单调用」，被丢弃的调用模型下一轮
	// 可以重新发起。
	if calls[0].ID != "call_a" {
		t.Errorf("kept %q, want the first call call_a", calls[0].ID)
	}
	if len(dropped) != 1 || dropped[0].Name != "list_dir" {
		t.Fatalf("dropped=%+v, want one entry naming list_dir", dropped)
	}
	if dropped[0].Reason == "" {
		t.Error("dropped call must carry a reason so the caller can log it")
	}
}

func TestEnforceParallelToolCallsFalseWithSingleCallIsUnchanged(t *testing.T) {
	no := false
	one := twoCalls()[:1]
	calls, dropped := enforceParallelToolCalls(one, &no)
	if len(calls) != 1 || len(dropped) != 0 {
		t.Fatalf("calls=%d dropped=%d, want 1/0", len(calls), len(dropped))
	}
}

func TestEnforceParallelToolCallsFalseOnEmptyIsSafe(t *testing.T) {
	no := false
	calls, dropped := enforceParallelToolCalls(nil, &no)
	if len(calls) != 0 || len(dropped) != 0 {
		t.Fatalf("calls=%d dropped=%d, want 0/0", len(calls), len(dropped))
	}
}
