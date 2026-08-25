package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 脚本化模型：驱动 runBenchTask 的工具循环而不触达上游。
// ---------------------------------------------------------------------------

type scriptedTurn struct {
	content string
	calls   []any
}

func benchToolCall(id, name, arguments string) any {
	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": arguments},
	}
}

type scriptedModel struct {
	turns    int
	captured [][]map[string]any
}

// installScriptedBenchChat 把 benchChatCall 换成脚本化模型：script(turn) 给出
// 第 turn 轮（从 1 起）的回复。返回值记录实际发生的轮数与每轮收到的消息，
// 用于断言「提前结束」与「提示注入」。
func installScriptedBenchChat(t *testing.T, script func(turn int) scriptedTurn) *scriptedModel {
	t.Helper()
	model := &scriptedModel{}
	previous := benchChatCall
	t.Cleanup(func() { benchChatCall = previous })
	benchChatCall = func(_ context.Context, _ *Server, _, _ string, messages []map[string]any) (map[string]any, time.Duration, error) {
		model.turns++
		model.captured = append(model.captured, append([]map[string]any(nil), messages...))
		turn := script(model.turns)
		message := map[string]any{"content": turn.content}
		if len(turn.calls) > 0 {
			message["tool_calls"] = turn.calls
		}
		return map[string]any{"choices": []any{map[string]any{"message": message}}}, time.Millisecond, nil
	}
	return model
}

// toolContents 取出某一轮收到的全部 tool 消息正文，按出现顺序返回。
func (m *scriptedModel) toolContents(turn int) []string {
	if turn < 1 || turn > len(m.captured) {
		return nil
	}
	var out []string
	for _, message := range m.captured[turn-1] {
		if message["role"] == "tool" {
			out = append(out, fmt.Sprint(message["content"]))
		}
	}
	return out
}

// convergenceCodingTask 是一个最小编程任务：a.py 内容为 "good" 即满分。
func convergenceCodingTask() benchTask {
	return benchTask{
		ID: "converge-coding", Title: "收敛探针", Detail: "probe", Category: "coding",
		Files: map[string]string{"a.py": "orig"},
		Grader: func(files map[string]string) (int, int, []string) {
			if files["a.py"] == "good" {
				return 3, 3, nil
			}
			return 1, 3, []string{"未修复"}
		},
	}
}

const writeGood = `{"path":"a.py","content":"good"}`
const writeBroken = `{"path":"a.py","content":"broken"}`

// ---------------------------------------------------------------------------
// a) 编程任务在「测试通过 + Grader 满分」后确实提前 break。
// ---------------------------------------------------------------------------

// 实测 8 个任务全部精确跑满 benchMaxSteps=14：debug 第 5 步就 TESTS PASSED
// 6/6，之后又调了 6 次 run_tests。闭环达成后必须停止推进。
func TestRunBenchTaskExitsAfterClosureAchieved(t *testing.T) {
	server := benchmarkHTTPServer()
	task := convergenceCodingTask()

	model := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		switch turn {
		case 1:
			return scriptedTurn{content: "修复", calls: []any{benchToolCall("c1", "write_file", writeGood)}}
		case 2:
			return scriptedTurn{content: "验证", calls: []any{benchToolCall("c2", "run_tests", "{}")}}
		}
		// 闭环已达成，模型仍想继续读文件 —— 正是日志里的空转形态。
		// 早退生效时这些轮次不应发生。
		return scriptedTurn{content: "再看看", calls: []any{benchToolCall("cx", "read_file", `{"path":"a.py"}`)}}
	})

	result := server.runBenchTask(context.Background(), task, "m", "high")

	if model.turns != 2 {
		t.Fatalf("上游轮数=%d，want 2：闭环达成后应立即结束（未早退会跑满 %d 步）", model.turns, benchMaxSteps)
	}
	if result.Steps != 2 {
		t.Errorf("工具执行数=%d want 2", result.Steps)
	}
	if result.Steps >= benchMaxSteps {
		t.Errorf("步数=%d 未低于上限 %d", result.Steps, benchMaxSteps)
	}
	if result.Status != "done" || result.NetScore != 1 {
		t.Errorf("status=%q netScore=%v：早退不得影响得分", result.Status, result.NetScore)
	}
	logs := strings.Join(server.benchmark.snapshot().Log, "\n")
	if !strings.Contains(logs, "闭环达成") {
		t.Errorf("缺少早退日志：%s", logs)
	}
}

