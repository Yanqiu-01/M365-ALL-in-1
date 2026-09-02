package web

import "testing"

// Measured on a clean Claude CLI run against /v1/messages, gateway b5337e7,
// 36 tools declared: the request named two files, read_file completed for
// inventory.txt only, and the second turn answered "I still need to read
// config.ini -- could you share its contents?" with disposition
// intent_answered_from_ledger. One success had switched the guard on while half
// the request was still outstanding, so the constrained retry that would have
// pushed for the second read never ran.
func TestPartiallyCoveredRequestStillAllowsRetry(t *testing.T) {
	prompt := "Read inventory.txt and config.ini in this directory, then tell me the build_serial and the region value."
	tools := readFileTools()

	partial := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "read_file", Arguments: `{"path":"inventory.txt"}`, Result: "build_serial: X",
	}}}
	if ledgerAnswersIntent(prompt, partial) {
		t.Errorf("config.ini is still unread; the ledger does not answer this request yet")
	}
	if !shouldRetryForToolIntent(prompt, tools, partial) {
		t.Errorf("the retry that would fetch the second file must still be allowed")
	}

	full := agentLedger{Completed: []toolEvidence{
		{ID: "call_1", Name: "read_file", Arguments: `{"path":"inventory.txt"}`, Result: "build_serial: X"},
		{ID: "call_2", Name: "read_file", Arguments: `{"path":"config.ini"}`, Result: "region = eu-west-2"},
	}}
	if !ledgerAnswersIntent(prompt, full) {
		t.Errorf("both named files are covered; prose is the correct output now")
	}
	if shouldRetryForToolIntent(prompt, tools, full) {
		t.Errorf("with every named file read, the retry only wastes an upstream round")
	}
}

// The original single-file repair must keep working: that measurement is what
// the guard was built for.
func TestFullyCoveredSingleFileStillSuppressesRetry(t *testing.T) {
	prompt := "Open inventory.txt in this directory and tell me the build_serial value recorded in it."
	tools := readFileTools()
	answered := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "read_file", Arguments: `{"path":"inventory.txt"}`, Result: "build_serial: X",
	}}}
	if shouldRetryForToolIntent(prompt, tools, answered) {
		t.Errorf("single named file, read: the retry must stay suppressed")
	}
}

// A request naming no file-like target cannot be checked against the ledger, so
// it falls back to the plain "something succeeded" reading rather than retrying
// forever.
func TestRequestWithNoNamedTargetsFallsBackToAnySuccess(t *testing.T) {
	prompt := "Please list the files in this directory."
	answered := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "list_dir", Arguments: `{"path":"."}`, Result: "inventory.txt\nconfig.ini",
	}}}
	if !ledgerAnswersIntent(prompt, answered) {
		t.Errorf("no named target means the ledger cannot be shown incomplete")
	}
}

func TestNamedFileTargetsExtraction(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   []string
	}{
		{"two plain names", "Read inventory.txt and config.ini", []string{"inventory.txt", "config.ini"}},
		{"windows path reduced to base", `Open E:\Temp\probe\config.ini now`, []string{"config.ini"}},
		{"posix path reduced to base", "Open /etc/app/settings.json now", []string{"settings.json"}},
		{"backticked name", "Read `notes.md` please", []string{"notes.md"}},
		{"trailing sentence punctuation", "Check server.go.", []string{"server.go"}},
		{"deduplicated", "read a.txt then re-read a.txt", []string{"a.txt"}},
		{"no targets", "Explain what a file descriptor is", nil},
		{"bare word is not a target", "read the config", nil},
		{"version number is not a file", "upgrade to 1.24 please", nil},
	}
	for _, tc := range cases {
		got := namedFileTargets(tc.prompt)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// Coverage is checked against completed evidence only. A failed read leaves its
// target uncovered, so a retry that changes strategy stays available.
func TestFailedCallDoesNotCoverItsTarget(t *testing.T) {
	prompt := "Read config.ini and report the region."
	failed := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "read_file", Arguments: `{"path":"config.ini"}`, Result: "Error: permission denied", Failed: true,
	}}}
	if ledgerAnswersIntent(prompt, failed) {
		t.Errorf("a failed read must not count as covering its target")
	}
}
