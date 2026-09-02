package web

import (
	"fmt"
	"strings"
	"testing"
)

// 这一组测的是同一件事的两半：预算按「实际会发出去的字节」计价，以及
// 路由器提示里的账本必须自己有界。两半都只在 agent 循环那种形状下才暴露
// ——一个 user turn 后面挂 N 对 assistant(tool_calls)/tool，而不是 N 个
// 独立的 user turn。
//
// agentLoopMessages builds the message shape a Claude Code style agent loop
// actually produces: one user turn, then N assistant(tool_calls)/tool pairs.
//
// Anthropic tool_result blocks convert to Role "tool" (protocol_compat.go:401)
// and emit no user message (protocol_compat.go:404 requires text or calls), so
// messageGroups (context_budget.go:80) puts the WHOLE loop in ONE group.
func agentLoopMessages(calls, resultBytes int) []oaiMsg {
	out := []oaiMsg{
		{Role: "system", Content: "WORKSPACE RUNTIME: gateway instruction block"},
		{Role: "user", Content: "audit the repository and fix the failing test"},
	}
	for i := 0; i < calls; i++ {
		id := fmt.Sprintf("call_%04d", i)
		out = append(out, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
			"id": id, "type": "function",
			"function": map[string]any{
				"name":      "Read",
				"arguments": fmt.Sprintf(`{"file_path":"/repo/pkg/file_%04d.go"}`, i),
			},
		}}})
		out = append(out, oaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", resultBytes)})
	}
	return out
}

// separateTurnsMessages builds N distinct user turns, each with one tool call.
// This is the shape trimHistoryForRouter's 40-group cap was written for.
func separateTurnsMessages(turns, resultBytes int) []oaiMsg {
	out := []oaiMsg{{Role: "system", Content: "WORKSPACE RUNTIME: gateway instruction block"}}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%04d", i)
		out = append(out,
			oaiMsg{Role: "user", Content: fmt.Sprintf("request number %04d", i)},
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
				"id": id, "type": "function",
				"function": map[string]any{
					"name":      "Read",
					"arguments": fmt.Sprintf(`{"file_path":"/repo/pkg/file_%04d.go"}`, i),
				},
			}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", resultBytes)},
		)
	}
	return out
}

// ---------------------------------------------------------------------------
// 整组被丢光：context_budget.go:110-117 里，最新的那一个 group 单独就超预算
// 时，keepFrom 停在 len(groups)，groups[keepFrom:] 是空的，而 :124 的
// len(out)==0 兜底又永远不会触发 —— 因为 ensureRuntimeWorkspaceInstruction
// (runtime_prompt.go:111) 在这之前必然已经塞进一条 system
// (server.go:1682)。结果是：指令和工具 schema 都在，对话没了，err 还是 nil。
// ---------------------------------------------------------------------------

func TestTrimReportsWhenTheNewestTurnAloneExceedsBudget(t *testing.T) {
	budget := configuredContextBudget()

	// Realistic Claude Code shape: repeated Read of ~40KB source files. The send
	// path compacts each result to 4000 bytes (prompt.go:29), so the prompt that
	// would actually leave the gateway is ~10x smaller than the raw history.
	const resultBytes = 40_000
	const probe = 20
	_, probeGroups := messageGroups(agentLoopMessages(probe, resultBytes), "")
	perCall := probeGroups[0].cost / probe
	threshold := budget/max(1, perCall) + 1
	t.Logf("live budget=%d | %d-byte tool results charged %d tokens each | single group exceeds budget at ~%d completed calls (round limit is %d)",
		budget, resultBytes, perCall, threshold, maxToolRounds())

	messages := agentLoopMessages(threshold, resultBytes)
	_, groups := messageGroups(messages, "")
	got, err := trimMessagesWithBudget(messages, nil, nil, "", budget)

	kept := 0
	for _, m := range got {
		if !isInstructionRole(m.Role) {
			kept++
		}
	}
	sentTokens := honestMessagesTokens(messages)
	t.Logf("groups=%d charged_cost=%d budget=%d | prompt the send path would emit = %d tokens (%.0f%% of budget) | in=%d out=%d non_instruction_kept=%d err=%v",
		len(groups), groups[0].cost, budget, sentTokens, 100*float64(sentTokens)/float64(budget),
		len(messages), len(got), kept, err)

	if err == nil && kept == 0 {
		t.Fatalf("instructions-only result: trim returned %d messages, all instructions. The user request and all %d tool results were dropped, no error reported, "+
			"while the real prompt would have used only %d of %d budget tokens", len(got), threshold, sentTokens, budget)
	}
}

