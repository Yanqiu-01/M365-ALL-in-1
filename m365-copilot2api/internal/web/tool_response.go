package web

import (
	"m365-copilot2api/internal/chathub"
	"net/http"
	"time"
)

// toolCallMaps 把抽取结果转成 OpenAI tool_calls 数组形状。
func toolCallMaps(calls []detectedToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{"id": c.ID, "type": typ, "function": map[string]any{"name": c.Name, "arguments": string(c.Arguments)}})
	}
	return out
}

func writeToolResponse(w http.ResponseWriter, id, model string, stream bool, calls []detectedToolCall, res chathub.Result) error {
	toolCalls := toolCallMaps(calls)
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
	if res.Reasoning != "" {
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
	}
	pt := EstimateTokens(res.Text)
	for _, tc := range calls {
		pt += EstimateTokens(string(tc.Arguments))
	}
	ct := EstimateTokens(res.Text)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		emit := func(v any) error {
			return sseDataRaw(w, flusher, mustJSON(v))
		}
		base := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		firstDelta := map[string]any{"role": "assistant", "content": nil}
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			firstDelta["reasoning_content"] = reasoning
		}
		if err := emit(base(firstDelta, nil)); err != nil {
			return err
		}
		for i, tc := range calls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			if err := emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": string(tc.Arguments)}}}}, nil)); err != nil {
				return err
			}
		}
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
		if err := sseSafeRaw(w, flusher, "data: "+mustJSON(usageChunk)+"\n\n"); err != nil {
			return err
		}
		if err := sseSafeRaw(w, flusher, "data: [DONE]\n\n"); err != nil {
			return err
		}
		return nil
	}
	jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "m365": compatM365Metadata(res), "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}})
	return nil
}
