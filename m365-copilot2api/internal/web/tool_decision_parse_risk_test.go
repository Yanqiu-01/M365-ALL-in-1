package web

import "testing"

// R1 measures the residual risk of the lead-3 patch: prose AFTER a valid
// decision that happens to be envelope-shaped now fails the turn closed.
func TestEnvelopeShapedProseAfterADecision(t *testing.T) {
	tools := auditTools()
	cases := map[string]string{
		// Safe: braces present but not envelope-shaped -> still parses.
		"plain braces after decision": `{"calls":[{"name":"Read","arguments":{"file_path":"a.py"}}]}` +
			"\n\nNote: the path {a.py} is relative to the repo root.",
		"schema hint after decision": `{"calls":[{"name":"Read","arguments":{"file_path":"a.py"}}]}` +
			"\n\n(the argument shape is {\"file_path\": <string>})",
		// Risk: a malformed envelope-shaped example after the real decision.
		"malformed envelope example after decision": `{"calls":[{"name":"Read","arguments":{"file_path":"a.py"}}]}` +
			"\n\nFor reference the generic shape is {\"calls\":[...]}.",
	}
	for name, text := range cases {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		t.Logf("%-42s parsed=%v calls=%d", name, parsed, len(calls))
	}
}

// 散文里提到标记不得派发工具。裸名字形态没有 JSON 参数体作为意图证据，
// 唯一凭据是规则要求的 "end with EXACTLY one line"，所以判据是「独占一行」。
//
// 这些用例全部来自实测：改动前，除句点结尾之外每一条都真的派发了 run_tests。
// 句点结尾能逃掉是偶然 —— '.' 是合法名字字节，被吞进名字变成 run_tests.，
// 靠工具名校验失败挡住的，不是靠任何有意的判据。
func TestMarkerInProseDoesNotDispatch(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		"I will not output CALL_TOOL: run_tests",
		"I will not output CALL_TOOL run_tests",
		"Do not output CALL_TOOL: run_tests!",
		"Do not output CALL_TOOL: run_tests,",
		"Do not output CALL_TOOL: run_tests;",
		"Do not output CALL_TOOL: run_tests)",
		"Do not output CALL_TOOL: run_tests\"",
		"The marker looks like `CALL_TOOL: run_tests`",
		"Do not output CALL_TOOL: run_tests\nI will answer directly instead.",
		"**Do not** output CALL_TOOL run_tests",
		"I will not output CALL_TOOL: run_tests.", // 句点：偶然挡住的那条
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if parsed || len(calls) != 0 {
			t.Errorf("prose must not dispatch: %q -> parsed=%v calls=%+v", text, parsed, calls)
		}
	}
}

// 反向：真调用是独占一行的裸名字，必须照旧派发。带装饰与前置思考也算。
func TestBareNameOnItsOwnLineStillDispatches(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		"CALL_TOOL: run_tests",
		"先跑一遍测试确认。\n\nCALL_TOOL: run_tests",
		"**CALL_TOOL: run_tests**",
		"> CALL_TOOL: run_tests",
		"- CALL_TOOL: run_tests",
		"`CALL_TOOL: run_tests`",
		"CALL_TOOL run_tests",
	} {
		calls, parsed := parseModelToolDecision(text, tools, "auto")
		if !parsed || len(calls) != 1 || calls[0].Name != "run_tests" {
			t.Errorf("real bare call must dispatch: %q -> parsed=%v calls=%+v", text, parsed, calls)
		}
	}
}

// R3: the lead-1 patch must not lose a bash fence whose body IS a JSON object
// of arguments, which is the shape tool_protocol.go actually teaches.
func TestDeclaredNameFenceWithJSONBodyStillWorks(t *testing.T) {
	tools := auditTools()
	text := "```Bash\n{\"command\":\"go test ./...\"}\n```"
	calls, parsed := parseModelToolDecision(text, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("declared-name fence with JSON body must parse: parsed=%v calls=%+v", parsed, calls)
	}
	// Lowercase tag resolving to the declared spelling still works.
	calls, parsed = parseModelToolDecision("```bash\n{\"command\":\"go test ./...\"}\n```", tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("lowercase fence tag must still resolve: parsed=%v calls=%+v", parsed, calls)
	}
}

// R4: a declared-name fence with a non-JSON body as the ONLY content still
// reports parsed=false, so the repair path is not lost by the lead-1 patch.
func TestNonJSONFenceAloneStillFailsClosed(t *testing.T) {
	tools := auditTools()
	for _, text := range []string{
		"```bash\ngo test ./...\n```",
		"```Read\nfile_path=a.py\n```",
	} {
		if calls, parsed := parseModelToolDecision(text, tools, "auto"); parsed || len(calls) != 0 {
			t.Errorf("%q must stay unparsed: parsed=%v calls=%+v", text, parsed, calls)
		}
	}
}
