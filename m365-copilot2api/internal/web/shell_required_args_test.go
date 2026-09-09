package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// ompShellTools 复刻 omp 客户端的 bash 声明：required 含 "i"（concise intent）。
// 这正是 2026-09-08 当天 24 次 "missing required argument i" 拒收的来源。
func ompShellTools() []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"i":       map[string]any{"type": "string", "description": "concise intent"},
					"cwd":     map[string]any{"type": "string"},
					"timeout": map[string]any{"type": "number"},
				},
				"required": []any{"command", "i"},
			},
		},
	}}
}

func decodeArgs(t *testing.T, call detectedToolCall) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	return args
}

// 纯命令围栏（```bash\nls -la\n```）没有 JSON 参数，参数全由网关合成。
// 合成时必须照 schema 补齐 required，否则校验必拒、模型永远看不到结果。
func TestPlainCommandFenceFillsRequiredFields(t *testing.T) {
	tools := ompShellTools()
	calls := fencedToolCalls("```bash\nls -la\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	args := decodeArgs(t, calls[0])
	if args["command"] != "ls -la" {
		t.Fatalf("command=%v", args["command"])
	}
	if intent, ok := args["i"].(string); !ok || strings.TrimSpace(intent) == "" {
		t.Fatalf("required field i missing or empty: %#v", args["i"])
	}
	// 补齐后必须真能通过校验 —— 这才是「不再被拒」的直接证据。
	valid, rejected := validateDetectedToolCalls(calls, tools, "auto")
	if len(valid) != 1 || len(rejected) != 0 {
		t.Fatalf("valid=%d rejected=%+v", len(valid), rejected)
	}
}

// 带 JSON 的围栏漏掉必填字段时同样要补齐，且模型已给的键一个都不能动。
func TestJSONFenceKeepsModelKeysAndFillsMissingRequired(t *testing.T) {
	tools := ompShellTools()
	calls := fencedToolCalls("```bash\n{\"command\":\"grep -rn foo .\",\"cwd\":\"E:/x\"}\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	args := decodeArgs(t, calls[0])
	if args["cwd"] != "E:/x" {
		t.Fatalf("model-provided cwd was lost: %#v", args)
	}
	if intent, ok := args["i"].(string); !ok || strings.TrimSpace(intent) == "" {
		t.Fatalf("required field i not filled: %#v", args)
	}
	valid, rejected := validateDetectedToolCalls(calls, tools, "auto")
	if len(valid) != 1 || len(rejected) != 0 {
		t.Fatalf("valid=%d rejected=%+v", len(valid), rejected)
	}
}

// 模型自己写了必填字段时，网关不得覆盖它。
func TestModelSuppliedRequiredFieldIsNotOverwritten(t *testing.T) {
	tools := ompShellTools()
	calls := fencedToolCalls("```bash\n{\"command\":\"ls\",\"i\":\"list the directory\"}\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	if got := decodeArgs(t, calls[0])["i"]; got != "list the directory" {
		t.Fatalf("model intent overwritten: %#v", got)
	}
}

// 裸 JSON 兜底与围栏是同一条决策语义，补齐行为必须一致。
func TestBareJSONFallbackFillsRequiredFields(t *testing.T) {
	tools := ompShellTools()
	calls := fencedToolCalls("{\"command\":\"wc -l x.txt\"}\n", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	if intent, ok := decodeArgs(t, calls[0])["i"].(string); !ok || strings.TrimSpace(intent) == "" {
		t.Fatalf("required field i not filled on bare-JSON path")
	}
}

// 没有 required 的声明不得被塞进多余的键：补齐只针对 schema 真的要求的字段。
func TestNoRequiredMeansNoSynthesizedExtras(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
			},
		},
	}}
	calls := fencedToolCalls("```bash\nls\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	args := decodeArgs(t, calls[0])
	if len(args) != 1 || args["command"] != "ls" {
		t.Fatalf("unexpected synthesized args: %#v", args)
	}
}

// 非字符串的必填字段没有可诚实派生的值，网关不得瞎编 —— 留给校验拒收并在
// 修复轮如实告知模型。
func TestNonStringRequiredFieldIsNotFabricated(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"budget":  map[string]any{"type": "number"},
				},
				"required": []any{"command", "budget"},
			},
		},
	}}
	calls := fencedToolCalls("```bash\nls\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	if _, present := decodeArgs(t, calls[0])["budget"]; present {
		t.Fatal("gateway fabricated a value for a non-string required field")
	}
}

// 声明了 default 的必填字段按 default 补，而不是塞命令摘要。
func TestRequiredFieldWithDefaultUsesDeclaredDefault(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"mode":    map[string]any{"type": "string", "default": "safe"},
				},
				"required": []any{"command", "mode"},
			},
		},
	}}
	calls := fencedToolCalls("```bash\nls\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	if got := decodeArgs(t, calls[0])["mode"]; got != "safe" {
		t.Fatalf("declared default not used: %#v", got)
	}
}

// 修复指令必须如实说出拒收原因，并带上该工具的 schema。写死「你选错工具了」
// 是修不好「缺必填参数」的 —— 模型会换工具而不是补字段。
func TestRepairInstructionReportsRealReasonAndSchema(t *testing.T) {
	tools := ompShellTools()
	got := toolRejectionRepairInstruction(
		[]rejectedToolCall{{Name: "bash", Reason: "missing required argument i"}}, tools)
	for _, want := range []string{"bash", "missing required argument i", "\"required\"", "concise intent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("repair instruction missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "selected an undeclared tool") {
		t.Fatalf("repair instruction still hardcodes the wrong reason:\n%s", got)
	}
}

// 未声明工具这类拒收仍然要如实转达（这条原因本来就是对的）。
func TestRepairInstructionKeepsUndeclaredToolReason(t *testing.T) {
	got := toolRejectionRepairInstruction(
		[]rejectedToolCall{{Name: "unknown_tool", Reason: "tool was not declared by the client"}}, ompShellTools())
	if !strings.Contains(got, "unknown_tool") || !strings.Contains(got, "not declared") {
		t.Fatalf("undeclared-tool reason lost:\n%s", got)
	}
}

// P4 回归：路径类必填字段不得被塞命令摘要。cwd 的诚实值只能来自模型或
// default；网关代填 "run: ls" 会被当工作目录使用（2026-09-09 审计实锤）。
func TestPathLikeRequiredFieldNotPolluted(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "bash",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"cwd":     map[string]any{"type": "string"},
				},
				"required": []any{"command", "cwd"},
			},
		},
	}}
	calls := fencedToolCalls("```bash\nls -la\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	if _, present := decodeArgs(t, calls[0])["cwd"]; present {
		t.Fatal("cwd was fabricated by the gateway")
	}
}

func TestRepairInstructionEmptyWithoutRejections(t *testing.T) {
	if got := toolRejectionRepairInstruction(nil, ompShellTools()); got != "" {
		t.Fatalf("expected empty instruction, got %q", got)
	}
}
