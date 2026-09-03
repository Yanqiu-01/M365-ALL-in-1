package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesNamespaceCallIsUnflattened(t *testing.T) {
	output := responseNamespaceOutput(t, "multi_agent_v1__spawn_agent", codexNamespaceTools())
	item := output[0].(map[string]any)
	if item["name"] != "spawn_agent" || item["namespace"] != "multi_agent_v1" {
		t.Fatalf("namespace call = name=%v namespace=%v", item["name"], item["namespace"])
	}
}

func TestResponsesLongestNamespaceWins(t *testing.T) {
	output := responseNamespaceOutput(t, "mcp__codebase_memory_mcp__search_code", codexNamespaceTools())
	item := output[0].(map[string]any)
	if item["name"] != "search_code" || item["namespace"] != "mcp__codebase_memory_mcp" {
		t.Fatalf("namespace call = name=%v namespace=%v", item["name"], item["namespace"])
	}
}

func TestResponsesFlatFunctionNameWithSeparatorIsNotSplit(t *testing.T) {
	output := responseNamespaceOutput(t, "legacy__helper", codexNamespaceTools())
	item := output[0].(map[string]any)
	if item["name"] != "legacy__helper" || item["namespace"] != nil {
		t.Fatalf("flat function changed: %v", item)
	}
}

func TestResponsesNoDeclaredNamespacesLeavesNameAlone(t *testing.T) {
	output := responseNamespaceOutput(t, "multi_agent_v1__spawn_agent", nil)
	item := output[0].(map[string]any)
	if item["name"] != "multi_agent_v1__spawn_agent" || item["namespace"] != nil {
		t.Fatalf("undeclared namespace changed: %v", item)
	}
}

func TestStreamResponsesAdapterUnflattensNamespaceCalls(t *testing.T) {
	body := funcBody(t, readSourceFile(t, "protocol_handlers.go"), "streamResponsesAdapter")
	if !strings.Contains(body, "applyToolNamespace(item, namespaces)") {
		t.Fatal("streaming path does not restore namespace calls")
	}
}

func responseNamespaceOutput(t *testing.T, name string, tools []map[string]any) []any {
	t.Helper()
	src := map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content": "",
		"tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": name, "arguments": `{}`},
		}},
	}}}}
	var got map[string]any
	w := &writerFunc{write: func(p []byte) (int, error) {
		_ = json.Unmarshal(p, &got)
		return len(p), nil
	}}
	writeResponsesResult(w, "m365-copilot", false, src, declaredToolNamespaces(tools))
	output, _ := got["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output length = %d", len(output))
	}
	return output
}

func codexNamespaceTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "name": "exec_command"},
		{"type": "namespace", "name": "multi_agent_v1"},
		{"type": "namespace", "name": "mcp__codebase_memory_mcp"},
	}
}

type writerFunc struct{ write func([]byte) (int, error) }

func (w *writerFunc) Header() http.Header         { return http.Header{} }
func (w *writerFunc) Write(p []byte) (int, error) { return w.write(p) }
func (w *writerFunc) WriteHeader(int)             {}
