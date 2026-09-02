package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

func envTestTool(t *testing.T, payload map[string]any) Tool {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Tool{Function: encoded}
}

// The upstream model, left to itself, announces that it runs in an isolated
// cloud container and therefore cannot reach the caller's drives. Users act on
// that and stop using local paths, so every outbound turn has to carry the
// environment statement -- including turns with no tools, which previously went
// out untouched.
//
// The assertions are deliberately GOOS-stable. environmentPrompt names the host
// per platform, so pinning "Windows PC" here would fail the suite on any Mac or
// Linux build of the same gateway.
func TestEveryTurnCarriesEnvironmentStatement(t *testing.T) {
	tool := envTestTool(t, map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object"},
	})
	// A definition with no name is dropped, which leaves the tool branch with an
	// empty <tools> list. That fallback used to return the caller's text with no
	// environment statement at all.
	nameless := envTestTool(t, map[string]any{"description": "no name field"})

	cases := []struct {
		name        string
		tools       []Tool
		choice      any
		plugins     bool
		toolsInText bool
	}{
		{name: "no tools", tools: nil, choice: "auto"},
		{name: "tool choice none", tools: []Tool{tool}, choice: "none"},
		{name: "tool definitions", tools: []Tool{tool}, choice: "auto"},
		{name: "plugins", tools: []Tool{tool}, choice: "auto", plugins: true},
		{name: "no parsable definitions", tools: []Tool{nameless}, choice: "auto"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolProtocolPrompt("read E:\\work\\notes.txt", tc.tools, tc.choice, tc.plugins, tc.toolsInText, tc.toolsInText)

			if !strings.Contains(got, "Runtime: the caller's own") {
				t.Errorf("prompt does not state where execution happens:\n%s", got)
			}
			if !strings.Contains(got, "hosted locally on that") {
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

// Naming the cloud upload path or the container in a prohibition is the
// self-sabotage runtime_prompt.go already documents: it makes the wrong answer
// the most prominent thing in context. The plugin and tool branches used to do
// exactly that while trying to forbid it.
func TestToolProtocolPromptDoesNotRaiseTheCloudPaths(t *testing.T) {
	tool := envTestTool(t, map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object"},
	})

	for _, plugins := range []bool{false, true} {
		got := toolProtocolPrompt("list the directory", []Tool{tool}, "auto", plugins, false, false)
		for _, salient := range []string{"/mnt/data", "Linux", "container"} {
			if strings.Contains(got, salient) {
				t.Errorf("plugins=%t branch names %q, which pulls the model toward it:\n%s", plugins, salient, got)
			}
		}
	}
}

// Tool definitions and the tool-call protocol must still be there. Fixing the
// environment claim must not cost us the ability to call tools.
func TestEnvironmentStatementKeepsToolProtocol(t *testing.T) {
	tool := envTestTool(t, map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	})

	got := toolProtocolPrompt("list the directory", []Tool{tool}, "auto", false, false, false)
	for _, want := range []string{"<tools>", "bash", "User request:", "list the directory"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool protocol lost %q:\n%s", want, got)
		}
	}
}
