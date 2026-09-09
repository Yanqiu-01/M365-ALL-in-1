package web

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactToolResultKeepsHeadTailAndError(t *testing.T) {
	s := "start\n" + strings.Repeat("progress line\n", 1000) + "ERROR: build failed\nexit code 1"
	// 完整版省略标记（行号锚定 + 补读指引）只在预算充裕（limit≥1200）时出现。
	got := compactToolResult(s, 4000)
	if len(got) > 4400 || !strings.Contains(got, "start") || !strings.Contains(got, "ERROR: build failed") || !strings.Contains(got, "exit code 1") || !strings.Contains(got, "gateway truncation") || !strings.Contains(got, "NOT the upstream") {
		t.Fatalf("bad compact result: %d %q", len(got), got)
	}
	// 小预算退化为短版标记，标记本身不吃掉预算。
	short := compactToolResult(s, 300)
	if len(short) > 320 || !strings.Contains(short, "[truncated ") {
		t.Fatalf("small-budget compact: %d %q", len(short), short)
	}
}

func TestAgentLedgerDetectsRepeatedFailure(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "exit code 1: failed"},
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedFailure {
		t.Fatalf("expected repeated failure: %+v", l)
	}
	if !strings.Contains(l.RouterContext(), "change strategy") {
		t.Fatal(l.RouterContext())
	}
}

func TestAgentLedgerEvidenceAndUniqueCallIDs(t *testing.T) {
	a := scopedCallID("run", "{}", 0, "turn-a")
	b := scopedCallID("run", "{}", 0, "turn-b")
	if a == b {
		t.Fatal("call IDs collide across turns")
	}
	l := buildAgentLedger([]oaiMsg{{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "create", "arguments": "{}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "created"}})
	if len(l.Completed) != 1 || !strings.Contains(l.RouterContext(), "c1") {
		t.Fatalf("missing evidence: %+v", l)
	}
}

func TestAgentLedgerDetectsRepeatedCallAndRoundLimit(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": "{\"id\":1}"}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "still pending"})
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedCall || l.ToolRounds != 4 {
		t.Fatalf("loop not detected: %+v", l)
	}
	if err := l.CanContinue(3); err == nil {
		t.Fatal("expected round limit")
	}
}

func TestActiveMessagesIgnoresOlderToolHistory(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "old", "arguments": "{}"}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "done"},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "continue with a new model"})
	full := buildAgentLedger(msgs)
	active := buildAgentLedger(activeMessages(msgs))
	if full.ToolRounds < 20 {
		t.Fatalf("expected full history tools, got %d", full.ToolRounds)
	}
	if active.ToolRounds != 0 {
		t.Fatalf("new user turn should reset round limit scope, got %d", active.ToolRounds)
	}
	if err := active.CanContinue(16); err != nil {
		t.Fatalf("new user turn blocked by old history: %v", err)
	}
}

func TestCompletionGuardRejectsPendingAndUnsupportedSuccess(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "p1", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}},
		}},
	})
	if completionEvidenceAllows("Deployment completed successfully", l) {
		t.Fatal("pending action allowed as complete")
	}
}

// 契约变更：ledger 为空时不再按关键词拦截。
//
// 原断言是「Installed, started, and verified successfully」+ 空 ledger 必须被拒 ——
// 它编码的正是本次修掉的缺陷：空 ledger 说明这一轮没有任何工具调用，纯文本启发式
// 分不清「模型空口宣称成功」和「答复里恰好出现这些常见词」，而 server.go 会据此把
// 整段答复替换成罐头话。见 completionEvidenceAllows 的注释与下面三条实测用例。
//
// 保住的那半条断言仍然成立：诚实的「无法确认」在空 ledger 下必须放行。
func TestCompletionGuardAllowsProseWhenNoToolWasCalled(t *testing.T) {
	empty := buildAgentLedger(nil)
	if !completionEvidenceAllows("Installed, started, and verified successfully", empty) {
		t.Error("no tool call means no external action to verify; the answer must not be replaced")
	}
	if !completionEvidenceAllows("I cannot confirm completion because no tool results were returned.", empty) {
		t.Fatal("honest incomplete response rejected")
	}
}

