package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

// The upstream model, left to itself, announces that it runs in an isolated
// cloud container and therefore cannot reach the caller's drives. Users act on
// that and stop using local paths, so every outbound turn has to carry the
// environment statement -- including turns with no tools, which previously went
// out untouched.
func TestEveryTurnCarriesEnvironmentStatement(t *testing.T) {
	bash, err := json.Marshal(map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := Tool{Function: bash}

	cases := []struct {
		name    string
		tools   []Tool
		choice  any
		plugins bool
	}{
		{name: "no tools", tools: nil, choice: "auto"},
		{name: "tool choice none", tools: []Tool{tool}, choice: "none"},
		{name: "tool definitions", tools: []Tool{tool}, choice: "auto"},
		{name: "plugins", tools: []Tool{tool}, choice: "auto", plugins: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolProtocolPrompt("read E:\\work\\notes.txt", tc.tools, tc.choice, tc.plugins)

			if !strings.Contains(got, "caller's own Windows PC") {
				t.Errorf("prompt does not state where execution happens:\n%s", got)
			}
			if !strings.Contains(got, "hosted locally on that PC") {
				t.Errorf("prompt does not say the gateway is local:\n%s", got)
			}
			// The user's text must survive intact; the statement is a prefix,
			// not a replacement.
			if !strings.Contains(got, "read E:\\work\\notes.txt") {
				t.Errorf("user text was lost:\n%s", got)
			}
		})
	}
}

// Tool definitions and the tool-call protocol must still be there. Fixing the
// environment claim must not cost us the ability to call tools.
func TestEnvironmentStatementKeepsToolProtocol(t *testing.T) {
	bash, err := json.Marshal(map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := toolProtocolPrompt("list the directory", []Tool{{Function: bash}}, "auto", false)
	for _, want := range []string{"<tools>", "bash", "User request:", "list the directory"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool protocol lost %q:\n%s", want, got)
		}
	}
}