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