func TestTrimDoesNotSilentlyReturnInstructionsAlone(t *testing.T) {
	// Minimal reachable shape: one instruction that fits, one user turn that does not.
	messages := []oaiMsg{
		{Role: "system", Content: "rule"},
		{Role: "user", Content: strings.Repeat("task ", 400)},
	}
	got, err := trimMessagesWithBudget(messages, nil, nil, "", 60)
	t.Logf("out=%#v err=%v", got, err)
	if err == nil && len(got) == 1 && got[0].Role == "system" {
		t.Fatalf("trim silently returned the system rule alone, dropping the only user request")
	}
}

// ---------------------------------------------------------------------------
// 计价错位：context_budget.go:35 按完整 content 走 serializedTokenCount，可
// 发送路径把 tool 结果压到 4000 字节 (prompt.go:29)，并把 base64 图片剥进
// Attachments (multimodal.go) —— 后者一个字节都不进文本提示。
// ---------------------------------------------------------------------------

func TestBudgetChargesOnlyToolBytesTheSendPathEmits(t *testing.T) {
	count, _ := tokenEstimator("")
	raw := strings.Repeat("y", 100_000)
	msg := oaiMsg{Role: "tool", ToolCallID: "call_1", Content: raw}

	charged := messageTokenCost(msg, "")
	sentText, _ := flattenPromptMessages([]oaiMsg{msg}, nil)
	sent := count(sentText)

	t.Logf("tool result raw=%d bytes | charged=%d tokens | actually sent=%d bytes = %d tokens | inflation=%.1fx",
		len(raw), charged, len(sentText), sent, float64(charged)/float64(sent))

	if charged > sent*2 {
		t.Fatalf("budget charges %d tokens for a tool result the send path truncates to %d tokens (%.1fx over)",
			charged, sent, float64(charged)/float64(sent))
	}
}

func imageTurn(base64Bytes int) oaiMsg {
	return oaiMsg{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what is in this screenshot"},
		map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/png;base64," + strings.Repeat("A", base64Bytes),
		}},
	}}
}

// The charge for an image must not scale with the base64 payload, because
// parseContent moves that payload into Attachments and no byte of it reaches
// the text prompt. A nominal per-attachment charge is correct; a
// proportional one is not.
func TestBudgetChargeDoesNotScaleWithStrippedBase64(t *testing.T) {
	count, _ := tokenEstimator("")
	small := messageTokenCost(imageTurn(10_000), "")
	large := messageTokenCost(imageTurn(2_000_000), "")

	sentText, attachments := flattenPromptMessages([]oaiMsg{imageTurn(2_000_000)}, nil)
	t.Logf("base64 10KB -> charged=%d tokens | base64 2MB -> charged=%d tokens | 2MB turn sends text=%q (%d tokens) + %d attachment(s)",
		small, large, sentText, count(sentText), len(attachments))

	if large != small {
		t.Fatalf("charge scales with stripped base64: %d tokens at 10KB vs %d tokens at 2MB, a %dx swing for a payload that never enters the prompt",
			small, large, large/max(1, small))
	}
}

// honestMessagesTokens measures what the send path would really emit.
func honestMessagesTokens(messages []oaiMsg) int {
	count, _ := tokenEstimator("")
	text, _ := flattenPromptMessages(messages, nil)
	return count(text)
}

// 这一条的实测收益比它的名字听起来小：在 result_bytes=4000 时只多留回 1/40
// 个 turn，在 20000/40000/100000 三档都是 0（4000 字节以下本来就不会被
// 过量计价，见下面的 toolResultPromptLimit 门槛）。留着它是因为它把「按发出
// 字节计价」这条不变量钉住了，不是因为它救回了很多轮对话。
func TestInflatedAccountingDoesNotDropTurnsThatFit(t *testing.T) {
	const budget = 20000
	worst := 0
	for _, resultBytes := range []int{4_000, 20_000, 40_000, 100_000} {
		messages := separateTurnsMessages(40, resultBytes)
		kept, err := trimMessagesWithBudget(messages, nil, nil, "", budget)
		if err != nil {
			t.Fatalf("result_bytes=%d unexpected error: %v", resultBytes, err)
		}
		_, keptGroups := messageGroups(kept, "")

		instructions, groups := messageGroups(messages, "")
		used := honestMessagesTokens(instructions)
		honest := 0
		for i := len(groups) - 1; i >= 0; i-- {
			c := honestMessagesTokens(groups[i].messages)
			if used+c > budget {
				break
			}
			used += c
			honest++
		}
		t.Logf("result_bytes=%6d budget=%d | charged accounting keeps %2d/40 turns | honest accounting keeps %2d/40 turns | unnecessarily dropped=%d",
			resultBytes, budget, len(keptGroups), honest, honest-len(keptGroups))
		// Only results past the 4000-byte send-path truncation can be
		// over-charged. Below it, charged and honest legitimately differ by the
		// per-message protocol constants, which the honest reference omits.
		if resultBytes > toolResultPromptLimit && honest-len(keptGroups) > worst {
			worst = honest - len(keptGroups)
		}
	}
	if worst > 0 {
		t.Fatalf("inflated accounting drops up to %d conversation turns that would have fit the real prompt", worst)
	}
}

