package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// responsesRequest is the OpenAI Responses API request subset supported by the gateway.
type responsesRequest struct {
	Model              string           `json:"model"`
	AccountID          string           `json:"accountId,omitempty"`
	Instructions       string           `json:"instructions,omitempty"`
	Input              any              `json:"input"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	User               string           `json:"user,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Conversation       string           `json:"conversation,omitempty"`
	NewConversation    bool             `json:"new_conversation,omitempty"`
	// Codex sends these on every /v1/responses call. They were previously dropped
	// by the decoder without a trace; capture them so the request is represented
	// faithfully. ParallelToolCalls now has a matching field on oaiReq (server.go)
	// and is forwarded in the conversion below, so /v1/responses and
	// /v1/chat/completions agree on it. Store and PromptCacheKey are accepted,
	// not acted on: the gateway keeps no response store and no prompt cache.
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`
	Store             *bool  `json:"store,omitempty"`
	PromptCacheKey    string `json:"prompt_cache_key,omitempty"`
}

// customExecWorkspaceInstruction 只约束 custom exec 这一条通道：执行器已经落在
// 调用方选定的工程目录里，所以一律用相对路径。
//
// 这里刻意不再点名 /workspace。runtime_prompt.go 注入的环境说明会告诉 Android
// 宿主 /workspace 就是可写的工程根，两条提示词进同一个 prompt，一条说那是根目
// 录、另一条说不准写，模型只能二选一。改为「不要凭猜测使用任何绝对路径」，既
// 保住原意，又不跟宿主实际情况打架。
const customExecWorkspaceInstruction = `You are operating through the caller's local OpenCode execution bridge. Never use, request, or mention Microsoft 365/Copilot native tools. The only permitted execution tool is the caller-provided custom exec tool. The executor already starts in the caller-selected project workspace. Use relative paths only; do not guess at, cd to, or write under an absolute path you have not verified with the exec tool. Inspect the working directory and list it before making changes. Do not create files outside the current working directory. Never claim a file was created, modified, or verified until custom exec returns a successful result. After every execution, use custom exec to verify the result.`

func (r responsesRequest) openAI() (oaiReq, error) {
	o := oaiReq{
		Model:             r.Model,
		AccountID:         r.AccountID,
		Stream:            r.Stream,
		ToolChoice:        r.ToolChoice,
		User:              r.User,
		ParallelToolCalls: r.ParallelToolCalls,
	}
	if instructions := strings.TrimSpace(r.Instructions); instructions != "" {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: instructions})
	}
	if r.Reasoning != nil {
		o.Reasoning = r.Reasoning
		o.ReasoningEffort = r.Reasoning.Effort
	}
	switch v := r.Input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: v})
	case []any:
		// Codex 并行调用工具时，input 里会出现连续多个 function_call。OpenAI 的
		// chat 表示法要求同一轮的并行调用共享一条 assistant 消息：若每个调用各
		// 占一条，validateToolConversation 会在第二条 assistant 上报「tool results
		// missing before assistant message at index N」，整个请求以 HTTP 400 被拒
		// （CC Switch 侧看到的就是这个 400）。
		//
		// openCall 指向当前还能继续累加调用的那条 assistant 消息，遇到工具结果或
		// 普通消息就归零 —— 串行调用因此仍然各自成条，不会被错误地并进同一轮，
		// 那样会反过来让先前的调用配不上结果。
		openCall := -1
		appendToolCall := func(call map[string]any) {
			if openCall >= 0 {
				o.Messages[openCall].ToolCalls = append(o.Messages[openCall].ToolCalls, call)
				return
			}
			o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{call}})
			openCall = len(o.Messages) - 1
		}
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call_progress":
				// Progress is deliberately not converted into an assistant/tool
				// message. It is transport metadata from a long-running client-side
				// executor and must not trigger a model turn or tool completion.
				if _, ok := parseToolProgress(m); !ok {
					return o, fmt.Errorf("invalid function_call_progress")
				}
				continue
			case "function_call_output":
				id, _ := m["call_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
				openCall = -1
			case "custom_tool_call_output":
				id, _ := m["call_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
				openCall = -1
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args := m["arguments"]
				if s, ok := args.(string); ok {
					var x any
					if json.Unmarshal([]byte(s), &x) == nil {
						args = x
					}
				}
				appendToolCall(map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}})
			case "custom_tool_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				input, _ := m["input"].(string)
				appendToolCall(map[string]any{"id": id, "type": "custom", "function": map[string]any{"name": name, "arguments": mustJSON(map[string]any{"input": input})}})
			default:
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				// Responses input items use input_text/input_image/input_file/
				// input_audio blocks. Keep the blocks intact so flattenPromptMessages
				// can extract every attachment into the ChatHub payload.
				content := m["content"]
				if content == nil {
					content = []any{m}
				}
				o.Messages = append(o.Messages, oaiMsg{Role: role, Content: content})
				openCall = -1
			}
		}
	default:
		return o, fmt.Errorf("input must be string or array")
	}
	hasCustomExec := false
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if typ == "custom" && name == "exec" {
			hasCustomExec = true
			break
		}
	}
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if hasCustomExec && !(typ == "custom" && name == "exec") {
			continue
		}
		switch typ {
		case "namespace":
			// Codex groups related tools (multi_agent_v1, mcp__*) into a namespace
			// wrapper whose real functions live in its "tools" array. ChatHub only
			// understands a flat function list, so the children are flattened here;
			// without this the entire group never reaches the model.
			// Children are renamed to "<namespace>__<child>" for two reasons: two
			// namespaces may export the same child name (e.g. two MCP servers both
			// exposing "search"), and the tool-call callback carries only the flat
			// name, so the prefix is what lets a call be resolved back to the
			// namespace that owns it.
			children, _ := t["tools"].([]any)
			for _, raw := range children {
				c, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if ctyp, _ := c["type"].(string); ctyp != "function" {
					continue
				}
				cname, _ := c["name"].(string)
				if cname == "" {
					continue
				}
				if name != "" {
					cname = name + "__" + cname
				}
				cf := map[string]any{"name": cname, "description": c["description"], "parameters": c["parameters"]}
				cb, _ := json.Marshal(cf)
				o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: cb})
			}
			continue
		case "web_search":
			// Declaration passthrough only. Neither the gateway nor chathub.Request
			// has any web-search implementation; M365 Copilot grounds from the prompt
			// itself. Keep the client's declaration intact instead of silently
			// dropping it, and never synthesise a search tool or fake results.
			wb, _ := json.Marshal(t)
			o.Tools = append(o.Tools, chathub.Tool{Type: typ, Function: wb})
			continue
		}
		f := map[string]any{"name": t["name"], "description": t["description"], "parameters": t["parameters"]}
		if typ == "custom" && name == "exec" {
			// ChatHub accepts JSON function arguments while Codex exec accepts a
			// grammar-constrained raw input string. Preserve the distinction in
			// Tool.Type and bridge the input through a single string field.
			f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
			hasCustomExec = true
		} else if typ != "function" {
			continue
		}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: typ, Function: b})
	}
	if hasCustomExec {
		o.Messages = append([]oaiMsg{{Role: "system", Content: customExecWorkspaceInstruction}}, o.Messages...)
	}
	return o, nil
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}
type anthropicRequest struct {
	Model      string             `json:"model"`
	System     any                `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	MaxTokens  int                `json:"max_tokens,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	if r.System != nil {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		if s, ok := m.Content.(string); ok {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: s})
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			return o, fmt.Errorf("invalid anthropic content")
		}
		var text []any
		var calls []map[string]any
		for _, raw := range blocks {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				text = append(text, b)
			case "image":
				source, _ := b["source"].(map[string]any)
				if source != nil {
					srcType, _ := source["type"].(string)
					switch srcType {
					case "base64":
						data, _ := source["data"].(string)
						media, _ := source["media_type"].(string)
						if data != "" {
							if media == "" {
								media = "application/octet-stream"
							}
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": "data:" + media + ";base64," + data,
							})
						}
					case "url":
						url, _ := source["url"].(string)
						if url != "" {
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": url,
							})
						}
					}
				}
			case "tool_use":
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": mustJSON(b["input"])}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: b["content"]})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: text, ToolCalls: calls})
		}
	}
	for _, t := range r.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	if c, ok := r.ToolChoice.(map[string]any); ok {
		switch c["type"] {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": c["name"]}}
		}
	}
	return o, nil
}
