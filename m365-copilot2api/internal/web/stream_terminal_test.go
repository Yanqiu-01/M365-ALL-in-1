package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStreamTruncatedChunkMarksIncompleteOutput(t *testing.T) {
	progress := newStreamProgress(time.Now().Add(-25 * time.Millisecond))
	progress.addText("你好")
	progress.addReasoning("分析")

	chunk := streamTruncatedChunk("chatcmpl-test", "gpt-5.5-reasoning", progress)
	if got := chunk["id"]; got != "chatcmpl-test" {
		t.Fatalf("id=%v", got)
	}
	choices, ok := chunk["choices"].([]map[string]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices=%#v", chunk["choices"])
	}
	if got := choices[0]["finish_reason"]; got != "length" {
		t.Fatalf("finish_reason=%v want length", got)
	}

	top, ok := chunk["m365"].(map[string]any)
	if !ok {
		t.Fatalf("top-level m365=%#v", chunk["m365"])
	}
	if top["finished"] != false || top["truncated"] != true {
		t.Fatalf("diagnostics=%#v", top)
	}
	if top["textLen"] != 2 || top["reasoningLen"] != 2 || top["textEvents"] != 1 || top["reasoningEvents"] != 1 {
		t.Fatalf("diagnostic counters=%#v", top)
	}
	if _, ok := choices[0]["m365"].(map[string]any); !ok {
		t.Fatalf("choice m365=%#v", choices[0]["m365"])
	}

	encoded, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"finish_reason":"length"`) {
		t.Fatalf("serialized chunk=%s", encoded)
	}
}
