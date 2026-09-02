package chathub

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"strings"
)

// environmentPrompt states where execution actually happens. Left to itself the
// upstream model describes itself as running in an isolated cloud container with
// no access to the caller's drives. That is not a harmless hedge: users are told
// to give up on local paths and work around a limit that does not exist.
//
// Phrasing matters more than it looks. An earlier version of this text used
// emphatic negative imperatives ("You are NOT in a container", "Never claim
// ...") and Microsoft's content filter rejected the entire turn -- even "say OK"
// came back as the generic cannot-chat-about-this line. It reads as a jailbreak
// attempt. Keep this a flat statement of fact with no prohibitions.
//
// Prepended on every turn, not just the first: in long conversations a single
// opening statement gets diluted and the container story returns mid-session.
//
// Keep this factual and GOOS-aware. Do not name cloud-sandbox paths here — naming
// them raises their salience and is the self-sabotage already documented in the
// web runtime prompt.
//
// hasTools decides the closing sentence. The absent-tool clause is correct for a
// plain chat turn with nothing wired up: the honest answer to "read D:\x.txt" is
// that there is no tool for it. It is wrong whenever the turn does have tools --
// including both halves of the gateway's router mode, which empties Tools on the
// router turn (schemas travel inside Text) and on the answer turn that follows.
// Measured on a clean Claude CLI run: the model answered "I don't have a
// dedicated file-read tool wired up in this session", echoing this clause back
// out of a turn where 36 tools were declared.
//
// schemasFollow narrows the replacement. Pointing at "the tools described below"
// is only true when the schemas are actually inline; on the answer turn they are
// not, so that turn simply drops the escape hatch without promising a list.
func environmentPrompt(hasTools, schemasFollow bool) string {
	var closing string
	switch {
	case hasTools && schemasFollow:
		closing = "When a task needs file access, use one of the tools described below.\n\n"
	case hasTools:
		closing = "When a task needs file access, use one of the caller's tools.\n\n"
	default:
		closing = "When a task needs file access, use an available tool. If none is wired up for it, say so directly.\n\n"
	}
	switch runtime.GOOS {
	case "windows":
		return "[context] Runtime: the caller's own Windows PC. This gateway is hosted locally on that PC, " +
			"so local and mapped drive paths such as C:\\, D:\\ and E:\\ refer to files on the caller's machine. " +
			"The runtime stays the same for the whole conversation. " + closing
	case "darwin":
		return "[context] Runtime: the caller's own Mac. This gateway is hosted locally on that machine, " +
			"so local paths refer to files on the caller's filesystem. " +
			"The runtime stays the same for the whole conversation. " + closing
	default:
		return "[context] Runtime: the caller's own machine. This gateway is hosted locally on that machine, " +
			"so local paths refer to files on the caller's filesystem. " +
			"The runtime stays the same for the whole conversation. " + closing
	}
}

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
//
// Both tool branches are phrased affirmatively, for the same two reasons
// environmentPrompt above is. They used to carry a row of prohibitions naming a
// code interpreter, a Python sandbox and a cloud execution environment, which is
// the self-sabotage documented on the web runtime prompt -- a prohibition that
// spells out the wrong answer makes it the most prominent option in context --
// and "Do NOT ..." stacked five times is the jailbreak-shaped phrasing that
// Microsoft's filter has rejected outright, losing the whole turn.
//
// Each prohibition was replaced by the positive instruction it was reaching for,
// so the steering survives: "to run code, call the bash tool" covers both the
// interpreter ban and the ```python ban, "active and callable right now" covers
// "do not say a tool is unavailable", "answer from its result" covers "do not
// output environment diagnostics", and "that fenced block is the entire call and
// stands on its own" covers the XML/prose wrapper ban.
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool, toolsDeclared, schemasInText bool) string {
	// tool_choice=none is an explicit instruction not to call anything, so the
	// absent-tool clause is appropriate again even when tools are declared.
	choiceIsNone := strings.EqualFold(fmt.Sprint(choice), "none")
	if len(tools) == 0 || choiceIsNone {
		hasTools := toolsDeclared && !choiceIsNone
		return environmentPrompt(hasTools, hasTools && schemasInText) + text
	}
	if hasPlugins {
		return environmentPrompt(true, false) + fmt.Sprintf("[system] The caller has provided real tools (bash, read, edit, write, glob, grep, etc.) that run locally through this gateway. They are active and callable right now, and they are the execution path for commands, code, file reads and every other filesystem operation. To run code, call the bash tool. When you decide to use a tool, call it immediately and answer from its result.\n\n%s", text)
	}
	var defs []string
	var badJSON, noName int
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil {
			badJSON++
			continue
		}
		if f.Name == "" {
			noName++
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	// A dropped declaration is silent otherwise: the model simply never sees that
	// tool, and if every one drops we return the same prompt as a caller who
	// declared nothing. That made a gateway-side parse bug indistinguishable from
	// an empty client request. Counts and reasons only -- schemas can carry paths.
	if dropped := badJSON + noName; dropped > 0 {
		log.Printf("chathub tool-defs dropped=%d of %d bad_json=%d no_name=%d kept=%d", dropped, len(tools), badJSON, noName, len(defs))
	}
	if len(defs) == 0 {
		// Every declaration failed to parse, so there is genuinely nothing for the
		// model to call and the absent-tool clause is the honest closing again.
		return environmentPrompt(false, false) + text
	}
	return environmentPrompt(true, true) + fmt.Sprintf("You are an execution agent on that machine. The tools below are real, active, and callable right now, and they are the execution path for commands, code and filesystem access. To run code, call the bash tool.\nWhen the user's request requires a tool, call it by emitting ONLY one fenced block whose info string is the exact tool name and whose body is a JSON object of arguments. That fenced block is the entire call and stands on its own. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", strings.Join(defs, "\n\n"), text)
}
