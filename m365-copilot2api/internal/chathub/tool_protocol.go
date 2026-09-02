package chathub

import (
	"encoding/json"
	"fmt"
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
// including the gateway's router mode, where the schemas travel inside the text
// and Tools is deliberately empty. There the model was handed an escape hatch on
// a turn that could not possibly need one, immediately above the list of tools it
// was supposed to choose from.
func environmentPrompt(hasTools bool) string {
	var closing string
	if hasTools {
		closing = "When a task needs file access, use one of the tools described below.\n\n"
	} else {
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
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool, toolsInText bool) string {
	// tool_choice=none is an explicit instruction not to call anything, so the
	// absent-tool clause is appropriate again even when tools are declared.
	choiceIsNone := strings.EqualFold(fmt.Sprint(choice), "none")
	if len(tools) == 0 || choiceIsNone {
		return environmentPrompt(toolsInText && !choiceIsNone) + text
	}
	if hasPlugins {
		return environmentPrompt(true) + fmt.Sprintf("[system] The caller has provided real tools (bash, read, edit, write, glob, grep, etc.) that run locally through this gateway. These tools are the ONLY way to execute commands, run code, read files, or interact with the filesystem. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit ```python or ```code blocks for execution — if you need to run code, use the bash tool. Do NOT claim any tool is unavailable. Do NOT output environment diagnostics instead of tool calls. When you decide to use a tool, call it immediately.\n\n%s", text)
	}
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		// Every declaration failed to parse, so there is genuinely nothing for the
		// model to call and the absent-tool clause is the honest closing again.
		return environmentPrompt(false) + text
	}
	return environmentPrompt(true) + fmt.Sprintf("You are an execution agent on that machine. The tools below are real, active, and callable right now. Use the caller-provided tools for commands, code, and filesystem access. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit ```python or ```code blocks for execution — if you need to run code, use the bash tool.\nWhen the user's request requires a tool, call it by emitting ONLY one fenced block whose info string is the exact tool name and whose body is a JSON object of arguments. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", strings.Join(defs, "\n\n"), text)
}
