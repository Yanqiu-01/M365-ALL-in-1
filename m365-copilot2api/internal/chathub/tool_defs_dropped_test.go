package chathub

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for one call and returns what it
// wrote. Restores the previous destination and flags either way.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevFlags := log.Flags()
	prevOutput := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// A declaration that fails to parse is dropped with `continue`, and when every
// one drops the function returns the same prompt as a caller who declared no
// tools at all. That made a gateway-side parse bug look identical to an empty
// client request from the outside, with nothing in the log to tell them apart.
// The drop has to be visible.
func TestDroppedToolDefinitionsAreLogged(t *testing.T) {
	nameless := envTestTool(t, map[string]any{"description": "no name field"})
	badJSON := Tool{Function: json.RawMessage(`{"name":`)}
	good := envTestTool(t, map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object"}})

	cases := []struct {
		label   string
		tools   []Tool
		wants   []string
		wantLog bool
	}{
		{
			label:   "every declaration drops",
			tools:   []Tool{nameless, badJSON},
			wants:   []string{"dropped=2", "of 2", "bad_json=1", "no_name=1", "kept=0"},
			wantLog: true,
		},
		{
			label:   "one of two drops",
			tools:   []Tool{good, nameless},
			wants:   []string{"dropped=1", "of 2", "no_name=1", "kept=1"},
			wantLog: true,
		},
		{
			label:   "nothing drops",
			tools:   []Tool{good},
			wantLog: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			out := captureLog(t, func() {
				toolProtocolPrompt("list the directory", tc.tools, "auto", false, false, false)
			})
			if !tc.wantLog {
				if strings.Contains(out, "tool-defs dropped") {
					t.Errorf("no declaration dropped, so nothing should be logged:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, "tool-defs dropped") {
				t.Fatalf("dropped declarations were silent, which is the bug this pins:\n%s", out)
			}
			for _, want := range tc.wants {
				if !strings.Contains(out, want) {
					t.Errorf("log is missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// Schemas can carry local paths and argument values, so the log line stays at
// counts and reasons. Tool names are already in the prompt; schema bodies are
// not something to copy into the log.
func TestDroppedToolDefinitionLogCarriesNoSchemaBody(t *testing.T) {
	secretish := envTestTool(t, map[string]any{
		"description": "reads E:\\private\\credentials.txt",
		"parameters":  map[string]any{"default": "E:\\private\\credentials.txt"},
	})
	out := captureLog(t, func() {
		toolProtocolPrompt("list the directory", []Tool{secretish}, "auto", false, false, false)
	})
	for _, leaked := range []string{"private", "credentials", "reads E:"} {
		if strings.Contains(out, leaked) {
			t.Errorf("log line copied schema content (%q), which can carry paths:\n%s", leaked, out)
		}
	}
}
