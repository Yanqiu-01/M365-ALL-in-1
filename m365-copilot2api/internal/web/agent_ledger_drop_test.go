package web

import (
	"fmt"
	"testing"
)

// 本文件锁死 filterCompletedCalls 的丢弃边界。
//
// 起因是网关只记控制流、不记内容的路由日志（router_outcome.go）在 2026-09-02
// 连续三次记到同一形状：
//
//	13:06:13 substage=validated raw_candidates=1 post_ledger=0 valid_calls=0 rejected_calls=0
//	13:06:36 substage=validated raw_candidates=1 post_ledger=0 valid_calls=0 rejected_calls=0
//	13:15:24 substage=validated disposition=intent_retry raw_candidates=1 post_ledger=0 ...
//
// raw_candidates 是 parseModelToolDecision 的产出，而它内部（selectAllValid）
// 已经校验过「名字已声明 + 参数合 schema」；post_ledger 取自
// filterCompletedCalls 之后（server.go:1882-1885）。因此这三次都是「模型选中
// 了一个完全合法的工具，被 ledger 去重静默丢空」，该轮随后退化成散文或
// intent_retry —— 与 2026-08 那次 run_tests({}) 被永久剔除、评测罚到地板分
// 是同一类故障。
type dropStep struct{ name, args, result string }

func ledgerFromSteps(steps ...dropStep) agentLedger {
	var msgs []oaiMsg
	for i, s := range steps {
		id := fmt.Sprintf("dcall_%d", i)
		msgs = append(msgs, toolCallMsg(id, s.name, s.args),
			oaiMsg{Role: "tool", ToolCallID: id, Content: s.result})
	}
	return buildAgentLedger(msgs)
}

func callKept(l agentLedger, name, args string) bool {
	in := []detectedToolCall{{Name: name, Arguments: []byte(args)}}
	return len(filterCompletedCalls(in, l)) == 1
}

// 回归：评测里的 run_tests 参数恒为 {}，改完代码后必须能再跑一次。
// 这一条一旦失败，就是 2026-08「连续 N 步提前结束、闭环未通过、罚到地板分」
// 的复现，不要用调整断言的方式让它通过。
func TestLedgerDropRunTestsWithEmptyArgsStaysRepeatable(t *testing.T) {
	l := ledgerFromSteps(
		dropStep{"run_tests", "{}", "TESTS FAILED: 3/7"},
		dropStep{"write_file", `{"path":"inventory.py","content":"fixed"}`, `{"path":"inventory.py","bytes":5}`},
	)
	if !callKept(l, "run_tests", "{}") {
		t.Error("run_tests({}) 被当作已完成而剔除：模型无法验证修复，网关会判定「未调用工具」并空转到步数耗尽")
	}
	// 同一条 ledger 里，参数完全相同的写入仍必须被剔除 —— 去重的本职。
	if callKept(l, "write_file", `{"path":"inventory.py","content":"fixed"}`) {
		t.Error("参数完全相同的变更类重放必须继续被剔除")
	}
}

// 主缺陷：名字落在两张关键字表之外的工具，同参重复被无条件剔除。
//
// Bash{"command":"go test ./..."} 与 run_tests({}) 是同一件事，只是名字既不含
// read/list/test，也不含 write/exec/run，原判据（只放行 toolLooksObservational）
// 把它当成变更类丢掉。declared_tools=71 的真实客户端里这类名字是主力工具。
func TestLedgerDropUnclassifiedToolRepeatsAfterMutation(t *testing.T) {
	mutation := dropStep{"Edit", `{"path":"a.go","old":"x","new":"y"}`, `{"applied":true}`}
	for _, tc := range []struct{ name, args, result string }{
		{"Bash", `{"command":"go test ./..."}`, "FAIL: 2 tests"}, // 改完代码后复跑测试
		{"Glob", `{"pattern":"**/*.go"}`, "3 files"},             // 纯只读，表里没有
		{"poll_job", `{"id":1}`, "still running"},                // 轮询类，重复才是本意
		{"TodoRead", `{}`, "[]"},                                 // 空参 + 表外
	} {
		l := ledgerFromSteps(dropStep{tc.name, tc.args, tc.result}, mutation)
		if !l.hasAnyMutation() {
			t.Fatalf("test setup: Edit 应被算作变更（%s）", tc.name)
		}
		if !callKept(l, tc.name, tc.args) {
			t.Errorf("%s 同参重复在状态已变更后被剔除：表外的名字不该比写入更严", tc.name)
		}
	}
}

// 明确变更类的同参重放，在状态已变更之后同样必须继续被剔除 —— 放宽只针对
// 「不是明确变更类」的名字，不是对所有名字开闸。
func TestLedgerDropStillBlocksMutatingReplayAfterMutation(t *testing.T) {
	l := ledgerFromSteps(
		dropStep{"write_file", `{"path":"a.go","content":"v1"}`, `{"bytes":2}`},
		dropStep{"apply_patch", `{"diff":"@@"}`, `{"applied":true}`},
	)
	for _, tc := range []struct{ name, args string }{
		{"write_file", `{"path":"a.go","content":"v1"}`},
		{"apply_patch", `{"diff":"@@"}`},
	} {
		if callKept(l, tc.name, tc.args) {
			t.Errorf("%s 参数完全相同的变更类重放必须被剔除", tc.name)
		}
	}
	// 书写形式不同、语义相同的参数仍按同一调用去重。
	if callKept(l, "write_file", ` { "content":"v1", "path":"a.go" } `) {
		t.Error("规范化后相同的变更类参数必须仍被判为重复")
	}
	// 参数不同的写入必须放行。
	if !callKept(l, "write_file", `{"path":"a.go","content":"v2"}`) {
		t.Error("内容不同的写入不应被剔除")
	}
}