// 推理任务没有 run_tests 闭环要求：Grader 满分即应早退，不得强求 testsPass。
func TestRunBenchTaskExitsOnPerfectReasoningSnapshot(t *testing.T) {
	server := benchmarkHTTPServer()
	task := benchTask{
		ID: "converge-reasoning", Title: "推理收敛", Detail: "probe", Category: "reasoning",
		Grader: func(files map[string]string) (int, int, []string) {
			if files["out.json"] == "done" {
				return 2, 2, nil
			}
			return 0, 2, []string{"未产出"}
		},
	}

	model := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		if turn == 1 {
			return scriptedTurn{calls: []any{benchToolCall("c1", "write_file", `{"path":"out.json","content":"done"}`)}}
		}
		return scriptedTurn{calls: []any{benchToolCall("cx", "list_files", "{}")}}
	})

	result := server.runBenchTask(context.Background(), task, "m", "high")

	if model.turns != 1 {
		t.Fatalf("上游轮数=%d want 1：推理任务满分即应早退", model.turns)
	}
	if result.NetScore != 1 || result.TestRuns != 0 {
		t.Errorf("netScore=%v testRuns=%d：推理任务不应被 run_tests 闭环牵连", result.NetScore, result.TestRuns)
	}
}

// 编程任务即使 Grader 满分，也必须真的跑过并通过 run_tests 才早退 ——
// 与 benchCodingLoopPrompt 的闭环要求保持一致。
func TestCodingClosureRequiresActualTestRunBeforeEarlyExit(t *testing.T) {
	server := benchmarkHTTPServer()
	task := convergenceCodingTask()
	workspace := newBenchWorkspace(task)

	if _, err := workspace.execute("write_file", map[string]any{"path": "a.py", "content": "good"}); err != nil {
		t.Fatal(err)
	}
	if server.benchTaskConverged(task, workspace, 1) {
		t.Error("从未调用 run_tests 就早退，会绕过编程闭环要求")
	}
	if _, err := workspace.execute("run_tests", nil); err != nil {
		t.Fatal(err)
	}
	if !server.benchTaskConverged(task, workspace, 2) {
		t.Error("测试已通过且快照满分，应判定闭环达成")
	}
}

// ---------------------------------------------------------------------------
// b) 「测试通过但代码随后被改坏」不得早退（正确性关键）。
// ---------------------------------------------------------------------------

// workspace.testsPass 只记录「最后一次 run_tests 时」的结论，write_file 不会
// 重置它。若只看 testsPass，模型跑通测试后又改坏代码时会被误判为已完成。
func TestNoEarlyExitWhenCodeBrokenAfterTestsPassed(t *testing.T) {
	server := benchmarkHTTPServer()
	task := convergenceCodingTask()
	workspace := newBenchWorkspace(task)

	for _, step := range []struct {
		name string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "a.py", "content": "good"}},
		{"run_tests", nil},
		{"write_file", map[string]any{"path": "a.py", "content": "broken"}},
	} {
		if _, err := workspace.execute(step.name, step.args); err != nil {
			t.Fatal(err)
		}
	}

	// 前提：testsPass 仍为 true，只看它就会误判。
	if testsPass, runs := workspace.testStatus(); !testsPass || runs != 1 {
		t.Fatalf("前提失效：testsPass=%t runs=%d", testsPass, runs)
	}
	if server.benchTaskConverged(task, workspace, 3) {
		t.Fatal("代码已被改坏仍早退：早退必须以 Grader 对当前快照的判定为准")
	}

	// 改回正确后才允许早退。
	if _, err := workspace.execute("write_file", map[string]any{"path": "a.py", "content": "good"}); err != nil {
		t.Fatal(err)
	}
	if !server.benchTaskConverged(task, workspace, 4) {
		t.Error("修回正确后应判定闭环达成")
	}
}

// 端到端：模型在同一步里改对、跑测试、又改坏，循环必须继续推进给它机会改回来。
func TestRunBenchTaskKeepsGoingAfterRegressionInLoop(t *testing.T) {
	server := benchmarkHTTPServer()
	task := convergenceCodingTask()

	model := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		switch turn {
		case 1:
			return scriptedTurn{content: "改对并验证，随后误改", calls: []any{
				benchToolCall("c1", "write_file", writeGood),
				benchToolCall("c2", "run_tests", "{}"),
				benchToolCall("c3", "write_file", writeBroken),
			}}
		case 2:
			return scriptedTurn{content: "改回", calls: []any{benchToolCall("c4", "write_file", writeGood)}}
		}
		return scriptedTurn{content: "空转", calls: []any{benchToolCall("cx", "read_file", `{"path":"a.py"}`)}}
	})

	result := server.runBenchTask(context.Background(), task, "m", "high")

	if model.turns < 2 {
		t.Fatalf("上游轮数=%d：代码被改坏时不得早退", model.turns)
	}
	if model.turns != 2 {
		t.Errorf("上游轮数=%d want 2：改回正确后应立即早退", model.turns)
	}
	if result.NetScore != 1 {
		t.Errorf("netScore=%v want 1", result.NetScore)
	}
}

