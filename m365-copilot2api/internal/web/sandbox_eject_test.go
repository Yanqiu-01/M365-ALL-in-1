package web

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// ejectTestTools returns the map form, which is what deferredStreamSink takes
// (server.go builds it once as toolMaps). ejectRequest carries the chathub.Tool
// form instead, because that is what chathub.Request.Tools needs -- see
// ejectTestChathubTools.
func ejectTestTools(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name, "description": "test tool", "parameters": map[string]any{"type": "object"}},
		})
	}
	return out
}

func ejectTestChathubTools(names ...string) []chathub.Tool {
	out := make([]chathub.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, chathub.Tool{
			Type:     "function",
			Function: json.RawMessage(`{"name":"` + name + `","description":"test tool","parameters":{"type":"object"}}`),
		})
	}
	return out
}

// The streaming path used to flush prose fragment by fragment, so a reply that
// denied the caller's tools was already on the client's screen by the time the
// answer was complete enough to classify. SSE has no rewind: the only fix is to
// hold the prose until the finished answer has been checked.
func TestDeferredStreamSinkHoldsOutputWhileToolsAreDeclared(t *testing.T) {
	live := []string{}
	sink, held, deferred := deferredStreamSink(ejectTestTools("bash"), func(part string) error {
		live = append(live, part)
		return nil
	})
	if !deferred {
		t.Fatal("declared tools must defer output; a denial would otherwise reach the client unclassified")
	}
	if held == nil {
		t.Fatal("deferred output needs a buffer to accumulate into")
	}
	for _, part := range []string{"I only have ", "an isolated container"} {
		if err := sink(part); err != nil {
			t.Fatalf("sink returned %v", err)
		}
	}
	if len(live) != 0 {
		t.Errorf("deferred sink wrote to the wire: %q", live)
	}
	if got := held.String(); got != "I only have an isolated container" {
		t.Errorf("buffered text = %q, want the full accumulated answer", got)
	}
}

// Without tools there is nothing to correct and no fenced call to convert, so
// holding the text back would cost time-to-first-token for no benefit.
func TestDeferredStreamSinkStreamsLiveWithoutTools(t *testing.T) {
	live := []string{}
	sink, held, deferred := deferredStreamSink(nil, func(part string) error {
		live = append(live, part)
		return nil
	})
	if deferred {
		t.Error("a turn with no declared tools must stream live")
	}
	if held != nil {
		t.Error("live streaming must not allocate a hold buffer")
	}
	if err := sink("hello"); err != nil {
		t.Fatalf("sink returned %v", err)
	}
	if len(live) != 1 || live[0] != "hello" {
		t.Errorf("live sink delivered %q, want [hello]", live)
	}
}

// Both correction rounds are re-asks, not jailbreaks. Two failures are pinned
// here because the code already paid for both: naming the cloud path or the
// container in a prohibition raises their salience and pulls the model back
// toward them, and emphatic negative imperatives have been rejected outright by
// Microsoft's content filter, which loses the whole turn rather than fixing it.
func TestCorrectionPromptsStayFactualAndCarryTheRequest(t *testing.T) {
	const request = "[user]\nread E:\\download\\notes.txt"
	for name, prompt := range map[string]string{
		"tool refusal": toolRefusalCorrection(request),
		"sandbox":      sandboxDriftCorrection(request),
	} {
		if !strings.Contains(prompt, request) {
			t.Errorf("%s correction dropped the request it is supposed to retry:\n%s", name, prompt)
		}
		if !strings.Contains(prompt, "caller's own") {
			t.Errorf("%s correction does not state where execution happens:\n%s", name, prompt)
		}
		for _, salient := range []string{"/mnt/data", "Linux", "container", "code interpreter"} {
			if strings.Contains(prompt, salient) {
				t.Errorf("%s correction names %q; a prohibition that spells out the wrong answer raises it", name, salient)
			}
		}
		for _, emphatic := range []string{"CRITICAL", "You must NOT", "Do NOT", "NEVER", "you are NOT"} {
			if strings.Contains(prompt, emphatic) {
				t.Errorf("%s correction uses jailbreak-shaped phrasing %q, which the content filter rejects", name, emphatic)
			}
		}
	}
}

// Ordering matters: a tool denial is corrected first, and the sandbox round then
// re-reads whatever that produced.
func TestDriftCorrectionsCoverBothDetectors(t *testing.T) {
	corrections := driftCorrections()
	if len(corrections) != 2 {
		t.Fatalf("len(driftCorrections()) = %d, want 2", len(corrections))
	}
	if corrections[0].stage != "tool-eject" || corrections[1].stage != "sandbox-eject" {
		t.Fatalf("unexpected stages: %q, %q", corrections[0].stage, corrections[1].stage)
	}
	if !corrections[0].detects("The tools actually provided to me this turn do not include a shell.") {
		t.Error("tool-eject stage does not detect a tool denial")
	}
	if !corrections[1].detects("I ran it in the python sandbox for you.") {
		t.Error("sandbox-eject stage does not detect sandbox narration")
	}
}