// 失败的先例不构成「已完成的劳动」：那一次什么都没产出，压制同参重试等于
// 禁止模型从瞬时错误（文件被占用、命令超时）里恢复。此时工作区状态尚未改变，
// 原判据无论名字是什么都会剔除。
func TestLedgerDropFailedPrecedentDoesNotSuppressRetry(t *testing.T) {
	l := ledgerFromSteps(dropStep{"read_file", `{"path":"a.go"}`, "Error: file is locked by another process"})
	if len(l.Completed) != 1 || !l.Completed[0].Failed {
		t.Fatalf("test setup: 该结果应被判为失败，got %+v", l.Completed)
	}
	if l.hasAnyMutation() {
		t.Fatal("test setup: 只有一次读取，不应算作发生过变更")
	}
	if !callKept(l, "read_file", `{"path":"a.go"}`) {
		t.Error("失败读取的同参重试被剔除：模型无法重试，该轮直接退化成散文")
	}
}

// 反向：失败的变更类调用不放行。写入/执行可能已部分生效，而
// toolResultLooksFailed 对非观察类走宽匹配（输出里出现 "error" 即算失败），
// 放行会把一次其实成功的执行变成可重放。
func TestLedgerDropFailedMutatingCallStaysSuppressed(t *testing.T) {
	l := ledgerFromSteps(dropStep{"write_file", `{"path":"a.go","content":"v1"}`, "error: permission denied"})
	if !l.Completed[0].Failed {
		t.Fatalf("test setup: 该结果应被判为失败，got %+v", l.Completed)
	}
	if callKept(l, "write_file", `{"path":"a.go","content":"v1"}`) {
		t.Error("失败的变更类同参重放不得放行：可能已部分生效，且失败判定本身是宽匹配")
	}
}

// 既有契约（benchmark_toolloop_progress_test.go:51 已锁）：状态未改变时，
// 成功的观察类同参重复仍应剔除。这里再钉一次，说明放宽没有顺手改掉它 ——
// 「磁盘上的文件被网关看不见的外部动作改了」这一情形因此仍会被剔除，属于
// 已知残留缺口，需要单独决策而不是在本次一并放开。
func TestLedgerDropSuccessfulObservationWithoutMutationStaysFiltered(t *testing.T) {
	l := ledgerFromSteps(dropStep{"read_file", `{"path":"a.py"}`, "contents"})
	if l.Completed[0].Failed || l.hasAnyMutation() {
		t.Fatalf("test setup: 应为一次成功读取且无变更，got %+v", l.Completed)
	}
	if callKept(l, "read_file", `{"path":"a.py"}`) {
		t.Error("状态未改变时重复读取同一文件仍应剔除")
	}
}

// 一批调用里只丢该丢的那些，剩下的顺序与内容不能被就地复用的底层数组弄坏。
func TestLedgerDropKeepsSurvivorsIntactInBatch(t *testing.T) {
	l := ledgerFromSteps(
		dropStep{"Edit", `{"path":"a.go","old":"x","new":"y"}`, `{"applied":true}`},
		dropStep{"Bash", `{"command":"go build ./..."}`, "ok"},
	)
	in := []detectedToolCall{
		{Name: "Edit", Arguments: []byte(`{"path":"a.go","old":"x","new":"y"}`)}, // 同参重放 → 丢
		{Name: "Bash", Arguments: []byte(`{"command":"go build ./..."}`)},        // 表外 + 已变更 → 留
		{Name: "Write", Arguments: []byte(`{"path":"b.go","content":"new"}`)},    // 全新 → 留
	}
	got := filterCompletedCalls(in, l)
	if len(got) != 2 || got[0].Name != "Bash" || got[1].Name != "Write" {
		t.Fatalf("批量过滤结果不对: %#v", got)
	}
	if string(got[1].Arguments) != `{"path":"b.go","content":"new"}` {
		t.Errorf("保留下来的调用参数被改写: %s", got[1].Arguments)
	}
}

// shouldSuppressCompletedCall（已随 2026-09-09 审计删除）曾与
// toolCanRepeatSameArguments 逐字同形、取值处处相同，但命名与注释按相反极性
// 解释同一判据（「应当压制」对「可以重复」）。所以不能按它的名字接到
// filterCompletedCalls 上：那样读出来是「读取压制、写入放行」，正好把去重的
// 本职反过来。删除前用本测试钉住 toolCanRepeatSameArguments 的取值契约，
// 防止有人将来「顺手合并」回那个反向语义。
func TestLedgerDropDeadSuppressHelperIsNotTheRepeatGate(t *testing.T) {
	// 只读/表外名字：允许同名同参重复。
	for _, name := range []string{"read_file", "Glob", "poll_job"} {
		if !toolCanRepeatSameArguments(name) {
			t.Errorf("%s: 只读/表外名字应进入「可重复」分支", name)
		}
	}
	// 写入类：变更类不得进入「可重复」分支 —— 这正是与旧函数语义相反的地方。
	for _, name := range []string{"write_file", "apply_patch", "delete_path"} {
		if toolCanRepeatSameArguments(name) {
			t.Errorf("%s: 变更类不得进入「可重复」分支", name)
		}
	}
}
