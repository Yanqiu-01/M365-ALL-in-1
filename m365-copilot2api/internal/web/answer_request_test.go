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
	// The prompt may gain the schema block (the refusal fix), but the user turn
	// itself must survive verbatim at the head.
	if !strings.HasPrefix(req.Text, "[user]\nhello") {
		t.Fatalf("answer prompt lost the user turn: %q", req.Text)
	}
}

func TestBuildAnswerRequestNativeForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "native")
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("native answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
}

// Router mode must keep Tools empty here (pinned above) while still telling
// chathub that the caller declared tools. The schemas now also ride the text:
// the answer turn is the last chance to keep a refused action callable, and the
// measured failure was Codex replying 「这个回合没有提供相应的本地 PowerShell
// 工具」on a turn where 14 tools were declared -- the model could name the list
// only by its absence.
func TestBuildAnswerRequestReportsDeclaredToolsWithoutForwardingThem(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router")
	if len(req.Tools) != 0 {
		t.Fatalf("router answer must not forward native tools: tools=%d", len(req.Tools))
	}
	if !req.ToolsDeclared {
		t.Errorf("router answer turn must report that the caller declared tools")
	}
	if !req.SchemasInText {
		t.Errorf("router answer prompt now carries the schema list, so SchemasInText must be true")
	}
	if !strings.Contains(req.Text, "read_file") {
		t.Errorf("answer prompt must carry the declared tool schemas, got:\n%s", req.Text)
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
