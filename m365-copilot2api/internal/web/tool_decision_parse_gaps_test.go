package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// auditTools mirrors a real Claude Code declaration set: a shell tool spelled
// "Bash" (so a prose ```bash fence collides with it case-insensitively), a
// required-arg reader, and a no-required-arg tool.
func auditTools() []map[string]any {
	mk := func(name string, props map[string]any, required ...string) map[string]any {
		req := make([]any, 0, len(required))
		for _, r := range required {
			req = append(req, r)
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       name,
				"parameters": map[string]any{"type": "object", "properties": props, "required": req},
			},
		}
	}
	return []map[string]any{
		mk("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command"),
		mk("Read", map[string]any{"file_path": map[string]any{"type": "string"}}, "file_path"),
		mk("run_tests", map[string]any{}),
	}
}

func argsOf(t *testing.T, c detectedToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Arguments, &m); err != nil {
		t.Fatalf("bad arguments json %q: %v", c.Arguments, err)
	}
	return m
}

// ---------------------------------------------------------------------

// A prose ```bash fence collides with a declared tool named "Bash" because
// declaredTool matches case-insensitively. Its body is shell text, not JSON,
// so the fence becomes a *malformed decision frame* and fails the turn closed.
func TestProseShellFenceDoesNotKillTheRealDirective(t *testing.T) {
	tools := auditTools()
	text := "CALL_TOOL: Bash({\"command\":\"go test ./...\"})\n\nThat runs:\n```bash\ngo test ./...\n```\n"
	calls, parsed := parseModelToolDecision(text, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("illustrative bash fence discarded the real directive: parsed=%v calls=%+v", parsed, calls)
	}
}

// Same collision, adjacent-fence merge path: the prose fence and the real
// fenced call merge into one frame, and the prose half poisons it.
func TestProseShellFenceDoesNotMergeIntoARealFencedCall(t *testing.T) {
	tools := auditTools()
	text := "```bash\ngo test ./...\n```\n```Read\n{\"file_path\":\"a.py\"}\n```\n"
	calls, parsed := parseModelToolDecision(text, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("adjacent prose fence poisoned the real fenced call: parsed=%v calls=%+v", parsed, calls)
	}
}

// Control for lead 1: a declared-name fence whose body IS valid JSON but fails
// schema is a genuine broken call attempt and MUST still fail closed.
// (pinned by TestFencedFrameFollowsLastFrameWins)
func TestJSONBodyFailingSchemaStillFailsClosed(t *testing.T) {
	tools := auditTools()
	text := "CALL_TOOL: Read({\"file_path\":\"a.py\"})\n```Read\n{\"file_path\":123}\n```"
	if calls, parsed := parseModelToolDecision(text, tools, "auto"); parsed || len(calls) != 0 {
		t.Fatalf("schema-invalid fenced frame must fail closed: parsed=%v calls=%+v", parsed, calls)
	}
}

// ---------------------------------------------------------------------

// lastToolDirectiveIndex demands a literal ':' after CALL_TOOL, while
// extractDirectiveCandidates accepts whitespace/newline/decoration. A directive
// with no colon is therefore extractable but positionally invisible.
func TestDirectiveRecognizersAgreeOnColonlessForm(t *testing.T) {
	for _, text := range []string{
		`CALL_TOOL Read({"file_path":"a.py"})`,
		"CALL_TOOL\nRead({\"file_path\":\"a.py\"})",
		"CALL_TOOL `Read`({\"file_path\":\"a.py\"})",
	} {
		norm := normalizeDecisionText(text)
		cands := extractDirectiveCandidates(norm)
		idx := lastToolDirectiveIndex(norm)
		if len(cands) == 0 {
			t.Fatalf("extractor found no candidate for %q (test premise wrong)", text)
		}
		if idx < 0 {
			t.Errorf("recognizer disagreement for %q: extractor has candidate at %d, lastToolDirectiveIndex=-1",
				text, cands[len(cands)-1].At)
		}
	}
}

// End-to-end consequence of the disagreement: raw_candidates=0.
func TestColonlessDirectiveYieldsACandidate(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		`CALL_TOOL Read({"file_path":"a.py"})`,
		"I'll open the file.\n\nCALL_TOOL\nRead({\"file_path\":\"a.py\"})",
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
			t.Errorf("colonless directive not parsed: %q -> parsed=%v calls=%+v", text, parsed, calls)
		}
	}
}

