package web

import (
	"fmt"
	"strings"
	"testing"
)

func TestResponsesToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "what time", Tools: []map[string]any{{"type": "function", "name": "clock", "parameters": map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 1 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

// 名字曾叫 ...PropagatesToGateway，但它只覆盖 responsesRequest -> oaiReq 的结构体
// 转换，并不验证到网关或上游的传递。真正落实这个字段语义的是
// enforceParallelToolCalls，见 toolloop_parallel_test.go。
func TestResponsesParallelToolCallsReachesOAIRequest(t *testing.T) {
	enabled := true
	r := responsesRequest{
		Model:             "gpt-5.6-reasoning",
		Input:             "inspect both files",
		ParallelToolCalls: &enabled,
		Tools: []map[string]any{
			{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}},
			{"type": "function", "name": "list_files", "parameters": map[string]any{"type": "object"}},
		},
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ParallelToolCalls == nil || !*o.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls was lost during Responses conversion: %#v", o.ParallelToolCalls)
	}
	if len(o.Tools) != 2 {
		t.Fatalf("tools=%d, want 2", len(o.Tools))
	}
}

func TestResponsesParallelToolCallsFalseIsPreserved(t *testing.T) {
	disabled := false
	r := responsesRequest{Input: "run sequentially", ParallelToolCalls: &disabled}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ParallelToolCalls == nil || *o.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls=false was not preserved: %#v", o.ParallelToolCalls)
	}
}

func TestResponsesCustomExecToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "inspect", Tools: []map[string]any{{"type": "custom", "name": "exec", "description": "run a command", "format": map[string]any{"type": "grammar"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Tools) != 1 || o.Tools[0].Type != "custom" {
		t.Fatalf("tools=%+v err=%v", o.Tools, err)
	}
	if string(o.Tools[0].Function) == "" || !containsJSON(o.Tools[0].Function, "input") {
		t.Fatalf("custom exec did not receive an input schema: %s", o.Tools[0].Function)
	}
}

// 这个测试原名 TestResponsesCustomExecIsExclusiveTool，断言的是
// len(o.Tools) == 1 —— 它 pin 住的是 bug 本身：转换器当年一旦看到 custom/exec
// 就把其余声明全丢掉，测试照着这个行为写，于是「只剩 exec」成了被保护的契约。
// 真实后果是 Codex 声明 exec + apply_patch + read_file + MCP namespace 之后模型
// 手上只有 exec，然后照实说「read_file 不可用」。
//
// 现在断言修正后的契约：混合声明全部存活、顺序不变，并且 exec 不是唯一工具时
// 注入的系统提示词不能声称互斥。exec 独占的覆盖挪到下面 ...IsSoleTool。
func TestResponsesCustomExecCoexistsWithOtherTools(t *testing.T) {
	r := responsesRequest{Input: "edit the project", Tools: []map[string]any{
		{"type": "custom", "name": "exec", "description": "local execution"},
		{"type": "function", "name": "read_file", "description": "read a file", "parameters": map[string]any{"type": "object"}},
		{"type": "namespace", "name": "mcp__docs", "tools": []any{
			map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}},
		}},
		{"type": "web_search"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	// 声明顺序必须原样保留：exec、普通 function、namespace 子工具（带命名空间
	// 前缀）、web_search 直通。
	wantTypes := []string{"custom", "function", "function", "web_search"}
	wantNames := []string{"exec", "read_file", "mcp__docs__search", ""}
	if len(o.Tools) != len(wantTypes) {
		t.Fatalf("tools=%d (%#v), want %d: mixed declarations were dropped by the converter", len(o.Tools), o.Tools, len(wantTypes))
	}
	for i, tool := range o.Tools {
		if tool.Type != wantTypes[i] {
			t.Fatalf("tools[%d].Type=%q, want %q (declaration order changed): %#v", i, tool.Type, wantTypes[i], o.Tools)
		}
		if wantNames[i] != "" && !containsJSON(tool.Function, "name") {
			t.Fatalf("tools[%d] lost its name: %s", i, tool.Function)
		}
		if wantNames[i] != "" && !strings.Contains(string(tool.Function), `"`+wantNames[i]+`"`) {
			t.Fatalf("tools[%d]=%s, want name %q", i, tool.Function, wantNames[i])
		}
	}
	policy := fmt.Sprint(o.Messages[0].Content)
	if !strings.Contains(policy, "Never use, request, or mention Microsoft 365/Copilot native tools") {
		t.Fatalf("missing native-tool prohibition: %#v", o.Messages)
	}
	// 关键回归点：调用方声明了别的工具时，提示词不能再宣布 exec 独占，否则模型会
	// 拒绝调用方自己声明的工具 —— 光改转换器不改提示词等于没修。
	if strings.Contains(policy, customExecSoleToolInstruction) {
		t.Fatalf("policy claims exec exclusivity while other tools are declared: %q", policy)
	}
	if strings.Contains(policy, "only permitted execution tool") || strings.Contains(policy, "only tool available") {
		t.Fatalf("policy still asserts exclusivity: %q", policy)
	}
}

func TestResponsesCustomExecPolicyStatesExclusivityOnlyWhenSoleTool(t *testing.T) {
	r := responsesRequest{Input: "inspect the project", Tools: []map[string]any{
		{"type": "custom", "name": "exec", "description": "local execution"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 1 || o.Tools[0].Type != "custom" {
		t.Fatalf("tools=%#v, want only custom exec", o.Tools)
	}
	policy := fmt.Sprint(o.Messages[0].Content)
	if !strings.Contains(policy, customExecSoleToolInstruction) {
		t.Fatalf("exec-only request lost the sole-tool statement: %q", policy)
	}
	if !strings.Contains(policy, "Never use, request, or mention Microsoft 365/Copilot native tools") {
		t.Fatalf("missing native-tool prohibition: %q", policy)
	}
}

// 非 exec 的 custom 工具此前会被静默丢掉：[custom/apply_patch] 单独声明时转换结果
// 跟「没有工具」完全一样。现在走同一套 {"input": string} 桥接，Tool.Type 保持
// custom，这样响应路径才会把它序列化成 custom_tool_call 而不是 function_call。
func TestResponsesNonExecCustomToolSurvives(t *testing.T) {
	r := responsesRequest{Input: "apply the patch", Tools: []map[string]any{
		{"type": "custom", "name": "apply_patch", "description": "apply a patch", "format": map[string]any{"type": "grammar"}},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 1 || o.Tools[0].Type != "custom" {
		t.Fatalf("tools=%#v, want the custom apply_patch declaration to survive", o.Tools)
	}
	if !strings.Contains(string(o.Tools[0].Function), `"apply_patch"`) || !containsJSON(o.Tools[0].Function, "input") {
		t.Fatalf("apply_patch did not receive the custom input schema: %s", o.Tools[0].Function)
	}
	// exec 不在场，就不该注入工作区策略：那段提示词的每一条都要求用 exec 核实。
	for _, m := range o.Messages {
		if strings.Contains(fmt.Sprint(m.Content), "OpenCode execution bridge") {
			t.Fatalf("workspace policy injected without a custom exec tool: %#v", o.Messages)
		}
	}
}

// 无名 custom 声明是唯一仍被丢掉的形状：clientPlugins 会跳过没有 name 的工具，
// 校验器也没法把模型的调用解析回一条无名声明，留着只会变成幽灵条目。
func TestResponsesUnnamedCustomToolIsDropped(t *testing.T) {
	r := responsesRequest{Input: "do something", Tools: []map[string]any{
		{"type": "custom", "description": "no name at all"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 0 {
		t.Fatalf("tools=%#v, want an unnamed custom declaration to be dropped", o.Tools)
	}
}

func TestResponsesInstructionsAndCustomExecPolicyAreSystemMessages(t *testing.T) {
	r := responsesRequest{
		Instructions: "Use the repository selected by the caller.",
		Input:        "inspect the repository",
		Tools:        []map[string]any{{"type": "custom", "name": "exec", "description": "run a command"}},
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 {
		t.Fatalf("messages=%#v", o.Messages)
	}
	// exec 是这里唯一的工具，所以整条策略 = 常驻段 + 独占段。断言组装结果而不是
	// 硬编码文本，提示词改字时这个测试只关心顺序，不会连带失败。
	if o.Messages[0].Role != "system" || o.Messages[0].Content != customExecInstruction(true) {
		t.Fatalf("missing custom exec policy: %#v", o.Messages[0])
	}
	if o.Messages[1].Role != "system" || o.Messages[1].Content != r.Instructions {
		t.Fatalf("instructions not preserved: %#v", o.Messages[1])
	}
	if o.Messages[2].Role != "user" || o.Messages[2].Content != r.Input {
		t.Fatalf("input ordering changed: %#v", o.Messages[2])
	}
}

func TestResponsesCustomToolOutputToOpenAI(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "custom_tool_call", "call_id": "call_exec", "name": "exec", "input": "uname -s"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec", "output": "Linux"},
	}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[0].Role != "assistant" || o.Messages[0].ToolCalls[0]["type"] != "custom" || o.Messages[1].Role != "tool" || o.Messages[1].ToolCallID != "call_exec" {
		t.Fatalf("messages=%+v err=%v", o.Messages, err)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("custom tool continuation rejected: %v", err)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	r := anthropicRequest{Model: "m", System: any("be concise"), Messages: []anthropicMessage{{Role: "user", Content: any("weather")}}, Tools: []anthropicTool{{Name: "weather", InputSchema: map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestAnthropicToolResult(t *testing.T) {
	r := anthropicRequest{Messages: []anthropicMessage{{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "x", "name": "f", "input": map[string]any{}}}}, {Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "ok"}}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[1].ToolCallID != "x" {
		t.Fatalf("%+v %v", o, err)
	}
}