// 三条对运行中网关的实测用例，请求里一个工具都没声明。
// 前两条曾表现为「同一个翻译任务，答复里有没有某个过去分词决定它是否被整段替换」。
func TestCompletionGuardKeepsLiveTranslationAnswers(t *testing.T) {
	noTools := buildAgentLedger(nil)
	// 1. 翻译「数据已经写入磁盘」：答复含 "written"，此前被整段换成罐头话。
	if !completionEvidenceAllows("The data has been written to the disk.", noTools) {
		t.Error(`a translation containing "written" was replaced by a sentence the model never wrote`)
	}
	// 2. 翻译「磁盘正在接收数据」：不含关键词，此前就正常返回 —— 回归基线。
	if !completionEvidenceAllows("The disk is receiving data.", noTools) {
		t.Error("an answer with no listed word must stay allowed")
	}
	// 3. 同一条答复，历史里有一条已完成的工具结果：此前走 Completed>0 分支正常返回，
	//    修复后必须仍然正常返回。
	withEvidence := buildAgentLedger([]oaiMsg{
		toolCallMsg("call_t1", "read_file", `{"path":"disk.md"}`),
		{Role: "tool", ToolCallID: "call_t1", Content: "disk.md: 1 line"},
	})
	if len(withEvidence.Completed) != 1 {
		t.Fatalf("test setup: Completed=%d want 1", len(withEvidence.Completed))
	}
	if !completionEvidenceAllows("The data has been written to the disk.", withEvidence) {
		t.Error("an answer backed by a completed tool result must stay allowed")
	}
}

// 普通散文里出现这些词的其它形态同样不得触发替换。
func TestCompletionGuardDoesNotFireOnOrdinaryProse(t *testing.T) {
	noTools := buildAgentLedger(nil)
	for _, answer := range []string{
		"The report is written in Markdown, and the appendix was created by the previous author.",
		"Git 的 `installed` 标记只是元数据；`ran` 是 run 的过去式。",
		"A deployed model and a deleted branch are different things; neither has been verified here.",
		"This function returns successfully when the loop completed.",
	} {
		if !completionEvidenceAllows(answer, noTools) {
			t.Errorf("ordinary prose replaced by a canned sentence: %q", answer)
		}
	}
}

// 两条仍然合法的守卫：欠着工具结果时宣称完成，以及有工具证据却自称无法确认。
func TestCompletionGuardStillHoldsWhereTheLedgerHasEvidence(t *testing.T) {
	pending := buildAgentLedger([]oaiMsg{toolCallMsg("call_p1", "deploy", `{}`)})
	if len(pending.Pending) != 1 {
		t.Fatalf("test setup: Pending=%d want 1", len(pending.Pending))
	}
	if completionEvidenceAllows("The disk is receiving data.", pending) {
		t.Error("a turn that still owes a tool result cannot report completion")
	}
	completed := buildAgentLedger([]oaiMsg{
		toolCallMsg("call_c1", "run_tests", `{}`),
		{Role: "tool", ToolCallID: "call_c1", Content: "3 passed"},
	})
	if completionEvidenceAllows("I cannot confirm the tests ran; no tool result was returned.", completed) {
		t.Error("an answer that denies evidence the ledger holds must still be caught")
	}
}

func TestCompactRouterEvidenceBoundsSingleHugeArguments(t *testing.T) {
	completed := []toolEvidence{{
		ID:        "call_1",
		Name:      "run_shell",
		Arguments: strings.Repeat("x", routerEvidenceMaxBytes),
		Result:    "ok",
	}}
	got, dropped := compactRouterEvidence(completed)
	if dropped != 0 || len(got) != 1 {
		t.Fatalf("got %d entries, dropped %d; want one retained entry", len(got), dropped)
	}
	if len(mustJSON(got)) > routerEvidenceMaxBytes {
		t.Fatalf("single retained entry escaped router evidence budget: %d bytes", len(mustJSON(got)))
	}
	if !strings.Contains(got[0].Arguments, "gateway truncation for prompt budget") {
		t.Fatalf("huge arguments were not compacted: %d bytes", len(got[0].Arguments))
	}
}
