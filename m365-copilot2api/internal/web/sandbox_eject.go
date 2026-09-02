package web

import (
	"context"
	"encoding/json"
	"log"
	"m365-copilot2api/internal/chathub"
	"runtime"
	"strings"
)

// A finished answer drifts away from the caller's machine in two ways that no
// amount of prompt injection reliably prevents: it reports that the caller's
// tools do not exist, or it narrates a hosted execution environment instead of
// calling one of those tools. Both are classified from the completed text, so
// the correction can only run after generation -- which is also why the
// streaming path has to buffer instead of flushing fragments as they arrive.
// SSE has no rewind: a denial the client already rendered cannot be unsaid.

// driftCorrection pairs a detector with the re-ask that answers it.
type driftCorrection struct {
	stage    string
	detects  func(string) bool
	instruct func(userRequest string) string
}

func driftCorrections() []driftCorrection {
	return []driftCorrection{
		{stage: "tool-eject", detects: isToolRefusal, instruct: toolRefusalCorrection},
		{stage: "sandbox-eject", detects: isSandboxHallucination, instruct: sandboxDriftCorrection},
	}
}

// ejectRequest is a struct rather than nine positional arguments because both
// call sites pass the same set, and silently transposing ConversationID with
// SessionID would route the correction into the wrong cloud conversation.
type ejectRequest struct {
	AccountID   string
	Account     chathub.Account
	Tools       []chathub.Tool
	ToolChoice  any
	Text        string
	UserRequest string
	Tone        string
	Attachments []chathub.Attachment
	// ConversationID and SessionID must identify the conversation the answer
	// came from. A correction sent with empty IDs opens a *new* cloud chat that
	// sees only the correction sentence: it has no request to act on, cannot
	// tell which tool was wanted, and leaves a stray conversation behind.
	// Callers resolve these as firstNonEmpty(body ID, result ID) -- a first turn
	// carries none on the body but the result always reports them.
	ConversationID string
	SessionID      string
}

// correctionChat is a narrow seam for the eject regression tests, matching the
// routerFailoverChat pattern. Production delegates straight to the
// account-aware ChatHub path.
var correctionChat = func(ctx context.Context, s *Server, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	return s.chatWithAccount(ctx, accountID, account, request)
}

// correctionRequest builds the turn the correction is actually sent as.
//
// Tools and ToolChoice have to ride along, and that is the whole point of the
// round. chathub's toolProtocolPrompt emits the <tools> block only when the
// tool list is non-empty and the choice is not "none"; with an empty list it
// falls through to environmentPrompt() alone, which tells the model to say so
// directly when no tool is wired up for the task. So a correction sent without
// tools instructs the model to "call the tool that fits the request" while the
// model can see no tools at all -- and the prompt it *can* see sanctions exactly
// the denial this round exists to repair. The correction could not succeed on
// its own failure mode.
//
// 纠正轮必须带上原请求的 tools 与 tool_choice。工具列表为空时 chathub 不发
// <tools> 段，模型看不到任何可调工具，却被要求「调用合适的工具」；它能看到的那段
// 环境说明还恰好允许它回答「没有工具可用」—— 正是这一轮要修的那种答复。
// tool_choice 同样要原样传：auto / required / none 决定了这一轮是否允许调用，
// 丢掉它等于让纠正轮按默认策略走，与原请求不一致。
func correctionRequest(req ejectRequest, instruction string) chathub.Request {
	return chathub.Request{
		Text:        instruction,
		Tone:        req.Tone,
		Attachments: req.Attachments,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		// 纠正轮必须留在同一个云端对话里：另起对话等于把原始请求的上下文
		// 丢掉，模型只看到一句纠正，既无从判断该调哪个工具，也会在对话池里
		// 多留一条记录。
		ConversationID: req.ConversationID,
		SessionID:      req.SessionID,
	}
}