// 受保护输入被篡改时不得早退：那不是闭环达成，剩余步数要留给模型改回来。
func TestNoEarlyExitWhileProtectedInputTampered(t *testing.T) {
	server := benchmarkHTTPServer()
	task := benchTask{
		ID: "protect-converge", Category: "coding",
		Files:     map[string]string{"a.py": "orig", "fixture.txt": "keep"},
		Protected: map[string]string{"fixture.txt": "keep"},
		Grader:    func(map[string]string) (int, int, []string) { return 1, 1, nil },
	}
	workspace := newBenchWorkspace(task)
	if _, err := workspace.execute("run_tests", nil); err != nil {
		t.Fatal(err)
	}
	if !server.benchTaskConverged(task, workspace, 1) {
		t.Fatal("前提失效：未篡改时应可早退")
	}
	if _, err := workspace.execute("write_file", map[string]any{"path": "fixture.txt", "content": "hacked"}); err != nil {
		t.Fatal(err)
	}
	if server.benchTaskConverged(task, workspace, 2) {
		t.Error("受保护输入被篡改仍早退")
	}
}

// ---------------------------------------------------------------------------
// c) 连续重复的同参同结果调用触发提示注入，且不影响评分。
// ---------------------------------------------------------------------------

// 指纹只认「连续」重复：中间夹了别的调用、或结果发生变化都要重置计数。
func TestBenchRepeatTrackerCountsOnlyConsecutiveIdentical(t *testing.T) {
	tracker := &benchRepeatTracker{}
	if got := tracker.observe("read_file", `{"path":"a"}`, "x"); got != 1 {
		t.Fatalf("首次=%d want 1", got)
	}
	if got := tracker.observe("read_file", `{"path":"a"}`, "x"); got != 2 {
		t.Fatalf("第二次=%d want 2", got)
	}
	// 结果变化 → 重置。
	if got := tracker.observe("read_file", `{"path":"a"}`, "y"); got != 1 {
		t.Fatalf("结果变化后=%d want 1", got)
	}
	// 换工具 → 重置。
	tracker.observe("read_file", `{"path":"a"}`, "y")
	if got := tracker.observe("list_files", "{}", "z"); got != 1 {
		t.Fatalf("换工具后=%d want 1", got)
	}
	if benchRepeatHintThreshold < 2 || benchRepeatHintThreshold >= benchMaxSteps {
		t.Errorf("阈值 %d 不合理（须 >=2 且小于步数上限 %d）", benchRepeatHintThreshold, benchMaxSteps)
	}
}

// shift 任务第 9-14 步连续 6 次 read_file 读同一个 schedule.json，返回内容
// 一字不差。第 benchRepeatHintThreshold 次起必须收到「停止空转」提示，
// 但工具本身仍要执行（拒绝执行会打断推理链），得分口径也不受影响。
func TestRepeatedIdenticalCallsGetHintWithoutAffectingScore(t *testing.T) {
	server := benchmarkHTTPServer()
	task := benchTask{
		ID: "repeat-probe", Title: "重复调用", Detail: "probe", Category: "reasoning",
		Files:  map[string]string{"schedule.json": "{}"},
		Grader: func(map[string]string) (int, int, []string) { return 1, 3, []string{"未产出"} },
	}

	const sameArgs = `{"path":"schedule.json"}`
	model := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		if turn <= 4 {
			return scriptedTurn{content: "再读一次", calls: []any{benchToolCall("c", "read_file", sameArgs)}}
		}
		return scriptedTurn{content: "最终回答"}
	})

	result := server.runBenchTask(context.Background(), task, "m", "high")

	if model.turns != 5 {
		t.Fatalf("上游轮数=%d want 5", model.turns)
	}
	// 第 5 轮收到的 tool 消息即前 4 次调用的回传内容。
	contents := model.toolContents(5)
	if len(contents) != 4 {
		t.Fatalf("tool 消息数=%d want 4", len(contents))
	}
	for i, content := range contents {
		hinted := strings.Contains(content, benchRepeatHint)
		wantHint := i+1 >= benchRepeatHintThreshold
		if hinted != wantHint {
			t.Errorf("第 %d 次重复 hinted=%t want %t：内容=%s", i+1, hinted, wantHint, trimForLog(content))
		}
		// 工具仍被执行：结果正文必须保留。
		if !strings.Contains(content, "schedule.json") {
			t.Errorf("第 %d 次调用的结果正文丢失：%s", i+1, trimForLog(content))
		}
	}
	if result.Steps != 4 {
		t.Errorf("工具执行数=%d want 4：提示注入不得拒绝执行", result.Steps)
	}
	// 提示注入只改回给模型的文本，不碰评分。
	wantPassed, wantTotal, _ := task.Grader(map[string]string{"schedule.json": "{}"})
	if result.Passed != wantPassed || result.Total != wantTotal {
		t.Errorf("得分=%d/%d want %d/%d", result.Passed, result.Total, wantPassed, wantTotal)
	}
	if result.NetScore != float64(wantPassed)/float64(wantTotal) {
		t.Errorf("netScore=%v want %v", result.NetScore, float64(wantPassed)/float64(wantTotal))
	}
}

