package web

import (
	"context"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
}

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string) {
	// 本次请求的计时。不能用包级 startedAt —— 那是服务启动时间，
	// 拿它算 DurationMs 会把每条记录写成进程已运行的总时长。
	requestStartedAt := time.Now()
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	// 与 runOpenAIAdapter 同理：内层不记账，避免一次请求两条用量记录。
	r2.Header.Set(innerAdapterHeader, "1")
	r2, innerSink := withInnerStats(r2)
	pr, pw := io.Pipe()
	// The reader end MUST be closed on every exit path, including the ones that
	// bypass <-innerDone (client disconnect, scanner error, panic in this
	// function). io.Pipe is unbuffered and io.PipeWriter.Write does not observe
	// context cancellation, so the inner goroutine parks in pw.Write forever if
	// nobody drains or closes the pipe. Its deferred releases then never run,
	// permanently stranding the process-wide chat slot, the per-account slot,
	// the client-key slot and the upstream ChatHub WebSocket. Closing pr makes
	// that pending Write fail immediately, which lets the goroutine unwind.
	defer pr.Close()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[responses] inner goroutine panic: %v", r)
			}
			_ = pw.Close()
			close(innerDone)
		}()
		s.openaiChat(irw, r2)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	emit := func(name string, v any) error {
		return writeSSE(r, w, flusher, name, v)
	}
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	type tcState struct {
		ID, Name, Args, Type string
		ItemID               string
	}
	calls := map[int]*tcState{}
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if r.Context().Err() != nil {
			return
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			if !textStarted {
				textStarted = true
				emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}})
			}
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": content})
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, _ := raw.(map[string]any)
				// Every other read from this map uses the two-value form; this one
				// used a bare assertion and was the single panic site in the loop.
				// A tool_calls delta without a numeric index is malformed, so skip
				// it rather than taking down the request.
				rawIdx, ok := tc["index"].(float64)
				if !ok {
					continue
				}
				idx := int(rawIdx)
				st := calls[idx]
				typ := "function"
				if v, ok := tc["type"].(string); ok && v == "custom" {
					typ = "custom"
				}
				if st == nil {
					prefix := "fc_"
					item := map[string]any{"type": "function_call", "call_id": "", "name": "", "arguments": "", "status": "in_progress"}
					if typ == "custom" {
						prefix = "ctc_"
						item = map[string]any{"type": "custom_tool_call", "call_id": "", "name": "", "input": "", "status": "in_progress"}
					}
					st = &tcState{ItemID: prefix + uuid.NewString(), Type: typ}
					calls[idx] = st
					item["id"] = st.ItemID
					emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})
				}
				if v, ok := tc["id"].(string); ok {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					st.Name += v
				}
				if v, ok := fn["arguments"].(string); ok {
					st.Args += v
					if st.Type != "custom" {
						emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": st.ItemID, "delta": v})
					}
				}
			}
		}
	}
	// Reading is finished either way, so release the pipe before waiting. On a
	// clean end-of-stream the inner goroutine has already closed pw and this is a
	// no-op; on a scanner error (for example a single SSE line above the 2 MiB
	// cap) the goroutine is still blocked in pw.Write, and only this close lets
	// it unwind so <-innerDone can return instead of deadlocking. The deferred
	// pr.Close above stays as the guard for the early-return paths.
	_ = pr.Close()
	<-innerDone
	if scanner.Err() != nil || irw.status >= http.StatusBadRequest {
		status := irw.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": status, "message": "inner chat request failed"},
			},
		})
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" {
		// Never leave a Responses stream after response.created without a
		// terminal event: clients otherwise render this as a successful blank
		// answer and may reuse an incomplete response on the next turn.
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": "empty_upstream_response", "message": "ChatHub returned no text or tool call"},
			},
		})
		return
	}
	output := []any{}
	if len(calls) > 0 {
		for i := 0; i < len(calls); i++ {
			st := calls[i]
			if st == nil {
				continue
			}
			if st.Type == "custom" {
				input := customToolInput(st.Args)
				item := map[string]any{"type": "custom_tool_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "input": input, "status": "completed"}
				output = append(output, item)
				emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": item["id"], "delta": input})
				emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": item["id"], "input": input})
				emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				continue
			}
			item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": "completed"}
			output = append(output, item)
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": st.ItemID, "arguments": st.Args})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	} else {
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}
		output = append(output, item)
		if !textStarted {
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": text.String()})
		}
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": messageID, "text": text.String()})
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	usageOutput := text.String()
	for _, call := range calls {
		usageOutput += call.Name + call.Args
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOutput)
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "usage": estimate.Values, "m365": localUsageMetadata(estimate.Source)}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})

	// 流式 Responses 此前完全不记账：内层被 innerAdapterHeader 挡掉，外层这条
	// 路径又没有 usage.record，于是走流式的 Codex 请求在用量表里根本不存在。
	// 缓存 token 来自内层回填 —— 只有它看得到会话历史。
	if s.usage != nil {
		s.usage.record(UsageRecord{
			Time:         time.Now(),
			APIKeyPrefix: extractAPIKey(r),
			Model:        model,
			Endpoint:     "/v1/responses",
			Stream:       true,
			InputTokens:  int64(estimate.Values["input_tokens"].(int)),
			OutputTokens: int64(estimate.Values["output_tokens"].(int)),
			CacheTokens:  innerStatsCacheTokens(innerSink),
			DurationMs:   time.Since(requestStartedAt).Milliseconds(),
			Status:       200,
		})
	}
}

