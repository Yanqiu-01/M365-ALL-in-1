package web

import "testing"

func TestLatestUserIntentIgnoresPriorEvidence(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "Use tools when needed."},
		{Role: "user", Content: "Please read the old file."},
		{Role: "tool", Content: "read complete"},
		{Role: "user", Content: "What is two plus two?"},
	}
	if got := latestUserIntent(messages, "fallback"); got != "What is two plus two?" {
		t.Fatalf("latestUserIntent=%q", got)
	}
}

func TestLatestUserIntentFallsBackWhenNoUserTurn(t *testing.T) {
	messages := []oaiMsg{{Role: "system", Content: "be brief"}}
	if got := latestUserIntent(messages, "fallback"); got != "fallback" {
		t.Fatalf("latestUserIntent=%q, want fallback", got)
	}
}

func TestToolIntentLikelyRepairsExplicitActions(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "read_file"}}}
	for _, prompt := range []string{
		"Please read the file at C:/tmp/a.txt",
		"请读取这个文件",
		"帮我看一下这个目录，然后读取配置",
		"Use read_file for this request",
		"Read the config and summarize it.",
		"Can you read the file for me?",
	} {
		if !toolIntentLikely(prompt, tools) {
			t.Fatalf("tool intent not detected for %q", prompt)
		}
	}
}

func TestToolIntentLikelyDoesNotForceOrdinaryQuestion(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "read_file"}}}
	for _, prompt := range []string{
		"What is two plus two?",
		"Explain what a file is.",
		"What does this library use for hashing?",
		"Why do people write unit tests?",
		"什么是文件描述符",
		"Do not use a tool; answer from your knowledge.",
		"无需工具，直接告诉我答案",
	} {
		if toolIntentLikely(prompt, tools) {
			t.Fatalf("ordinary question was classified as tool intent: %q", prompt)
		}
	}
}

func TestToolIntentLikelyUsesDeclaredNameSemantics(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}}
	if !toolIntentLikely("What is the weather in Beijing?", tools) {
		t.Fatal("weather question should match the declared weather tool")
	}
	if toolIntentLikely("What is the capital of France?", tools) {
		t.Fatal("unrelated question should not match the weather tool")
	}
}

func TestToolIntentLikelyRequiresDeclaredTools(t *testing.T) {
	if toolIntentLikely("Please read the file", nil) {
		t.Fatal("intent must never be reported without declared tools")
	}
}

func TestToolIntentLikelyDetectsLocalWork(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "read_file"}}}
	for _, prompt := range []string{
		"Open C:\\Users\\ad\\project\\main.go",
		"Look at ./src/app.py",
		"Fix this bug in /home/ad/app.py",
		"Implement the handler",
		"Debug the failing test",
		"Refactor the parser",
		"Apply the patch to the gateway",
		"Repair the failing test",
		"Patch the parser's terminal-frame handling",
		"Audit the tool router",
		"帮我改这段代码",
		"修复这个工具调用问题",
		"审计工具路由实现",
		"打开项目看下代码",
	} {
		if !toolIntentLikely(prompt, tools) {
			t.Fatalf("local work intent not detected for %q", prompt)
		}
	}
}