// correctSandboxDrift re-asks the model when the delivered answer denied the
// caller's tools or described a hosted sandbox in place of calling one.
//
// It returns a replacement result and true only when a correction round came
// back with text that no longer trips the detector. On a transport failure, or
// when the retry drifts the same way again, the original answer stands: a
// second denial is no improvement on the first, and swapping it in would cost
// the request whatever partial content it already had.
func (s *Server) correctSandboxDrift(ctx context.Context, req ejectRequest) (chathub.Result, bool) {
	if len(req.Tools) == 0 || strings.TrimSpace(req.Text) == "" {
		return chathub.Result{}, false
	}
	// tool_choice:"none" 是调用方明确禁止调用工具。此时模型说「这件事需要
	// 一个我现在不能调的工具」是正确回答，而它恰好会命中 isToolRefusal。
	// 没有这道门槛，纠正轮会去要求模型调用调用方刚刚禁止的工具，而且一旦
	// 那轮回来不再命中检测器，还会用它顶掉本来合规的回答。
	if !toolChoiceAllowsAnyCall(req.ToolChoice) {
		return chathub.Result{}, false
	}
	current := req.Text
	corrected := chathub.Result{}
	replaced := false
	// Sequential, and each stage re-reads the text the previous one produced: a
	// tool-refusal correction that comes back narrating a sandbox still needs
	// the second round.
	for _, correction := range driftCorrections() {
		if !correction.detects(current) {
			continue
		}
		log.Printf("[%s] answer drifted off the caller's tools, retrying with a correction round", correction.stage)
		res, err := correctionChat(ctx, s, req.AccountID, req.Account, correctionRequest(req, correction.instruct(req.UserRequest)))
		if err != nil {
			log.Printf("[%s] correction round failed: %v", correction.stage, err)
			continue
		}
		// 先看纠正轮有没有真的发出一次已声明工具的调用。发出了就是成功，
		// 不管它的散文写成什么样 —— 检测器的模式表里本来就有
		// "I can run that for you" / "I'll run that" / "let me run that"，
		// 而这些恰恰是模型「即将调用工具」时最自然的开场白。拿同一张表去
		// 评判自己的纠正结果，会把带着有效调用的那一轮判成又漂了，然后把
		// 先前扣住的那句拒绝发给客户端。
		if len(correctionToolCalls(res.Text, req)) == 0 && correction.detects(res.Text) {
			log.Printf("[%s] correction round drifted again; keeping the original answer", correction.stage)
			continue
		}
		// Keep the answer turn's conversation identity. The correction normally
		// rides the same conversation, but if it had to open its own then the
		// binding must still point at the one holding the request.
		res.ConversationID = firstNonEmpty(req.ConversationID, res.ConversationID)
		res.SessionID = firstNonEmpty(req.SessionID, res.SessionID)
		corrected, replaced = res, true
		current = res.Text
	}
	return corrected, replaced
}

// ejectDelivery 算出 eject 路径最终发给客户端的正文。rejected 是 schema 校验
// 淘汰掉的调用数（此时已确定没有一个调用可派发，否则不会走到这里）。
//
// 契约：corrected 去掉空白后非空时，返回值一定非空。eject 路径已经额外花了
// 一轮请求，到这一步再交出一个空的 assistant 轮是最差的结果 —— 客户端既没有
// tool_calls 帧，也没有正文，连「校验为什么拒了」都看不到。
//
// rejected > 0 表示模型确实发出了调用，只是没过 schema（最常见的是参数名写错）。
// 这时必须原样发：tool_protocol.go 要求「只输出那个围栏块」，所以这段文本往往
// 整体就是一个围栏，交给 flushStreamText 会被当成已取走的调用剥掉，剥完什么都
// 不剩。rejected == 0 才是真的散文，走正常的围栏剥离。
func ejectDelivery(corrected string, rejected int, toolMaps []map[string]any, choice any) string {
	if strings.TrimSpace(corrected) == "" {
		return ""
	}
	if rejected > 0 {
		return corrected
	}
	var pending strings.Builder
	pending.WriteString(corrected)
	var out strings.Builder
	if err := flushStreamText(&pending, toolMaps, choice, true, func(part string) error {
		out.WriteString(part)
		return nil
	}); err != nil {
		return corrected
	}
	// 剥离把整段都吃掉了（纠正轮其实就是一个围栏，只是它的调用被别处认走了）：
	// 仍然不能交空轮，原样发。
	if strings.TrimSpace(out.String()) == "" {
		return corrected
	}
	return out.String()
}