// innerAdapterHeader 标记「这是一次内部委派」。
//
// /v1/messages 与 /v1/responses 都把请求转成 OpenAI 形态后回调 openaiChat，
// 而 openaiChat 自己会写一条 /v1/chat/completions 用量记录，外层再按真实端点
// 写一条 —— 于是一次请求在用量表里变成两行（messages 与 chat/completions
// 各一条，时间戳相同），token 被重复计算，路由归属也看不出来。
// 带上这个标记后内层不再记账，由外层按真实端点记一次。
const innerAdapterHeader = "X-M365-Inner-Adapter"

// isInnerAdapterCall 报告当前请求是否由另一个协议处理器内部委派而来。
func isInnerAdapterCall(r *http.Request) bool {
	return r != nil && r.Header.Get(innerAdapterHeader) == "1"
}

// innerStatsKey 在 context 里挂一个回填槽，让内层把只有它算得出来的统计量
// 交给外层。
//
// 为什么需要：历史（缓存）token 只有内层算得出来 —— 那里才有 body.Messages
// 和 session 解析结果。外层 /v1/messages、/v1/responses 手上只有转换后的请求，
// 自己重算既是重复劳动，又容易和内层口径不一致。内层跳过记账后如果不回传，
// 外层写出的记录里缓存永远是 0（面板上就是「缓0」），看起来像完全没命中缓存。
//
// 用 context 而不是响应头：流式路径的响应头早就随首个 SSE 帧发走了，等内层
// 算完历史 token 时再 Set 已经无效。
type innerStatsKeyType struct{}

var innerStatsKey innerStatsKeyType

// innerStats 承接内层回填的统计量。
type innerStats struct {
	CacheTokens int64
}

// withInnerStats 给内部委派请求挂上回填槽。
func withInnerStats(r *http.Request) (*http.Request, *innerStats) {
	sink := &innerStats{}
	return r.WithContext(context.WithValue(r.Context(), innerStatsKey, sink)), sink
}

// innerStatsSink 取出当前请求的回填槽；非内部委派时为 nil。
func innerStatsSink(r *http.Request) *innerStats {
	if r == nil {
		return nil
	}
	sink, _ := r.Context().Value(innerStatsKey).(*innerStats)
	return sink
}

// innerStatsCacheTokens 安全地读出回填的缓存 token 数。
func innerStatsCacheTokens(stats *innerStats) int64 {
	if stats == nil {
		return 0
	}
	return stats.CacheTokens
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	out, raw, status, _, err := s.runOpenAIAdapterWithStats(r, o)
	return out, raw, status, err
}