// ---------------------------------------------------------------------------
// 得分口径不变：同一份最终快照，早退与跑满步数必须得到同一个分数。
// ---------------------------------------------------------------------------

// 早退只是「提前停止推进」，收尾的 gradeBenchTask 校验与 Grader 打分完全不变。
// 这里让同一份最终快照分别经由「2 轮早退」与「一路磨到步数上限」产生，
// 断言两者的 floor/passed/total/netScore 完全一致。
func TestEarlyExitPreservesScoringForIdenticalFinalSnapshot(t *testing.T) {
	task := convergenceCodingTask()

	// 早退路径：改对 + 跑测试，之后不再推进。
	early := benchmarkHTTPServer()
	earlyModel := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		switch turn {
		case 1:
			return scriptedTurn{calls: []any{benchToolCall("c1", "write_file", writeGood)}}
		case 2:
			return scriptedTurn{calls: []any{benchToolCall("c2", "run_tests", "{}")}}
		}
		return scriptedTurn{calls: []any{benchToolCall("cx", "read_file", `{"path":"a.py"}`)}}
	})
	earlyResult := early.runBenchTask(context.Background(), task, "m", "high")

	// 满步路径：同一份最终快照，但模型在中途反复改坏再改回，直到步数耗尽。
	// 末轮必须以正确内容收尾，最终快照与早退路径完全相同。
	burn := benchmarkHTTPServer()
	burnModel := installScriptedBenchChat(t, func(turn int) scriptedTurn {
		if turn == 1 {
			return scriptedTurn{calls: []any{benchToolCall("c1", "run_tests", "{}")}}
		}
		if turn >= benchMaxSteps {
			return scriptedTurn{calls: []any{benchToolCall("cz", "write_file", writeGood)}}
		}
		if turn%2 == 0 {
			return scriptedTurn{calls: []any{benchToolCall("cb", "write_file", writeBroken)}}
		}
		return scriptedTurn{calls: []any{benchToolCall("cg", "read_file", `{"path":"a.py"}`)}}
	})
	burnResult := burn.runBenchTask(context.Background(), task, "m", "high")

	if earlyModel.turns != 2 {
		t.Fatalf("早退路径轮数=%d want 2", earlyModel.turns)
	}
	if burnModel.turns != benchMaxSteps {
		t.Fatalf("满步路径轮数=%d want %d", burnModel.turns, benchMaxSteps)
	}

	earlySnapshot, burnSnapshot := benchMustJSON(t, benchScoringInputs(task, earlyResult)), benchMustJSON(t, benchScoringInputs(task, burnResult))
	if earlySnapshot != burnSnapshot {
		t.Fatalf("前提失效：两条路径的最终得分输入不一致\n%s\n%s", earlySnapshot, burnSnapshot)
	}
	if earlyResult.Floor != burnResult.Floor ||
		earlyResult.Passed != burnResult.Passed ||
		earlyResult.Total != burnResult.Total ||
		earlyResult.NetScore != burnResult.NetScore ||
		earlyResult.TestsPass != burnResult.TestsPass {
		t.Errorf("得分口径改变：早退=%d/%d floor=%d net=%v testsPass=%t，满步=%d/%d floor=%d net=%v testsPass=%t",
			earlyResult.Passed, earlyResult.Total, earlyResult.Floor, earlyResult.NetScore, earlyResult.TestsPass,
			burnResult.Passed, burnResult.Total, burnResult.Floor, burnResult.NetScore, burnResult.TestsPass)
	}
	if earlyResult.NetScore != 1 {
		t.Errorf("netScore=%v want 1", earlyResult.NetScore)
	}
	// 早退省下的上游轮次：这正是要消除的空转。
	if earlyModel.turns >= burnModel.turns {
		t.Errorf("早退未省下轮次：%d vs %d", earlyModel.turns, burnModel.turns)
	}
}

// taskSnapshotOf 提取参与最终评分的字段，用于比较两条路径的评分输入。
func benchScoringInputs(task benchTask, result benchTaskResult) map[string]any {
	return map[string]any{
		"category": task.Category,
		"passed":   result.Passed,
		"total":    result.Total,
		"failures": result.Failures,
	}
}

func benchMustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
