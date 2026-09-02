package chathub

import (
	"strings"
	"testing"
)

const absentToolClause = "If none is wired up for it, say so directly"

// The gateway's router mode carries tool schemas inside Text and leaves Tools
// empty on purpose, so that ChatHub's own plugin protocol does not compete with
// the router's CALL_TOOL contract. That made every router turn take the
// no-tools branch, which closes by inviting the model to report that no tool is
// wired up -- printed immediately above the list of tools it was meant to pick
// from. The router only ever runs when tools exist, so that invitation can only
// mislead.
func TestRouterTurnDoesNotOfferAnAbsentToolEscape(t *testing.T) {
	routerText := "Available tools: read_file, write_file\nCALL_TOOL: name({...}) or NO_TOOL_NEEDED"

	got := toolProtocolPrompt(routerText, nil, "auto", false, true, true)
	if strings.Contains(got, absentToolClause) {
		t.Errorf("router turn still offers the absent-tool escape hatch:\n%s", got)
	}
	if !strings.Contains(got, "use one of the tools described below") {
		t.Errorf("router turn should point at the tools it was given:\n%s", got)
	}
	if !strings.Contains(got, routerText) {
		t.Errorf("router prompt text was lost:\n%s", got)
	}
}

// The turn that produces the visible answer also runs with Tools empty in router
// mode, and it is the one the user actually reads. Measured on a clean Claude CLI
// run against /v1/messages with 36 tools declared, it came back "I don't have a
// dedicated file-read tool wired up in this session" -- this clause echoed back.
//
// No schema list follows on this turn, so the closing must not promise one.
func TestAnswerTurnDropsTheEscapeWithoutPromisingASchemaList(t *testing.T) {
	got := toolProtocolPrompt("[user]\nread inventory.txt", nil, "auto", false, true, false)
	if strings.Contains(got, absentToolClause) {
		t.Errorf("answer turn still offers the absent-tool escape hatch:\n%s", got)
	}
	if strings.Contains(got, "described below") {
		t.Errorf("answer turn carries no schema list, so it must not point at one:\n%s", got)
	}
	if !strings.Contains(got, "use one of the caller's tools") {
		t.Errorf("answer turn should still say the caller's tools exist:\n%s", got)
	}
}

// The clause is correct when nothing is wired up: "read D:\x.txt" with no tools
// has no honest answer other than saying there is no tool for it. Removing it
// unconditionally would push the model to invent a call instead.
func TestPlainTurnKeepsTheAbsentToolClause(t *testing.T) {
	got := toolProtocolPrompt("read D:\\x.txt", nil, "auto", false, false, false)
	if !strings.Contains(got, absentToolClause) {
		t.Errorf("a turn with no tools must still be allowed to say so:\n%s", got)
	}
}

// tool_choice=none is an explicit instruction not to call anything, so the
// clause is appropriate again even though tools were declared.
func TestToolChoiceNoneKeepsTheAbsentToolClause(t *testing.T) {
	got := toolProtocolPrompt("read D:\\x.txt", nil, "none", false, true, true)
	if !strings.Contains(got, absentToolClause) {
		t.Errorf("tool_choice=none must not claim tools are usable:\n%s", got)
	}
}

// Branches that really do hand over callable tools must not carry the clause
// either.
func TestDeclaredToolBranchesDropTheAbsentToolClause(t *testing.T) {
	tool := envTestTool(t, map[string]any{
		"name":        "bash",
		"description": "run a command",
		"parameters":  map[string]any{"type": "object"},
	})
	for _, plugins := range []bool{false, true} {
		got := toolProtocolPrompt("list the directory", []Tool{tool}, "auto", plugins, false, false)
		if strings.Contains(got, absentToolClause) {
			t.Errorf("plugins=%t branch declares tools yet offers the absent-tool escape:\n%s", plugins, got)
		}
	}
}

// When every declaration fails to parse there is genuinely nothing to call, so
// the clause is the honest closing again.
func TestUnparsableDefinitionsKeepTheAbsentToolClause(t *testing.T) {
	nameless := envTestTool(t, map[string]any{"description": "no name field"})
	got := toolProtocolPrompt("list the directory", []Tool{nameless}, "auto", false, false, false)
	if !strings.Contains(got, absentToolClause) {
		t.Errorf("no parsable definition means no callable tool:\n%s", got)
	}
}

// The filter lesson from environmentPrompt's own comment: no prohibitions, no
// emphatic negatives in the replacement clause.
func TestToolsPresentClosingStaysFactual(t *testing.T) {
	for _, schemas := range []bool{false, true} {
		got := toolProtocolPrompt("read a file", nil, "auto", false, true, schemas)
		for _, banned := range []string{"NOT", "Never", "never claim", "Do not claim"} {
			if strings.Contains(got, banned) {
				t.Errorf("schemas=%t closing reintroduces a prohibition (%q), which tripped the content filter before:\n%s", schemas, banned, got)
			}
		}
	}
}
