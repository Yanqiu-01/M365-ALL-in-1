package web

import "testing"

// These pin the two halves of one defect: the Anthropic adapter throws away the
// client's authoritative is_error flag, and the heuristic that has to guess in
// its absence guesses wrong for the tool names Claude Code actually uses.
func TestSuccessfulBashResultIsNotMarkedFailed(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		result string
	}{
		{"grep hit for the word error", "Bash", "src/app.go:42:\tif err != nil { return fmt.Errorf(\"error reading config: %w\", err) }"},
		{"go test naming an error test", "Bash", "ok  \tmypkg\t0.2s\n--- PASS: TestErrorPathReturnsFailure (0.00s)"},
		{"successful build printing a cleared error count", "Bash", "Build succeeded. 0 errors, 0 warnings."},
		{"glob listing a file named error.go", "Glob", "internal/web/error.go\ninternal/web/errors.go"},
		{"task summarising a fixed failure", "Task", "Investigated the failed login and fixed the permission denied path."},
	}
	for _, tc := range cases {
		if toolResultLooksFailed(tc.tool, tc.result) {
			t.Errorf("%s: %s result classified Failed, but it succeeded", tc.name, tc.tool)
		}
	}
}

// The heuristic must still catch a real failure for the same tool names, or
// fixing the false positives would just invert the defect.
func TestGenuineBashFailureIsStillMarkedFailed(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		result string
	}{
		{"non-zero exit", "Bash", "go: cannot find main module\nexit status 1"},
		{"compiler error", "Bash", "./main.go:7:2: undefined: fmt.Printn\nexit status 2"},
		{"permission denied at the start", "Bash", "permission denied: /etc/shadow"},
	}
	for _, tc := range cases {
		if !toolResultLooksFailed(tc.tool, tc.result) {
			t.Errorf("%s: %s failure was classified as success", tc.name, tc.tool)
		}
	}
}

// Bash can run anything, so it must count as mutating for the purposes that ask
// about capability: how many calls may run in parallel, and whether the
// workspace has changed. It must NOT thereby become unrepeatable -- re-running
// tests after an edit is necessary work, not duplicated work.
func TestShellToolsCountAsMutatingButStayRepeatable(t *testing.T) {
	for _, name := range []string{"Bash", "bash", "Task", "pwsh"} {
		if !toolLooksMutating(name) {
			t.Errorf("%q not counted as mutating; a shell-capable tool can write files", name)
		}
		if !toolCanRepeatSameArguments(name) {
			t.Errorf("%q cannot repeat identical arguments; re-running a command after a change is necessary work", name)
		}
	}
}

// Tools whose purpose is rewriting state must still never replay identically.
func TestStateRewritingToolsStayUnrepeatable(t *testing.T) {
	for _, name := range []string{"write_file", "apply_patch", "Edit", "Write", "delete_file", "install_deps"} {
		if toolCanRepeatSameArguments(name) {
			t.Errorf("%q allowed to replay identical arguments; that repeats the same change twice", name)
		}
	}
}

// The failed-precedent release must not auto-retry a shell command: a script
// that failed halfway may already have applied part of its effect.
func TestFailedShellCallIsNotAutoRetried(t *testing.T) {
	l := agentLedger{Completed: []toolEvidence{
		{Name: "Bash", Arguments: `{"command":"deploy.sh"}`, Result: "exit status 1", Failed: true},
	}}
	calls := []detectedToolCall{{Name: "Bash", Arguments: []byte(`{"command":"deploy.sh"}`)}}
	if got := filterCompletedCalls(calls, l); len(got) != 0 {
		t.Fatalf("failed shell call was re-emitted for automatic retry: %+v", got)
	}

	// A failed read, by contrast, has no side effect to redo and must be retryable.
	lr := agentLedger{Completed: []toolEvidence{
		{Name: "Read", Arguments: `{"path":"a.txt"}`, Result: "Error: file is locked", Failed: true},
	}}
	callsR := []detectedToolCall{{Name: "Read", Arguments: []byte(`{"path":"a.txt"}`)}}
	if got := filterCompletedCalls(callsR, lr); len(got) != 1 {
		t.Fatalf("failed read was not retryable: %+v", got)
	}
}

// Read-only names must stay repeatable, or the ledger-drop fix regresses.
func TestObservationalNamesStayRepeatable(t *testing.T) {
	for _, name := range []string{"Read", "Glob", "Grep", "read_file", "list_dir", "run_tests", "poll_status"} {
		if !toolCanRepeatSameArguments(name) {
			t.Errorf("%q reported as unsafe to repeat; read-only work must stay repeatable", name)
		}
	}
}