// ---------------------------------------------------------------------------
// 40 组的帽子管不住整条提示：router_history_trim.go:39 把历史截到 40 组，可
// server.go:1860 把 routerPromptMessages(...) 和 ledger.RouterContext() 拼成
// 同一个提示串，而后者自己没有任何上界。
// ---------------------------------------------------------------------------

func TestRouterLedgerStopsGrowingWithConversationLength(t *testing.T) {
	t.Setenv("M365_ROUTER_MAX_MESSAGES", "40")

	// (a) Agent-loop shape: the cap is inert, the loop is a single group.
	loop := agentLoopMessages(200, 2000)
	_, loopGroups := messageGroups(loop, "")
	loopHistory := routerPromptMessages(loop)
	t.Logf("agent loop with 200 tool calls -> %d group(s); 40-group cap trims nothing; router history = %d bytes",
		len(loopGroups), len(loopHistory))

	// (b) Separate-turn shape: the cap does trim, then the ledger re-injects.
	turns := separateTurnsMessages(200, 2000)
	history := routerPromptMessages(turns)
	ledgerCtx := buildAgentLedger(turns).RouterContext()
	total := len(history) + 1 + len(ledgerCtx)

	keptTurns := 0
	for i := 0; i < 200; i++ {
		if strings.Contains(history, fmt.Sprintf("request number %04d", i)) {
			keptTurns++
		}
	}
	evidenced := strings.Count(ledgerCtx, `"id":"call_`)
	t.Logf("separate turns: history keeps %d/200 turns (%d bytes) | RouterContext re-injects %d/200 calls (%d bytes) | combined router prompt = %d bytes",
		keptTurns, len(history), evidenced, len(ledgerCtx), total)

	// The property that matters is that the ledger half stops growing with
	// conversation length, so the capped history half actually caps the prompt.
	at200 := len(buildAgentLedger(separateTurnsMessages(200, 2000)).RouterContext())
	at2000 := len(buildAgentLedger(separateTurnsMessages(2000, 2000)).RouterContext())
	t.Logf("RouterContext at 200 turns = %d bytes | at 2000 turns = %d bytes", at200, at2000)
	if at2000 > at200*2 {
		t.Fatalf("RouterContext still tracks conversation length: %d bytes at 200 calls vs %d bytes at 2000; the 40-group history cap therefore bounds nothing",
			at200, at2000)
	}

	// And the newest evidence must always survive the bound.
	if !strings.Contains(ledgerCtx, "call_0199") {
		t.Fatalf("newest completed call is missing from RouterContext; the bound evicted the evidence that prevents redoing the last step")
	}
}

// ---------------------------------------------------------------------------
// 账本本身无界：agent_ledger.go 把每一条 completed toolEvidence 都序列化进去，
// 每条带一个压到 4000 字节的 Result 和一个完全没有上界的 Arguments。而这串东西
// 会被重新拼进每一个 router / retry / repair / answer 提示。
// ---------------------------------------------------------------------------

func TestRouterContextStaysBoundedAtManyCompletedCalls(t *testing.T) {
	for _, n := range []int{50, 200, 500} {
		ledger := buildAgentLedger(agentLoopMessages(n, 4000))
		ctx := ledger.RouterContext()
		retained := 0
		for _, e := range ledger.Completed {
			retained += len(e.ID) + len(e.Name) + len(e.Arguments) + len(e.Result)
		}
		t.Logf("completed=%3d | retained in ledger = %8d bytes (%d bytes/call) | RouterContext = %8d bytes | re-serialized into EVERY router prompt",
			len(ledger.Completed), retained, retained/max(1, len(ledger.Completed)), len(ctx))
	}

	ctx := buildAgentLedger(agentLoopMessages(500, 4000)).RouterContext()
	const cap = 64 << 10
	if len(ctx) > cap {
		t.Fatalf("RouterContext is %d bytes at 500 completed calls (cap would be %d): every router, retry, repair and answer prompt carries all of it",
			len(ctx), cap)
	}
}
