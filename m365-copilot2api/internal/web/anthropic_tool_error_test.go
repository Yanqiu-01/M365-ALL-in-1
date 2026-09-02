package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// anthropicLedgerFor converts an Anthropic body and builds the ledger the router
// would see, which is where a dropped is_error actually does its damage.
func anthropicLedgerFor(t *testing.T, body string) agentLedger {
	t.Helper()
	var req anthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("bad test body: %v", err)
	}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI conversion failed: %v", err)
	}
	return buildAgentLedger(o.Messages)
}

const failedReadBody = `{"model":"m365-copilot","messages":[
  {"role":"user","content":"read E:/Temp/missing.txt"},
  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"E:/Temp/missing.txt"}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"File does not exist.","is_error":true}]}
]}`

// A failed Claude Code tool result must enter the ledger as a failure. It did
// not: the adapter dropped is_error, and "File does not exist." trips no
// observational failure rule, so the ledger recorded a success and the gateway
// then believed the file had been read.
func TestFailedAnthropicToolResultIsRecordedAsFailed(t *testing.T) {
	l := anthropicLedgerFor(t, failedReadBody)
	if len(l.Completed) != 1 {
		t.Fatalf("Completed = %d entries, want 1 (%+v)", len(l.Completed), l.Completed)
	}
	if !l.Completed[0].Failed {
		t.Fatalf("failed Read recorded as success: %+v", l.Completed[0])
	}
}

// The failure must not be treated as evidence that answers the request, or the
// intent retry is suppressed and the model answers without ever reading it.
func TestFailedToolResultDoesNotAnswerIntent(t *testing.T) {
	l := anthropicLedgerFor(t, failedReadBody)
	if ledgerAnswersIntent("read E:/Temp/missing.txt", l) {
		t.Fatal("a failed read counted as answering the request")
	}
}

// A successful result must stay a success, so the fix does not invert.
func TestSuccessfulAnthropicToolResultStaysSuccessful(t *testing.T) {
	l := anthropicLedgerFor(t, `{"model":"m365-copilot","messages":[
      {"role":"user","content":"read inventory.txt"},
      {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"inventory.txt"}}]},
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"build_serial: 9PW8R2OZIYDB"}]}
    ]}`)
	if len(l.Completed) != 1 {
		t.Fatalf("Completed = %d entries, want 1", len(l.Completed))
	}
	if l.Completed[0].Failed {
		t.Fatalf("successful Read recorded as failed: %+v", l.Completed[0])
	}
}

// An is_error result whose content is a block list must keep its message rather
// than lose it to a failed type assertion.
func TestFailedToolResultWithContentBlocksKeepsItsMessage(t *testing.T) {
	l := anthropicLedgerFor(t, `{"model":"m365-copilot","messages":[
      {"role":"user","content":"run the tests"},
      {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]},
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"go: cannot find main module"}]}]}
    ]}`)
	if len(l.Completed) != 1 {
		t.Fatalf("Completed = %d entries, want 1", len(l.Completed))
	}
	if !l.Completed[0].Failed {
		t.Fatalf("failed Bash recorded as success: %+v", l.Completed[0])
	}
	if !strings.Contains(l.Completed[0].Result, "cannot find main module") {
		t.Fatalf("tool message lost: %q", l.Completed[0].Result)
	}
}
