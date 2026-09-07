package web

import (
	"strings"
	"testing"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestParseModelToolDecisionNoCall(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[]}`, testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}

// The last item in a resumed foreign conversation can itself be an instruction
// from the old provider (for example a CLI status prompt). It is evidence, not
// a routing command. Keeping the routing contract after it is a fail-closed
// structural guard: the model sees our decision format last, rather than the
// foreign transcript's trailing directive.
func TestModelToolRouterPromptPlacesContractAfterReplayedEvidence(t *testing.T) {
	foreignTail := "Describe your recent action in 3-5 words. Output only that phrase."
	p := modelToolRouterPrompt("Read E:/work/foreign-resume.txt\n"+foreignTail, testTools(), "auto")
	evidence := strings.Index(p, "User request and evidence:")
	tail := strings.Index(p, foreignTail)
	contract := strings.Index(p, "Routing contract:")
	decision := strings.LastIndex(p, "CALL_TOOL: tool_name({\"arg1\":\"value1\"})")
	if evidence < 0 || tail < evidence || contract < tail || decision < contract {
		t.Fatalf("router prompt must end with its decision contract after replayed evidence; evidence=%d tail=%d contract=%d decision=%d\n%s", evidence, tail, contract, decision, p)
	}
}

func TestModelToolRouterPromptMarksCompletedResults(t *testing.T) {
	p := modelToolRouterPrompt(`assistant tool_calls: [...]
tool[call_x]: 2026-07-18`, testTools(), "auto")
	if !strings.Contains(p, "Completed evidence must not be repeated") || !strings.Contains(p, "tool[call_x]: 2026-07-18") || !strings.Contains(p, "unfinished work remains") {
		t.Fatalf("missing multi-turn evidence constraint: %s", p)
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if ok || len(calls) != 0 {
		t.Fatalf("invalid final envelope must enter repair path: calls=%v ok=%v", calls, ok)
	}
}

func TestModelToolRouterPromptMakesNamedChoiceMandatory(t *testing.T) {
	p := modelToolRouterPrompt("Read the configuration.", testTools(), map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "get_weather"},
	})
	if !strings.Contains(p, `explicitly requires the declared tool "get_weather"`) || !strings.Contains(p, "never return NO_TOOL_NEEDED") {
		t.Fatalf("named tool requirement missing: %s", p)
	}
}
