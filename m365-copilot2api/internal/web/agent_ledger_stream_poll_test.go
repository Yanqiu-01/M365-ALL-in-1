package web

import "testing"

// 回归：Codex 的 write_stdin 轮询必须能同名同参重复。
//
// 复现自 2026-09-03 20:48:50 的真实请求（/v1/responses，declared_tools=31，
// 客户端顶层声明 14 个工具）。轨迹是 exec_command 起了一条 30 秒未结束的
// PowerShell 扫描（session 7773），write_stdin{"chars":"","session_id":7773}
// 轮询一次，模型第二次要轮询时被 ledger 丢空：
//
//	router-outcome disposition=ordinary_answer_fallback declared_tools=31
//	               parsed=true raw_candidates=1 post_ledger=0
//	               valid_calls=0 rejected_calls=0
//
// 结果那一轮退化成散文，回答写成「这个回合没有提供相应的本地 PowerShell 工具」。
// 这一条失败即是该缺陷复现，不要用调整断言的方式让它通过。
func TestLedgerKeepsShellSessionPolling(t *testing.T) {
	execArgs := `{"cmd":"Get-ChildItem -LiteralPath 'E:\\' -Recurse","max_output_tokens":20000,"workdir":"E:\\download","yield_time_ms":30000}`
	execOut := "Chunk ID: f456e9\nWall time: 30.0135 seconds\nProcess running with session ID 7773\n" +
		"Original token count: 936\nOutput:\n=== PowerShell / Location ===\n7      6      5\n"
	stdinArgs := `{"chars":"","max_output_tokens":20000,"session_id":7773,"yield_time_ms":30000}`
	stdinOut := "Chunk ID: ae00fd\nWall time: 30.0133 seconds\nProcess running with session ID 7773\n" +
		"Original token count: 0\nOutput:\n"

	l := ledgerFromSteps(
		dropStep{"exec_command", execArgs, execOut},
		dropStep{"write_stdin", stdinArgs, stdinOut},
	)
	if !callKept(l, "write_stdin", stdinArgs) {
		t.Error("write_stdin 同参轮询被剔除：模型无法继续读取还在跑的会话，该轮退化成散文并自称没有工具")
	}
	// 同一条轨迹里，未失败的 exec_command 同参重放此前就是放行的，不能被这次改动带坏。
	if !callKept(l, "exec_command", execArgs) {
		t.Error("exec_command 同参重放被剔除：壳层扫描的重跑是必要动作")
	}
}

// 流写入的放行只针对 stdin/stdout/stderr，不得对含 write 的持久写入开闸。
func TestToolWritesToStreamDoesNotOpenFileWrites(t *testing.T) {
	for _, n := range []string{"write_stdin", "write_stdout", "read_stderr", "WriteStdin"} {
		if !toolWritesToStream(n) {
			t.Errorf("%s 应判为流写入", n)
		}
		if toolRewritesState(n) {
			t.Errorf("%s 不该判为改写持久状态", n)
		}
	}
	for _, n := range []string{"write_file", "Write", "edit_file", "apply_patch", "delete_path", "install_deps"} {
		if toolWritesToStream(n) {
			t.Errorf("%s 不是流写入", n)
		}
		if !toolRewritesState(n) {
			t.Errorf("%s 必须仍判为改写持久状态", n)
		}
	}
	// 表外名字的既有行为不变。
	if toolRewritesState("Bash") || toolRewritesState("exec_command") {
		t.Error("Bash / exec_command 的用途判定不该被这次改动波及")
	}
}
