package web

import "testing"

// 主不变量：去重可以缩短候选列表，但不允许把它清空。
//
// 清空的后果是静音的：该轮退化成 ordinary_answer_fallback，回答轮拿到
// native_tools=0，模型如实说「这个回合没给我工具」。用户看到的是「模型幻觉」，
// 而真实故障点是 filterCompletedCalls 的名字启发式判错，日志里只有一个
// post_ledger=0 能看出来。2026-09-02 与 09-03 各踩一次，工具名不同、根因同一。
//
// 这一条失败即是该类缺陷复现。不要用调整断言的方式让它通过，也不要靠往
// toolRewritesState / toolLooksObservational 的词表里加词来「修」——
// 那些表按定义不可能完备，加词只是让下一个没见过的名字继续掉坑。
func TestDedupeNeverEmptiesParsedCandidates(t *testing.T) {
	mutation := dropStep{"apply_patch", `{"diff":"@@"}`, `{"applied":true}`}
	for _, tc := range []struct{ name, args, result string }{
		// filterCompletedCalls 会剔除的各类同参重放，逐一确认不会归零。
		{"write_file", `{"path":"a.go","content":"v1"}`, `{"bytes":2}`},
		{"write_stdin", `{"chars":"","session_id":7773}`, "Process running with session ID 7773"},
		{"exec_command", `{"cmd":"pnpm build"}`, "exit_code=1\ncommand not found"},
		{"Bash", `{"command":"go test ./..."}`, "exit_code=2\nFAIL"},
		{"delete_path", `{"path":"tmp"}`, `{"deleted":true}`},
	} {
		l := ledgerFromSteps(dropStep{tc.name, tc.args, tc.result}, mutation)
		in := []detectedToolCall{{Name: tc.name, Arguments: []byte(tc.args)}}
		got := dedupeCompletedCalls("test-req", in, l)
		if len(got) != 1 {
			t.Errorf("%s: 唯一候选被去重清空（len=%d）：该轮会退化成散文并自称没有工具", tc.name, len(got))
			continue
		}
		if got[0].Name != tc.name {
			t.Errorf("%s: 保留下来的候选名字变了：%s", tc.name, got[0].Name)
		}
	}
}

// 列表里还有别的候选时，去重必须照常生效 —— 不变量只兜住「归零」这一种情况。
func TestDedupeStillDropsDuplicatesWhenOthersSurvive(t *testing.T) {
	l := ledgerFromSteps(
		dropStep{"write_file", `{"path":"a.go","content":"v1"}`, `{"bytes":2}`},
		dropStep{"apply_patch", `{"diff":"@@"}`, `{"applied":true}`},
	)
	in := []detectedToolCall{
		{Name: "write_file", Arguments: []byte(`{"path":"a.go","content":"v1"}`)}, // 同参重放，应被剔除
		{Name: "Read", Arguments: []byte(`{"path":"b.go"}`)},                      // 全新，应保留
	}
	got := dedupeCompletedCalls("test-req", in, l)
	if len(got) != 1 || got[0].Name != "Read" {
		names := make([]string, len(got))
		for i, c := range got {
			names[i] = c.Name
		}
		t.Errorf("有其他候选存活时去重必须照常生效，得到 %v", names)
	}
}

// filterCompletedCalls 不得就地改写入参：dedupeCompletedCalls 的兜底要靠原始
// 候选，入参被覆盖过就退不回去了。原实现用 calls[:0] 复用底层数组，两个候选
// 时会把 in[0] 覆盖成 in[1]。
func TestFilterCompletedCallsDoesNotAliasInput(t *testing.T) {
	l := ledgerFromSteps(
		dropStep{"write_file", `{"path":"a.go","content":"v1"}`, `{"bytes":2}`},
		dropStep{"apply_patch", `{"diff":"@@"}`, `{"applied":true}`},
	)
	in := []detectedToolCall{
		{Name: "write_file", Arguments: []byte(`{"path":"a.go","content":"v1"}`)},
		{Name: "Read", Arguments: []byte(`{"path":"b.go"}`)},
	}
	want := []string{"write_file", "Read"}
	_ = filterCompletedCalls(in, l)
	for i, c := range in {
		if c.Name != want[i] {
			t.Errorf("入参被就地改写：in[%d] 应为 %s，实为 %s", i, want[i], c.Name)
		}
	}
}
