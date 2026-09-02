package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// auditRealTools mirrors what an agentic CLI actually declares: a required
// argument per tool, so schema validation is a real gate rather than a no-op.
func auditRealToolMaps() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "Bash", "description": "run a shell command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}},
		}},
		{"type": "function", "function": map[string]any{
			"name": "Read", "description": "read a file",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}},
		}},
	}
}

func auditRealChathubTools() []chathub.Tool {
	out := make([]chathub.Tool, 0, 2)
	for _, m := range auditRealToolMaps() {
		fn, _ := json.Marshal(m["function"])
		out = append(out, chathub.Tool{Type: "function", Function: fn})
	}
	return out
}

const auditFence = "```"

// auditCorrectionWithCall is a compliant correction: a one-line preamble plus
// exactly the fenced call tool_protocol.go asks for.
func auditCorrectionWithCall(preamble, tool, args string) string {
	return preamble + "\n\n" + auditFence + tool + "\n" + args + "\n" + auditFence + "\n"
}

// correctSandboxDrift scores its own correction with the same substring
// detector that found the original denial. The pattern lists contain ordinary
// tool-call preambles ("I can run that for you", "let me run that", "I'll run
// that"), so a correction that DID emit a valid declared call is thrown away for
// its prose, and the held-back denial is what reaches the client.
func TestCorrectionCarryingValidToolCallIsNotDiscardedForItsProse(t *testing.T) {
	cases := []struct {
		name     string
		denial   string
		preamble string
	}{
		{
			name:     "tool-eject stage rejects its own compliant correction",
			denial:   "The tools actually provided to me this turn do not include a shell, so I cannot read that path.",
			preamble: "I can run that for you.",
		},
		{
			name: "sandbox-eject stage rejects its own compliant correction",
			// Trips only the sandbox list, so the sandbox stage is the one that
			// both raises and then judges the correction.
			denial:   "That path is not reachable from the cloud sandbox where I execute.",
			preamble: "Let me run that now.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			correction := auditCorrectionWithCall(tc.preamble, "Bash", `{"command":"ls E:\\Temp"}`)
			// Precondition: this correction really is what the round exists to get.
			calls, rejected := validateDetectedToolCalls(fencedToolCalls(correction, auditRealToolMaps(), "auto"), auditRealToolMaps(), "auto")
			if len(calls) != 1 || len(rejected) != 0 {
				t.Fatalf("test input is not a valid compliant correction: calls=%d rejected=%v", len(calls), rejected)
			}

			previous := correctionChat
			t.Cleanup(func() { correctionChat = previous })
			rounds := 0
			correctionChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request) (chathub.Result, error) {
				rounds++
				return chathub.Result{Text: correction}, nil
			}

			got, ok := (&Server{}).correctSandboxDrift(context.Background(), ejectRequest{
				AccountID: "a1", Tools: auditRealChathubTools(), ToolChoice: "auto",
				Text: tc.denial, UserRequest: "[user]\nlist E:\\Temp",
				ConversationID: "c1", SessionID: "s1",
			})
			if rounds == 0 {
				t.Fatalf("no correction round ran; the denial was not detected at all")
			}
			if !ok {
				t.Fatalf("correction carrying a valid %q call was discarded because its preamble %q matched a detector pattern; the denial reaches the client instead", calls[0].Name, tc.preamble)
			}
			if !strings.Contains(got.Text, "Bash") {
				t.Errorf("accepted replacement lost the tool call: %q", got.Text)
			}
		})
	}
}

