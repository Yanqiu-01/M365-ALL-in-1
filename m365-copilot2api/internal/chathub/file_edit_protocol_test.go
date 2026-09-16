package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFileEditProtocolExampleIsValidJSON(t *testing.T) {
	start := strings.Index(FileEditProtocolNote, `{"old_string":`)
	end := start + strings.Index(FileEditProtocolNote[start:], "}") + 1
	var example map[string]string
	if err := json.Unmarshal([]byte(FileEditProtocolNote[start:end]), &example); err != nil {
		t.Fatal(err)
	}
	if example["old_string"] != "\tfirst\n\tsecond" {
		t.Fatalf("wrong JSON escaping: %#v", example)
	}
	if !strings.Contains(FileEditProtocolNote, `\\t and \\n represent literal`) {
		t.Fatal("literal backslash text is not distinguished from JSON escaping")
	}
	for _, phrase := range []string{"Never", "You are NOT", "Do NOT", "real tab/"} {
		if strings.Contains(FileEditProtocolNote, phrase) {
			t.Errorf("unwanted wording %q", phrase)
		}
	}
}

func TestFileEditProtocolCoversAllToolPromptPaths(t *testing.T) {
	tools := []Tool{{Type: "function", Function: json.RawMessage(`{"name":"Edit","parameters":{"type":"object"}}`)}}
	cases := []struct {
		name                      string
		tools                     []Tool
		plugins, declared, inline bool
		choice                    any
		text                      string
		want                      bool
	}{
		{"fenced", tools, false, false, false, "auto", "edit it", true},
		{"plugin", tools, true, false, false, "auto", "edit it", true},
		{"router", nil, false, true, true, "auto", "route it", true},
		{"answer", nil, false, true, false, "auto", "continue", true},
		{"router note already inline", nil, false, true, true, "auto", FileEditProtocolNote + "\nroute it", true},
		{"tool choice none", tools, false, false, false, "none", "explain only", false},
		{"no declared tools", nil, false, false, false, nil, "hello", false},
		{"malformed declarations", []Tool{{Function: json.RawMessage(`{`)}}, false, false, false, "auto", "hello", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolProtocolPrompt(tc.text, tc.tools, tc.choice, tc.plugins, tc.declared, tc.inline)
			want := 0
			if tc.want {
				want = 1
			}
			if n := strings.Count(got, FileEditProtocolNote); n != want {
				t.Fatalf("note count=%d, want %d", n, want)
			}
		})
	}
}
