package web

import "testing"

// Captured verbatim from a clean Claude CLI run against /v1/messages on
// 2026-09-02 (gateway 33bf3db, 36 tools declared, choice=auto). The router
// produced unparsable prose, the intent retry did the same, and the answer turn
// emitted this. It reached the client uncorrected: neither detector matched, so
// correctSandboxDrift never ran.
//
// The near misses are the point. toolRefusalPatterns already held "sandbox
// environment" and "python sandbox", but the model wrote "sandboxed
// environment" and "python_execution" -- substring matching does not bridge
// either. A denial that names the caller's own drive letter while offering a
// hosted interpreter instead is the exact failure both lists exist to catch.
const measuredSandboxDenial = "It seems the Python execution tool couldn't access the local filesystem " +
	"path directly. Let me clarify: I don't have a dedicated file-read tool wired up in this " +
	"session that can browse `E:\\Temp\\m365-clean-probe\\` directly — the `python_execution` " +
	"tool runs in a sandboxed environment and can't reach your local drive paths.\n\n" +
	"To get those values, you could:\n\n" +
	"1. **Paste the file contents here** — just copy and paste the text from `inventory.txt`\n" +
	"2. **Run this in PowerShell yourself** and share the output"

func TestMeasuredSandboxDenialIsDetected(t *testing.T) {
	if !isToolRefusal(measuredSandboxDenial) {
		t.Errorf("isToolRefusal missed the measured denial; it reached the client uncorrected")
	}
	if !isSandboxHallucination(measuredSandboxDenial) {
		t.Errorf("isSandboxHallucination missed the measured denial")
	}
}

// Each phrasing that carried the denial should be caught on its own, so a
// shorter variant of the same answer cannot slip through.
func TestMeasuredDenialPhrasingsAreEachDetected(t *testing.T) {
	for _, text := range []string{
		"the `python_execution` tool runs in a sandboxed environment",
		"I don't have a dedicated file-read tool wired up in this session",
		"the Python execution tool couldn't access the local filesystem path",
		"I can't reach your local drive paths",
		"paste the file contents here and I'll extract the values",
		"run this in PowerShell yourself and share the output",
	} {
		if !isToolRefusal(text) {
			t.Errorf("isToolRefusal missed %q", text)
		}
	}
}

// The lists are matched as substrings against ordinary model prose, so they must
// not fire on legitimate answers. A gateway that rewrites correct output is
// worse than one that occasionally misses a denial.
func TestRefusalDetectorsLeaveLegitimateAnswersAlone(t *testing.T) {
	for _, text := range []string{
		"I read inventory.txt and the build_serial is 9PW8R2OZIYDB.",
		"The config file sets region = eu-west-2 and retry_limit = 3.",
		"I ran the tests with the bash tool and all 14 passed.",
		"The sandbox module in your repo is at internal/sandbox/run.go.",
		"Your PowerShell script writes to E:\\logs\\out.txt on each run.",
		"I used read_file on both paths and neither contains a region key.",
		"The function returns an error when the local filesystem path is missing.",
	} {
		if isToolRefusal(text) {
			t.Errorf("isToolRefusal fired on a legitimate answer: %q", text)
		}
		if isSandboxHallucination(text) {
			t.Errorf("isSandboxHallucination fired on a legitimate answer: %q", text)
		}
	}
}