// tool_choice:"none" forbids calling anything, and chathub's
// toolProtocolPrompt honours that by falling through to environmentPrompt's "if
// none is wired up for it, say so directly" clause. The model then does exactly
// that -- and the answer trips isToolRefusal. correctSandboxDrift has no
// tool_choice gate, so it spends a round telling the model to "call the tool that
// fits the request" against the caller's explicit instruction, and if that round
// comes back clean it REPLACES the compliant answer.
func TestCorrectSandboxDriftSkipsCorrectionWhenToolChoiceIsNone(t *testing.T) {
	previous := correctionChat
	t.Cleanup(func() { correctionChat = previous })
	rounds := 0
	correctionChat = func(_ context.Context, _ *Server, _ string, _ chathub.Account, _ chathub.Request) (chathub.Result, error) {
		rounds++
		return chathub.Result{Text: "Listing E:\\Temp would need a shell; here is how you would do it yourself."}, nil
	}

	// The honest answer the "say so directly" clause asks for.
	honest := "No file-reading tool is not available to me on this turn, so I cannot list that directory."
	if !isToolRefusal(honest) {
		t.Fatalf("precondition: the honest none-choice answer must be what the detector fires on")
	}
	corrected, ok := (&Server{}).correctSandboxDrift(context.Background(), ejectRequest{
		AccountID: "a1", Tools: auditRealChathubTools(), ToolChoice: "none",
		Text: honest, UserRequest: "[user]\nlist E:\\Temp",
	})
	if rounds != 0 {
		t.Errorf("ran %d correction round(s) under tool_choice=none; the round instructs a call the caller forbade", rounds)
	}
	if ok {
		t.Errorf("replaced a compliant tool_choice=none answer with %q", corrected.Text)
	}
}

// The streaming eject fallback. tool_protocol.go
// tells the model to emit ONLY the fenced block, so a correction that gets one
// argument name wrong is pure fence and nothing else. fencedToolCalls extracts it
// (it does not check the schema), validateCalls then rejects it, and the fallback
// re-runs the SAME text through flushStreamText -- whose fence strip removes the
// block it just refused to honour. deferred was already Reset, so the turn ends
// as an empty delta plus finish_reason=stop: no tool call, no prose, and the
// rejection is never reported to the client.
func TestStreamEjectDoesNotEndTheTurnEmptyWhenCorrectionFailsSchema(t *testing.T) {
	tools := auditRealToolMaps()
	// "path" instead of the schema's required "file_path" -- the ordinary
	// wrong-argument-name model error.
	correction := auditFence + "Read\n" + `{"path":"E:\\Temp\\notes.txt"}` + "\n" + auditFence + "\n"

	ejected := fencedToolCalls(correction, tools, "auto")

	if len(ejected) != 1 {
		t.Fatalf("precondition: fence must be extracted before validation, got %d", len(ejected))
	}
	valid, rejected := validateDetectedToolCalls(ejected, tools, "auto")
	if len(valid) != 0 || len(rejected) != 1 {
		t.Fatalf("precondition: call must fail schema validation, got valid=%d rejected=%v", len(valid), rejected)
	}

	sink, deferred, deferOutput := deferredStreamSink(tools, func(string) error { return nil })
	if !deferOutput {
		t.Fatal("declared tools must defer output")
	}
	// The held denial, then the delivery decision server.go actually makes.
	// Calling ejectDelivery rather than replaying flushStreamText inline: a
	// replay pins the sequence as it was written, so a fix inside the handler
	// cannot be observed by it.
	_ = sink("I cannot reach your local drive from here.")
	deferred.Reset()
	if err := sink(ejectDelivery(correction, len(rejected), tools, "auto")); err != nil {
		t.Fatalf("sink: %v", err)
	}

	delivered := deferred.String()
	if strings.TrimSpace(delivered) == "" {
		t.Fatalf("client receives an empty assistant turn: no tool_calls frame (%d valid), no prose (%q), and the %q rejection is dropped", len(valid), delivered, rejected[0].Reason)
	}
	// The rejected call must stay visible so the caller can see which tool was
	// tried and which argument was wrong.
	if !strings.Contains(delivered, "Read") {
		t.Errorf("delivered text lost the attempted tool name: %q", delivered)
	}
}
