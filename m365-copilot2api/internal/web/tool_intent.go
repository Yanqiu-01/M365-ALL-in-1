package web

import (
	"strings"
	"unicode"
)

// toolIntentActions are verbs that usually introduce a concrete action rather
// than a knowledge question. They are matched with a trailing space so that
// "read " does not fire on "already".
var toolIntentActions = []string{
	"call ", "invoke ", "use ", "run ", "execute ", "open ", "read ", "write ",
	"edit ", "modify ", "create ", "delete ", "remove ", "list ", "search ",
	"find ", "look up ", "check ", "test ", "install ", "download ", "upload ",
	"send ", "save ", "generate ", "update ", "rename ", "move ", "copy ",
	"commit ", "push ", "pull ", "inspect ", "implement ", "debug ", "refactor ",
	"fix ", "repair ", "patch ", "audit ", "review ", "diagnose ",
	"fix this", "apply the patch", "apply patch",
	"查看", "读取", "写入", "编辑", "修改", "创建", "删除", "移除", "列出",
	"搜索", "查找", "运行", "执行", "打开", "测试", "安装", "下载", "上传",
	"保存", "生成", "更新", "重命名", "移动", "复制", "提交", "推送", "拉取",
	"检查", "调用", "使用", "修复", "审计", "排查", "处理", "帮我改", "帮我修", "帮我看", "看下代码", "打开项目",
}

var toolIntentNegations = []string{
	"do not use a tool", "don't use a tool", "without a tool", "no tool", "no tools",
	"do not call any tool", "don't call any tool",
	"无需工具", "不需要工具", "不要调用工具", "不要使用工具", "不用工具",
}

// toolIntentGenericNameParts are tool-name fragments that carry no intent on
// their own. Without this stoplist a declared read_file tool would classify
// "Explain what a file is." as an action request.
var toolIntentGenericNameParts = map[string]bool{
	"file": true, "files": true, "path": true, "paths": true, "text": true,
	"data": true, "info": true, "item": true, "items": true, "list": true,
	"line": true, "lines": true, "code": true, "name": true, "names": true,
	"value": true, "values": true, "thing": true, "things": true,
	"result": true, "results": true, "input": true, "output": true,
	"string": true, "number": true, "json": true, "object": true,
	"message": true, "content": true, "context": true, "request": true,
	"response": true, "entry": true, "field": true, "tool": true,
	"read": true, "write": true, "edit": true, "open": true, "call": true,
	"create": true, "delete": true, "remove": true, "update": true,
	"search": true, "find": true, "fetch": true, "send": true, "save": true,
	"check": true, "test": true, "exec": true, "execute": true, "run": true,
	"move": true, "copy": true, "load": true, "store": true, "query": true,
	"apply": true, "make": true, "build": true, "start": true, "stop": true,
}

// toolIntentRequestLeads mark an explicit request even inside a question, e.g.
// "Can you read the file?".
var toolIntentRequestLeads = []string{
	"please", "can you", "could you", "would you", "will you", "help me",
	"let's", "let us", "i need you to", "i want you to", "go ahead and",
	"请", "帮我", "帮忙", "麻烦", "给我", "替我", "你能", "可以帮",
}

// toolIntentQuestionLeads mark a knowledge question. Combined with the absence
// of a request lead they suppress the verb heuristic so that "What does this
// library use for hashing?" is answered instead of forced into a tool call.
var toolIntentQuestionLeads = []string{
	"what ", "what's", "why ", "how ", "when ", "who ", "which ", "where ",
	"does ", "do you", "is ", "are ", "can i", "should i", "explain ",
	"describe ", "tell me about", "什么", "为什么", "怎么", "如何", "是不是",
	"哪些", "哪个", "介绍", "解释",
}

// latestUserIntent keeps the heuristic focused on the current user turn. The
// router prompt also contains system/developer instructions and prior tool
// evidence, which must not by themselves force a new call.
func latestUserIntent(messages []oaiMsg, fallback string) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			if text := strings.TrimSpace(contentToString(messages[i].Content)); text != "" {
				return text
			}
		}
	}
	return fallback
}

