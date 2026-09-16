package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSalvageComposesPathAndControlRepairs(t *testing.T) {
	path := `C:\src\renderer\state.ts`
	escapedPath, _ := json.Marshal(path)
	cases := []struct{ name, path, old, want string }{
		{"escaped path and JSON escapes", string(escapedPath), `\tfirst\n\tsecond`, "\tfirst\n\tsecond"},
		{"escaped path and bare controls", string(escapedPath), "\tfirst\n\tsecond", "\tfirst\n\tsecond"},
		{"escaped path and literal backslashes", string(escapedPath), `\\tfirst\\n\\tsecond`, `\tfirst\n\tsecond`},
		{"raw path and JSON escapes", `"` + path + `"`, `\tfirst\n\tsecond`, "\tfirst\n\tsecond"},
		{"raw path and bare controls", `"` + path + `"`, "\tfirst\n\tsecond", "\tfirst\n\tsecond"},
		{"raw path and literal backslashes", `"` + path + `"`, `\\tfirst\\n\\tsecond`, `\tfirst\n\tsecond`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"file_path":%s,"old_string":"%s","new_string":"\tnew\r\nline"}`, tc.path, tc.old)
			var args struct {
				FilePath string `json:"file_path"`
				Old      string `json:"old_string"`
				New      string `json:"new_string"`
			}
			if !unmarshalJSONTolerant(body, &args) {
				t.Fatalf("could not parse %q", body)
			}
			if args.FilePath != path {
				t.Fatalf("path corrupted: %q, want %q", args.FilePath, path)
			}
			if args.Old != tc.want || args.New != "\tnew\r\nline" {
				t.Fatalf("content corrupted: old=%q new=%q", args.Old, args.New)
			}
		})
	}
}

func TestSalvagePathWithTrailingSlashAndBareControls(t *testing.T) {
	body := `{"file_path":"C:\temp\renderer\","old_string":"` + "\tx\ny" + `","new_string":"a\nb"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("could not parse %q", body)
	}
	if args["file_path"] != `C:\temp\renderer\` || args["old_string"] != "\tx\ny" || args["new_string"] != "a\nb" {
		t.Fatalf("corrupted: %#v", args)
	}
}

func TestSalvageFindsCorruptedPathAfterAnUnrelatedPath(t *testing.T) {
	body := `{"source":"C:\\work\\a","file_path":"E:\src\renderer\state.ts","old_string":"` + "\tx\ny" + `","new_string":"x"}`
	var args map[string]any
	if !unmarshalJSONTolerant(body, &args) {
		t.Fatalf("could not parse %q", body)
	}
	if args["file_path"] != `E:\src\renderer\state.ts` {
		t.Fatalf("later path corrupted: %q", args["file_path"])
	}
	if args["source"] != `C:\work\a` || args["old_string"] != "\tx\ny" {
		t.Fatalf("unrelated fields changed: %#v", args)
	}
}

func TestSalvageComposedRepairReachesBothToolParsers(t *testing.T) {
	tools := editTools()
	body := `{"file_path":"C:\src\renderer\state.ts","old_string":"` + "\tx\ny" + `","new_string":"\tx\nz"}`
	outputs := []string{"```Edit\n" + body + "\n```", "CALL_TOOL: Edit(" + body + ")"}
	for _, output := range outputs {
		var calls []detectedToolCall
		if strings.HasPrefix(output, "```") {
			calls = fencedToolCalls(output, tools, "auto")
		} else {
			calls, _ = parseModelToolDecision(output, tools, "auto")
		}
		valid, rejected := validateDetectedToolCalls(calls, tools, "auto")
		if len(valid) != 1 || len(rejected) != 0 {
			t.Fatalf("call lost: calls=%+v rejected=%+v", calls, rejected)
		}
		var args map[string]string
		if err := json.Unmarshal(valid[0].Arguments, &args); err != nil {
			t.Fatal(err)
		}
		if args["file_path"] != `C:\src\renderer\state.ts` || args["old_string"] != "\tx\ny" {
			t.Fatalf("corrupted parser output: %#v", args)
		}
	}
}

// These strings are file CONTENT, not paths, even when the first two characters
// happen to be a drive letter and colon. JSON escapes there keep their meaning.
func TestSalvageKeepsDrivePrefixedEditAndWriteText(t *testing.T) {
	for _, field := range []string{"old_string", "new_string", "content", "contents", `old_\u0073tring`} {
		for _, path := range []string{`C:\\work\\a.ts`, `C:\temp\a.ts`} {
			body := `{"file_path":"` + path + `","` + field + `":"C:\nfirst\tsecond\r\nlast"}`
			var args map[string]string
			if !unmarshalJSONTolerant(body, &args) {
				t.Fatalf("could not parse %q", body)
			}
			key := field
			if field == `old_\u0073tring` {
				key = "old_string"
			}
			if args[key] != "C:\nfirst\tsecond\r\nlast" {
				t.Fatalf("%s content changed to %q", field, args[key])
			}
		}
	}
}