// runOpenAIAdapterWithStats 同时交出内层回填的统计量，供外层按真实端点记账。
func (s *Server) runOpenAIAdapterWithStats(r *http.Request, o oaiReq) (map[string]any, []byte, int, *innerStats, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	r2.Header.Set(innerAdapterHeader, "1")
	r2, sink := withInnerStats(r2)
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, sink, err
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tenant := extractAPIKey(r)
	if body.PreviousResponseID != "" {
		s.responseMu.Lock()
		prior, ok := s.responseMessages[tenant][body.PreviousResponseID]
		messages := append([]oaiMsg(nil), prior.Messages...)
		s.responseMu.Unlock()
		if !ok || len(messages) == 0 {
			writeResponsesError(w, 400, "invalid_request_error", "unknown previous_response_id")
			return
		}
		o.Messages = append(messages, o.Messages...)
	}
	if body.Stream {
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(body.Model, "m365-copilot"))
		return
	}
	out, raw, status, stats, err := s.runOpenAIAdapterWithStats(r, o)
	if status >= 400 {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	msg, _ := openAIChoice(out)
	outputForUsage := ""
	if msg != nil {
		outputForUsage = fmt.Sprint(msg["content"])
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputForUsage += fmt.Sprint(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, outputForUsage)
	out["usage"] = estimate.Values
	out["m365_usage_source"] = estimate.Source
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/responses",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		// 缓存 token 由内层回填 —— 只有它看得到会话历史。
		CacheTokens: innerStatsCacheTokens(stats),
		DurationMs:  time.Since(startedAt).Milliseconds(),
		Status:      200,
	})
	// Retain the normalized history so a subsequent previous_response_id can
	// validate its function_call_output against the original tool call.
	if _, ok := out["id"].(string); ok {
		// Use the same public response id that writeResponsesResult exposes.
		publicID := "resp_" + uuid.NewString()
		out["m365_response_id"] = publicID
		stored := append([]oaiMsg(nil), o.Messages...)
		if msg, _ := openAIChoice(out); msg != nil {
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				converted := make([]map[string]any, 0, len(calls))
				for _, call := range calls {
					if m, ok := call.(map[string]any); ok {
						converted = append(converted, m)
					}
				}
				stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
			} else {
				if text, _ := msg["content"].(string); text != "" {
					stored = append(stored, oaiMsg{Role: "assistant", Content: text})
				}
			}
		}
		s.responseMu.Lock()
		bucket := s.responseMessages[tenant]
		if bucket == nil {
			bucket = map[string]respHistory{}
			s.responseMessages[tenant] = bucket
		}
		for k, h := range bucket {
			if time.Since(h.At) > time.Hour {
				delete(bucket, k)
			}
		}
		if len(bucket) >= maxResponsesPerTenant {
			var oldestKey string
			var oldestAt time.Time
			for k, h := range bucket {
				if oldestKey == "" || h.At.Before(oldestAt) {
					oldestKey, oldestAt = k, h.At
				}
			}
			delete(bucket, oldestKey)
		}
		bucket[publicID] = respHistory{At: time.Now(), Messages: stored}
		s.responseMu.Unlock()
	}
	writeResponsesResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	text, _ := msg["content"].(string)
	return strings.TrimSpace(text) != ""
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	out, raw, status, stats, err := s.runOpenAIAdapterWithStats(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	// 输出必须用真实回答来算。之前这里传空字符串，于是 /v1/messages 的
	// 用量永远是「出 0」—— 面板上看到的 110377入/出0 就是这么来的。
	answerText := ""
	if msg, _ := openAIChoice(out); msg != nil {
		if text, ok := msg["content"].(string); ok {
			answerText = text
		}
		if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
			answerText += mustJSON(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, answerText)
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/messages",
		Stream:       body.Stream,
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		// 缓存 token 由内层回填 —— 只有它看得到会话历史。
		CacheTokens: innerStatsCacheTokens(stats),
		DurationMs:  time.Since(startedAt).Milliseconds(),
		Status:      200,
	})
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}