// The guard runs before any upstream call, so a zero-value Server is enough to
// prove no correction round is spent when there is nothing to correct toward.
func TestCorrectSandboxDriftNeedsDeclaredToolsAndText(t *testing.T) {
	server := &Server{}
	if _, ok := server.correctSandboxDrift(context.Background(), ejectRequest{
		Text: "I only have an isolated container, so I cannot read C:\\.",
	}); ok {
		t.Error("a turn with no declared tools has no tool to redirect to; it must not burn a correction round")
	}
	if _, ok := server.correctSandboxDrift(context.Background(), ejectRequest{
		Tools: ejectTestChathubTools("bash"),
		Text:  "   ",
	}); ok {
		t.Error("empty answer text must not trigger a correction round")
	}
}

// The correction round used to be sent with no Tools and no ToolChoice at all.
// chathub's toolProtocolPrompt emits the <tools> block only for a non-empty tool
// list, so the model was told to "call the tool that fits the request" while it
// could see no tools -- the round could not repair the one failure it exists for.
// This pins the whole request the model is actually handed.
func TestCorrectionRoundCarriesToolsChoiceAndConversation(t *testing.T) {
	server := &Server{}
	tools := ejectTestChathubTools("bash", "read_file")
	choice := map[string]any{"type": "function", "function": map[string]any{"name": "bash"}}

	previous := correctionChat
	t.Cleanup(func() { correctionChat = previous })
	var sent []chathub.Request
	var sentAccounts []string
	correctionChat = func(_ context.Context, _ *Server, accountID string, _ chathub.Account, request chathub.Request) (chathub.Result, error) {
		sent = append(sent, request)
		sentAccounts = append(sentAccounts, accountID)
		return chathub.Result{Text: "已用 bash 工具读取 E:\\download\\notes.txt，共 12 行。"}, nil
	}

	corrected, ok := server.correctSandboxDrift(context.Background(), ejectRequest{
		AccountID:      "account-1",
		Tools:          tools,
		ToolChoice:     choice,
		Text:           "当前这一轮实际提供给我的工具中，仍没有 PowerShell，只有隔离的容器工具。",
		UserRequest:    "[user]\nread E:\\download\\notes.txt",
		Tone:           "magic",
		ConversationID: "conversation-1",
		SessionID:      "session-1",
	})
	if !ok {
		t.Fatal("a detected denial with declared tools must produce a correction")
	}
	if len(sent) == 0 {
		t.Fatal("no correction round reached the model")
	}
	if sentAccounts[0] != "account-1" {
		t.Errorf("correction went to account %q, want account-1", sentAccounts[0])
	}
	got := sent[0]
	if !reflect.DeepEqual(got.Tools, tools) {
		t.Errorf("correction round tools = %#v, want the caller's %d declared tools; with an empty list chathub emits no <tools> block", got.Tools, len(tools))
	}
	if !reflect.DeepEqual(got.ToolChoice, choice) {
		t.Errorf("correction round tool_choice = %#v, want %#v; the round must keep the original call policy", got.ToolChoice, choice)
	}
	if got.ConversationID != "conversation-1" || got.SessionID != "session-1" {
		t.Errorf("correction left the answer's conversation: conversation=%q session=%q", got.ConversationID, got.SessionID)
	}
	if got.Tone != "magic" {
		t.Errorf("correction round tone = %q, want magic", got.Tone)
	}
	if !strings.Contains(got.Text, "[user]\nread E:\\download\\notes.txt") {
		t.Errorf("correction round dropped the request it retries: %q", got.Text)
	}
	if corrected.ConversationID != "conversation-1" || corrected.SessionID != "session-1" {
		t.Errorf("replacement result lost the answer's identity: conversation=%q session=%q", corrected.ConversationID, corrected.SessionID)
	}
}

// tool_choice must survive as-is, including "none": a correction that upgrades
// the policy would invite a call the caller forbade.
func TestCorrectionRequestPreservesToolChoiceForms(t *testing.T) {
	tools := ejectTestChathubTools("bash")
	for _, choice := range []any{nil, "auto", "required", "none", map[string]any{"type": "function", "function": map[string]any{"name": "bash"}}} {
		got := correctionRequest(ejectRequest{Tools: tools, ToolChoice: choice}, "instruction")
		if !reflect.DeepEqual(got.ToolChoice, choice) {
			t.Errorf("tool_choice %#v was rewritten to %#v", choice, got.ToolChoice)
		}
		if !reflect.DeepEqual(got.Tools, tools) {
			t.Errorf("tools dropped for choice %#v: %#v", choice, got.Tools)
		}
	}
}

// A correction is only worth swapping in if it no longer trips the detector that
// caused it. This pins the detector contract the eject relies on, so a future
// pattern edit cannot silently make every clean answer look like a denial.
func TestEjectDetectorsSeparateDenialFromDelivery(t *testing.T) {
	denial := "当前这一轮实际提供给我的工具中，仍没有 PowerShell，只有隔离的容器工具。"
	delivered := "已用 bash 工具读取 E:\\download\\notes.txt，共 12 行。"
	if !isToolRefusal(denial) && !isSandboxHallucination(denial) {
		t.Error("no detector matched the denial the correction round exists for")
	}
	if isToolRefusal(delivered) || isSandboxHallucination(delivered) {
		t.Error("a delivered answer must not be treated as drift; the retry would replace a correct result")
	}
}