// Second consequence: a stale colon-form directive from the thinking phase wins
// over the colonless final decision.
func TestColonlessFinalDirectiveBeatsAStaleOne(t *testing.T) {
	tools := auditTools()
	text := "One option is CALL_TOOL: Read({\"file_path\":\"wrong.py\"}) but no.\n\nCALL_TOOL run_tests({})"
	calls, parsed := parseModelToolDecision(text, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "run_tests" {
		t.Fatalf("stale directive executed instead of the final one: parsed=%v calls=%+v", parsed, calls)
	}
}

// Extra shape found while reading: bold wrapping the marker only.
func TestBoldedMarkerDirectiveIsRecognized(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		`**CALL_TOOL**: Read({"file_path":"a.py"})`,
		`__CALL_TOOL__: Read({"file_path":"a.py"})`,
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
			t.Errorf("bolded marker broke extraction: %q -> parsed=%v calls=%+v", text, parsed, calls)
		}
	}
}

// Negative controls for lead 2: prose must stay unparsed.
func TestProseDiscussionOfToolsStaysUnparsed(t *testing.T) {
	tools := auditTools()
	for name, text := range map[string]string{
		"discusses a tool in prose": "The Read tool would let me open the file, but I already know its contents.",
		"mentions name only":        "Read and Bash are both available here.",
		"refuses the marker":        "I will not output CALL_TOOL like that.",
		"refuses the marker cn":     "我不会输出 CALL_TOOL 这种东西的。",
	} {
		if calls, parsed := parseModelToolDecision(text, tools, "auto"); parsed || len(calls) != 0 {
			t.Errorf("%s must not parse: %q -> parsed=%v calls=%+v", name, text, parsed, calls)
		}
	}
}

// ---------------------------------------------------------------------

// An unparseable / truncated final envelope is skipped instead of being kept as
// the newest intent, so an earlier stale frame executes.
func TestMalformedFinalEnvelopeFailsClosedInsteadOfLettingAStaleFrameWin(t *testing.T) {
	tools := auditTools()
	cases := map[string]string{
		"trailing comma in final envelope": `{"calls":[{"name":"Read","arguments":{"file_path":"a.py"}}]}` + "\n" +
			`{"calls":[{"name":"Bash","arguments":{"command":"go test",}}]}`,
		"truncated final envelope": `{"calls":[{"name":"Read","arguments":{"file_path":"a.py"}}]}` + "\n" +
			`{"calls":[{"name":"Bash","arguments":{"command":"go te`,
		"malformed envelope after directive": "CALL_TOOL: Read({\"file_path\":\"a.py\"})\n" +
			`{"calls":[{"name":"Bash","arguments":{"command":"go test",}}]}`,
	}
	for name, text := range cases {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if parsed || len(calls) != 0 {
			t.Errorf("%s: stale frame executed instead of failing closed: parsed=%v calls=%+v", name, parsed, calls)
		}
	}
}

// ---------------------------------------------------------------------

// "parameters" is accepted as an arguments alias, so a model echoing a
// JSON-Schema-shaped definition has its arguments read from the schema wrapper.
func TestSchemaEchoIsNotTreatedAsACall(t *testing.T) {
	tools := auditTools()

	// (a) required-arg tool: the echo becomes the newest frame and fails the
	// turn closed, discarding the real directive that preceded it.
	textA := "CALL_TOOL: Read({\"file_path\":\"a.py\"})\n\nFor reference the tool is:\n" +
		`{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}`
	calls, parsed := parseModelToolDecision(textA, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
		t.Errorf("schema echo discarded the real directive: parsed=%v calls=%+v", parsed, calls)
	}

	// (b) no-required-arg tool: the echo VALIDATES and is dispatched with the
	// schema keywords as arguments.
	textB := `{"name":"run_tests","parameters":{"type":"object","properties":{}}}`
	calls, parsed = parseModelToolDecision(textB, tools, "auto")
	if parsed && len(calls) == 1 {
		got := argsOf(t, calls[0])
		if _, isSchema := got["type"]; isSchema {
			t.Errorf("schema echo dispatched as a call with schema keywords as arguments: %s", calls[0].Arguments)
		}
	}

	// (c) full OpenAI definition echo also becomes a frame.
	textC := `{"type":"function","function":{"name":"run_tests","description":"run the suite","parameters":{"type":"object","properties":{}}}}`
	calls, parsed = parseModelToolDecision(textC, tools, "auto")
	if parsed && len(calls) == 1 {
		got := argsOf(t, calls[0])
		if _, isSchema := got["type"]; isSchema {
			t.Errorf("definition echo dispatched as a call: %s", calls[0].Arguments)
		}
	}
}

// Control for lead 4: "parameters" carrying real argument values must keep
// working (pinned by TestParseToleratesRealisticDecisionShapes/parameters 别名).
func TestParametersAliasStillCarriesRealArguments(t *testing.T) {
	tools := auditTools()
	calls, parsed := parseModelToolDecision(`{"calls":[{"name":"Read","parameters":{"file_path":"a.py"}}]}`, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("parameters-as-arguments must keep parsing: parsed=%v calls=%+v", parsed, calls)
	}
	if argsOf(t, calls[0])["file_path"] != "a.py" {
		t.Fatalf("arguments=%s", calls[0].Arguments)
	}
}

