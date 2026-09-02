package web

import "testing"

func readFileTools() []map[string]any {
	return []map[string]any{
		{"function": map[string]any{"name": "read_file"}},
	}
}

// The measured defect: on the turn that only has to report a tool result, the
// newest user message is still the original request, so the intent heuristic
// keeps reporting an action request and the router burns a constrained retry.
func TestShouldRetryForToolIntentStopsOnceLedgerAnswers(t *testing.T) {
	prompt := "Open inventory.txt in this directory and tell me the build_serial value recorded in it."
	tools := readFileTools()

	if !toolIntentLikely(prompt, tools) {
		t.Fatalf("precondition: expected the original request to read as an action request")
	}

	empty := agentLedger{}
	if !shouldRetryForToolIntent(prompt, tools, empty) {
		t.Errorf("first turn with no evidence should still allow the constrained retry")
	}

	answered := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "read_file", Arguments: `{"path":"inventory.txt"}`, Result: "build_serial: X",
	}}}
	if shouldRetryForToolIntent(prompt, tools, answered) {
		t.Errorf("turn with completed evidence must not spend a retry telling the model to call something")
	}
}

func TestLedgerAnswersIntentRequiresSettledSuccess(t *testing.T) {
	cases := []struct {
		name   string
		ledger agentLedger
		want   bool
	}{
		{"empty ledger", agentLedger{}, false},
		{
			"pending result still owed",
			agentLedger{Pending: []toolEvidence{{ID: "c1", Name: "read_file"}}},
			false,
		},
		{
			"completed and successful",
			agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "read_file", Result: "ok"}}},
			true,
		},
		{
			"only failures leaves room to change strategy",
			agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "run_tests", Result: "exit 1", Failed: true}}},
			false,
		},
		{
			"one success among failures answers the turn",
			agentLedger{Completed: []toolEvidence{
				{ID: "c1", Name: "run_tests", Result: "exit 1", Failed: true},
				{ID: "c2", Name: "read_file", Result: "ok"},
			}},
			true,
		},
		{
			"a completed call cannot answer while another is pending",
			agentLedger{
				Completed: []toolEvidence{{ID: "c1", Name: "read_file", Result: "ok"}},
				Pending:   []toolEvidence{{ID: "c2", Name: "write_file"}},
			},
			false,
		},
	}
	for _, tc := range cases {
		if got := ledgerAnswersIntent(tc.ledger); got != tc.want {
			t.Errorf("%s: ledgerAnswersIntent = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// The guard must not swallow the genuine repair case it was built for: a first
// turn that names a declared tool and has no evidence yet.
func TestShouldRetryForToolIntentKeepsOrdinaryQuestionsOut(t *testing.T) {
	tools := readFileTools()
	if shouldRetryForToolIntent("What is a file descriptor?", tools, agentLedger{}) {
		t.Errorf("knowledge question must not trigger a constrained retry")
	}
}