func hasLocalWorkMarker(prompt string) bool {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return false
	}
	if strings.Contains(text, `:\`) || strings.Contains(text, `:/`) {
		return true
	}
	if strings.Contains(text, "/home/") || strings.Contains(text, "/Users/") || strings.Contains(text, "./src/") {
		return true
	}
	lower := strings.ToLower(text)
	for _, ext := range []string{".go", ".py", ".json", ".ts", ".js"} {
		idx := strings.Index(lower, ext)
		if idx <= 0 {
			continue
		}
		prev := lower[idx-1]
		if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || prev == '/' || prev == '\\' || prev == '.' || prev == '_' || prev == '-' {
			return true
		}
	}
	return false
}

func containsAnyMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// declaredToolNameHit reports whether the text names a declared tool, either in
// full or through a distinctive fragment of its name. Generic fragments are
// ignored so that ordinary vocabulary never selects a tool.
func declaredToolNameHit(text string, tools []map[string]any) bool {
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if len(name) >= 4 && strings.Contains(text, name) {
			return true
		}
		for _, part := range strings.FieldsFunc(name, func(r rune) bool {
			return r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r)
		}) {
			if len(part) >= 4 && !toolIntentGenericNameParts[part] && strings.Contains(text, part) {
				return true
			}
		}
	}
	return false
}

// ledgerAnswersIntent reports whether this turn can already be answered from
// tool evidence that was gathered earlier in the same conversation.
//
// latestUserIntent deliberately returns the newest *user* turn, which in an
// agentic client stays the original request for the whole exchange. So on the
// turn whose only remaining job is to report the tool result, toolIntentLikely
// still sees "open inventory.txt ..." and reports an action request. The router
// then reads the model's well-formed NO_TOOL_NEEDED as a mistake and retries
// under tool_choice=required -- telling a model that has nothing left to call
// that it must call something. Measured against a clean Claude CLI run on
// /v1/messages: the third turn spent 27.7s on a retry that ended
// retry_exhausted before falling back to the answer it already had.
//
// A completed, non-failed call is the evidence that makes prose the correct
// output. Pending calls are not: results are still owed, so the turn is not
// answerable and the existing behaviour is left untouched. When every completed
// call failed, a retry can still legitimately change strategy, so the guard
// stays out of the way there too.
//
// "Some call succeeded" is necessary but not sufficient, and measuring it alone
// was wrong. On a clean Claude CLI run asking for two files, inventory.txt was
// read, config.ini was not, and the model answered "I still need to read
// config.ini -- could you share its contents?" with disposition
// intent_answered_from_ledger: one success had switched the guard on while half
// the request was still outstanding. So when the request names concrete targets,
// every named target must appear in the completed evidence before prose is
// accepted as the finished answer.
func ledgerAnswersIntent(prompt string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	succeeded := false
	for _, e := range l.Completed {
		if !e.Failed {
			succeeded = true
			break
		}
	}
	if !succeeded {
		return false
	}
	return namedTargetsCovered(prompt, l)
}

// namedTargetsCovered reports whether every concrete target the request names is
// present in the arguments of a completed, non-failed call.
//
// Only unambiguous targets count. A bare word cannot be checked -- "read the
// config" names nothing a ledger can be compared against -- so the test is
// restricted to file-like tokens, which is what an agentic client's requests
// actually carry. A request naming none of them falls back to the plain
// "something succeeded" reading, preserving the original repair.
func namedTargetsCovered(prompt string, l agentLedger) bool {
	targets := namedFileTargets(prompt)
	if len(targets) == 0 {
		return true
	}
	var evidence strings.Builder
	for _, e := range l.Completed {
		if e.Failed {
			continue
		}
		evidence.WriteString(strings.ToLower(e.Arguments))
		evidence.WriteByte('\n')
	}
	haystack := evidence.String()
	for _, target := range targets {
		if !strings.Contains(haystack, target) {
			return false
		}
	}
	return true
}

// namedFileTargets extracts lowercased file-like tokens from the request.
//
// The match is deliberately narrow: a token has to carry a dot followed by a
// short alphanumeric extension to count. Paths are reduced to their base name so
// that a request written as E:\dir\config.ini still matches a call recorded as
// {"path":"config.ini"}, which is how a client that already knows its working
// directory issues the call.
func namedFileTargets(prompt string) []string {
	fields := strings.FieldsFunc(strings.ToLower(prompt), func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '\'' || r == '`' ||
			r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' ||
			r == ',' || r == ';' || r == '<' || r == '>'
	})
	seen := map[string]bool{}
	var out []string
	for _, field := range fields {
		token := strings.Trim(field, ".:?!*")
		if idx := strings.LastIndexAny(token, `/\`); idx >= 0 {
			token = token[idx+1:]
		}
		dot := strings.LastIndex(token, ".")
		if dot <= 0 || dot == len(token)-1 {
			continue
		}
		ext := token[dot+1:]
		if len(ext) < 1 || len(ext) > 5 {
			continue
		}
		// An extension is alphanumeric and holds at least one letter. The letter
		// requirement is what keeps version numbers ("upgrade to 1.24", "python 3.12")
		// out: no real extension is all digits, so nothing is lost by excluding them.
		alnum := true
		hasLetter := false
		for _, r := range ext {
			switch {
			case unicode.IsLetter(r):
				hasLetter = true
			case unicode.IsDigit(r):
			default:
				alnum = false
			}
			if !alnum {
				break
			}
		}
		if !alnum || !hasLetter || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

// shouldRetryForToolIntent is the single gate both router paths use before
// spending an extra upstream round on a constrained retry.
func shouldRetryForToolIntent(prompt string, tools []map[string]any, l agentLedger) bool {
	if ledgerAnswersIntent(prompt, l) {
		return false
	}
	return toolIntentLikely(prompt, tools)
}

// toolIntentLikely is deliberately conservative: it only repairs an auto
// decision when the user named a declared tool or issued an action request.
// Knowledge questions stay ordinary answers even when tools are declared.
func toolIntentLikely(prompt string, tools []map[string]any) bool {
	if len(tools) == 0 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(prompt))
	if text == "" {
		return false
	}
	if containsAnyMarker(text, toolIntentNegations) {
		return false
	}
	if declaredToolNameHit(text, tools) {
		return true
	}
	if hasLocalWorkMarker(prompt) {
		return true
	}
	if !containsAnyMarker(text, toolIntentActions) {
		return false
	}
	if containsAnyMarker(text, toolIntentRequestLeads) {
		return true
	}
	if strings.HasSuffix(text, "?") || strings.HasSuffix(text, "？") {
		return false
	}
	for _, lead := range toolIntentQuestionLeads {
		if strings.HasPrefix(text, lead) {
			return false
		}
	}
	return true
}