// ---------------------------------------------------------------------

// The multi-turn "completed evidence" rules are gated on markers that the only
// producer of router prompt text (flattenPromptMessages) never emits.
func TestCompletedEvidenceRulesReachRealMultiTurnPrompts(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "read a.py then run the tests"},
		{Role: "assistant", ToolCalls: []map[string]any{{
			"id": "call_abc123", "type": "function",
			"function": map[string]any{"name": "Read", "arguments": `{"file_path":"a.py"}`},
		}}},
		{Role: "tool", ToolCallID: "call_abc123", Content: "print(1)"},
		{Role: "user", Content: "now run the tests"},
	}
	prompt := routerPromptMessages(msgs)
	t.Logf("flattened router prompt:\n%s", prompt)

	for _, marker := range []string{"tool_calls:", "tool[call_"} {
		if strings.Contains(prompt, marker) {
			t.Errorf("premise wrong: legacy gate marker %q IS emitted", marker)
		}
	}

	full := modelToolRouterPrompt(prompt, auditTools(), "auto")
	if !strings.Contains(full, "Completed evidence must not be repeated") {
		t.Errorf("multi-turn rules absent from a genuine multi-turn prompt (dead text)")
	}
}

// ---------------------------------------------------------------------

func TestUnclaimedDecisionShapes(t *testing.T) {
	tools := auditTools()
	for name, text := range map[string]string{
		"xml named":           `<tool_call name="Read"><arg name="file_path">a.py</arg></tool_call>`,
		"xml named json body": `<tool_call name="Read">{"file_path":"a.py"}</tool_call>`,
		"arrow":               `Read -> {"file_path": "a.py"}`,
		"equals":              `tool=Read args={"file_path": "a.py"}`,
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if !parsed || len(calls) != 1 {
			t.Logf("UNSUPPORTED %-20s parsed=%v calls=%+v", name, parsed, calls)
		} else {
			t.Logf("supported   %-20s -> %s %s", name, calls[0].Name, calls[0].Arguments)
		}
	}
}

// The XML named form with a JSON body is the one shape safe to add: the tag is
// an explicit marker, so prose cannot match it.
func TestXMLNamedCallWithJSONBodyParses(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		`<tool_call name="Read">{"file_path":"a.py"}</tool_call>`,
		"<tool_call name=\"Read\">\n{\"file_path\": \"a.py\"}\n</tool_call>",
		`<invoke name="Read">{"file_path":"a.py"}</invoke>`,
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
			t.Errorf("xml named form not parsed: %q -> parsed=%v calls=%+v", text, parsed, calls)
			continue
		}
		if argsOf(t, calls[0])["file_path"] != "a.py" {
			t.Errorf("arguments=%s", calls[0].Arguments)
		}
	}
	// Undeclared name still dies in validation.
	if _, parsed := parseModelToolDecision(`<tool_call name="delete_everything">{}</tool_call>`, tools, "auto"); parsed {
		t.Error("undeclared xml name must not parse")
	}
	// Last-frame-wins holds for the new shape.
	calls, parsed := parseModelToolDecision(
		"CALL_TOOL: run_tests({})\nactually:\n<tool_call name=\"Read\">{\"file_path\":\"a.py\"}</tool_call>", tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
		t.Errorf("later xml frame must win: parsed=%v calls=%+v", parsed, calls)
	}
}

// The plain Hermes shape (no name attribute) should already work via the bare
// single-call envelope path.
func TestHermesToolCallShapeParses(t *testing.T) {
	tools := auditTools()
	text := "<tool_call>\n{\"name\": \"Read\", \"arguments\": {\"file_path\": \"a.py\"}}\n</tool_call>"
	calls, parsed := parseModelToolDecision(text, tools, "auto")
	t.Logf("hermes: parsed=%v calls=%+v", parsed, calls)
	if !parsed || len(calls) != 1 || calls[0].Name != "Read" {
		t.Errorf("plain <tool_call> body not parsed: parsed=%v calls=%+v", parsed, calls)
	}
}

// Negative controls that MUST keep failing whatever gets added.
func TestCallSyntaxMentionedInProseStaysUnparsed(t *testing.T) {
	tools := auditTools()
	for name, text := range map[string]string{
		"prose discussion":  "I could use Read to look at a.py, and Bash to run the suite, but neither is needed yet.",
		"bare name mention": "Read, Bash, run_tests.",
		"arrow in prose":    "Read -> returns the file contents as a string.",
		"equals in prose":   "The tool=name convention is not what this gateway uses.",
		"xml mention":       "Some models wrap calls in <tool_call> tags; this one does not.",
	} {
		if calls, parsed := parseModelToolDecision(text, tools, "auto"); parsed || len(calls) != 0 {
			t.Errorf("%s must not parse: %q -> parsed=%v calls=%+v", name, text, parsed, calls)
		}
	}
}
