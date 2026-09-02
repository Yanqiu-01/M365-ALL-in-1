package web

import (
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func answerRequestTestBody() oaiReq {
	return oaiReq{
		ConversationID: "conversation-1",
		SessionID:      "session-1",
		Tools: []chathub.Tool{{
			Type:     "function",
			Function: json.RawMessage(`{"name":"read_file","parameters":{"type":"object"}}`),
		}},
		ToolChoice: "auto",
	}
}

func TestBuildAnswerRequestRouterOmitsNativePlugins(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router")
	if len(req.Tools) != 0 || req.ToolChoice != nil {
		t.Fatalf("router answer leaked native tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if req.Text != "[user]\nhello" {
		t.Fatalf("empty ledger changed answer prompt: %q", req.Text)
	}
}

func TestBuildAnswerRequestNativeForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "native")
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("native answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
}

// Router mode must keep Tools empty here (pinned above) while still telling
// chathub that the caller declared tools. Without that, the environment prompt
// takes its no-tools branch on the turn that produces the visible answer and
// offers the model an absent-tool escape hatch. Measured on a clean Claude CLI
// run with 36 tools declared: "I don't have a dedicated file-read tool wired up
// in this session".
func TestBuildAnswerRequestReportsDeclaredToolsWithoutForwardingThem(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router")
	if len(req.Tools) != 0 {
		t.Fatalf("router answer must not forward native tools: tools=%d", len(req.Tools))
	}
	if !req.ToolsDeclared {
		t.Errorf("router answer turn must report that the caller declared tools")
	}
	if req.SchemasInText {
		t.Errorf("answer prompt carries no schema list, so SchemasInText must stay false")
	}
}

func TestBuildAnswerRequestNoToolsDeclaresNone(t *testing.T) {
	body := answerRequestTestBody()
	body.Tools = nil
	req := buildAnswerRequest("[user]\nhello", "magic", body, agentLedger{}, "router")
	if req.ToolsDeclared {
		t.Errorf("a request with no tools must not claim the caller declared any")
	}
}

// tool_choice=none is an instruction not to call anything, so the answer turn
// should not assert the caller's tools are usable.
func TestBuildAnswerRequestToolChoiceNoneDeclaresNone(t *testing.T) {
	body := answerRequestTestBody()
	body.ToolChoice = "none"
	req := buildAnswerRequest("[user]\nhello", "magic", body, agentLedger{}, "router")
	if req.ToolsDeclared {
		t.Errorf("tool_choice=none must not report declared tools")
	}
}

func TestBuildAnswerRequestAddsCompletedEvidence(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{ID: "call_1", Name: "read_file", Arguments: `{}`, Result: "ok"}}}
	req := buildAnswerRequest("[user]\nsummarize", "magic", answerRequestTestBody(), ledger, "router")
	for _, want := range []string{"EVIDENCE_LEDGER:", "Report only actions supported by completed tool results"} {
		if !strings.Contains(req.Text, want) {
			t.Fatalf("answer prompt missing %q: %s", want, req.Text)
		}
	}
}