// correctionToolCalls 取出纠正轮文本里真正发出的已声明工具调用。
//
// 走的是派发路径同一个 fencedToolCalls，工具集也按 server.go 的同一种方式
// 归一化：判据必须和真正派发时一致，否则两边会像 declaredFenceStart 与
// fencedToolCalls 那样各说一套（那次是大小写，见 resolveDeclaredTool）。
//
// 这里只回答「有没有发出调用」。参数对不对由后续的 schema 校验负责，
// 那是另一条路径的事。
func correctionToolCalls(text string, req ejectRequest) []detectedToolCall {
	if strings.TrimSpace(text) == "" || len(req.Tools) == 0 {
		return nil
	}
	toolMaps := make([]map[string]any, 0, len(req.Tools))
	for _, tool := range req.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	choice := req.ToolChoice
	if choice == nil {
		choice = "auto"
	}
	return fencedToolCalls(text, toolMaps, choice)
}

// deferredStreamSink decides where streamed prose goes while it is still
// unclassified, and is the whole reason the correction above can work on a
// streaming turn.
//
// With tools declared, prose cannot be trusted to the wire yet. It may be a
// denial that a correction round is about to replace, or the preamble to a tool
// call that must not be sent as assistant content ahead of the tool_calls
// frame. Both are unfixable once Flush() has run, so prose accumulates in the
// returned buffer and live is never called. With no tools declared there is
// nothing to correct and nothing to convert, so prose streams as it arrives.
//
// The returned buffer is nil when output is not deferred: callers must gate
// every use of it on the deferred flag.
func deferredStreamSink(tools []map[string]any, live func(string) error) (func(string) error, *strings.Builder, bool) {
	if len(tools) == 0 {
		return live, nil, false
	}
	var held strings.Builder
	return func(part string) error {
		held.WriteString(part)
		return nil
	}, &held, true
}

// correctionHost names the machine without naming what it is not.
//
// Two constraints shape this. Spelling out the wrong answer in a prohibition
// raises its salience -- the self-sabotage already documented on
// runtimeWorkspaceInstruction, where repeating a cloud path made it the most
// prominent path in context. And emphatic negative imperatives ("you are NOT in
// a container") have been rejected outright by Microsoft's content filter as
// jailbreak attempts, taking the entire turn down with them. So: a flat
// statement of where execution happens, and nothing about where it does not.
func correctionHost() string {
	switch runtime.GOOS {
	case "windows":
		return "the caller's own Windows PC"
	case "darwin":
		return "the caller's own Mac"
	default:
		return "the caller's own machine"
	}
}

func toolRefusalCorrection(userRequest string) string {
	return "The previous reply reported that the caller's tools were unavailable. They are declared in this request, active, and callable right now, and they run on " +
		correctionHost() + ". Call the tool that fits the request below and answer from its result. Do not describe tool availability.\n\nUser request:\n" +
		userRequest
}

func sandboxDriftCorrection(userRequest string) string {
	return "The previous reply described where it was running instead of using the caller's tools. Commands, code and file access on " +
		correctionHost() + " go through those tools; they are what can see this machine. Call the tool that fits the request below and answer from its result. Do not report on the environment in place of a tool call.\n\nUser request:\n" +
		userRequest
}
