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
//
// 同理，开头那句原本是「一律用相对路径」，跟同一句后半段（未经核实的绝对路径不
// 要用）和 runtime_prompt.go（可以用上面那种形式的绝对路径）都对不上。用户开口
// 就是 E:\download\... 这种绝对路径，禁掉它的直接后果是模型改口说这条路径用不
// 了 —— 正是这套提示词要压住的那种回答。真正要禁的是凭猜测编出来的绝对路径。
//
// 第三处：原文有一句「The only permitted execution tool is the caller-provided
// custom exec tool.」，是配合下面那段已删除的「exec 在场就丢掉其余工具」写的 ——
// 当时模型手上确实只剩 exec，这句话是对的。转换器改成混合工具全部保留之后，这句
// 话会让模型拒绝调用方自己声明的 read_file/apply_patch/MCP 工具：光改代码不改提
// 示词，工具进了表也照样被模型自己挡回去。
//
// 现在拆成两段：customExecWorkspaceInstruction 是常驻策略，只说 exec 是本地
// shell/文件操作的通道，同时明确其余声明过的工具照样可以调用；
// customExecSoleToolInstruction 只在 exec 真的是转换后唯一那把工具时才追加，
// 那种情况下「没有别的工具可调」是事实陈述，不再是对调用方声明的否定。
// 「不许用/编造 Microsoft 365、Copilot 原生工具」这条跟工具表无关，留在常驻段里。
const customExecWorkspaceInstruction = `You are operating through the caller's local OpenCode execution bridge. Never use, request, or mention Microsoft 365/Copilot native tools, and never invent a tool the caller did not declare. Route local shell and file work through the caller-provided custom exec tool; every other tool the caller declared stays available and may be called for its own purpose. The executor already starts in the caller-selected project workspace, so a relative path is the normal way to name a file there. An absolute path the caller gave you is equally valid; inventing one is not -- do not guess at, cd to, or write under an absolute path you have not verified with the exec tool. Inspect the working directory and list it before making changes. Do not create files outside the current working directory. Never claim a file was created, modified, or verified until custom exec returns a successful result. After every execution, use custom exec to verify the result.`

// customExecSoleToolInstruction 只在 exec 是转换后唯一的工具时追加。措辞是「本次
// 请求里只有这一把工具」而不是「只准用这一把」：前者随工具表变化自动成立或消失，
// 后者在混合声明下就是错的。
const customExecSoleToolInstruction = ` In this request the custom exec tool is the only tool available to you; there is no other tool to call.`

// customExecInstruction 组装上面两段。soleTool 由调用点按转换后的 o.Tools 判断，
// 而不是按 r.Tools 的原始声明 —— 模型看到的是转换结果（namespace 展平之后、
// 无法表示的声明被丢掉之后），提示词必须跟那份表一致。
func customExecInstruction(soleTool bool) string {
	if soleTool {
		return customExecWorkspaceInstruction + customExecSoleToolInstruction
	}
	return customExecWorkspaceInstruction
}

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
	// 这里原先有一段 pre-scan：先扫一遍看有没有 custom/exec，只要有，循环里第一
	// 句 `if hasCustomExec && !(typ=="custom" && name=="exec") { continue }` 就把
	// 其余所有声明全部丢掉。Codex 一次声明 exec + apply_patch + read_file + 若干
	// MCP namespace，落到模型手里的工具表只剩 [exec]，模型于是照实回答
	// 「read_file 不可用」—— 它的工具表真的只有 exec。
	//
	// 用 /v1/responses 的 usage 估算器（按转换后的 o.Tools 计数）量过：
	//   [exec] = 239，[exec, read_file, apply_patch] = 239（差 0，两个 function
	//   schema 转换后根本不存在），[read_file, apply_patch] = 121，[] = 19
	//   （差 102，同样两个 schema 在没有 exec 时是算进去的）。
	//
	// 现在整段 pre-scan 和那个 continue 都删掉了：exec 只是「要不要注入工作区策略
	// 提示词」的信号，不再是对调用方工具表的过滤器。下面 namespace 展平和
	// web_search 直通两个 case 在旧代码里只要 exec 在场就永远走不到，属于死代码，
	// 一并复活。谁想把互斥逻辑加回来，请先重看上面那组 token 数字。
	//
	// 声明顺序按调用方给的原样保留：Codex 侧的工具优先级、以及
	// toolType/declaredTool 的名字查找都依赖这份表，重排没有好处。
	hasCustomExec := false
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
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
		if typ == "custom" {
			// A Responses custom tool has raw string input, while ChatHub's plugin
			// protocol always carries JSON function arguments. exec was originally
			// the only bridged custom name, but that made [custom/apply_patch] turn
			// into no tool at all with no explanation. The response path already
			// serializes every Tool.Type == "custom" as custom_tool_call and
			// customToolInput reads this exact {"input": "..."} envelope, so the
			// representation is not exec-specific: bridge every named custom tool
			// through the same one-string schema.
			//
			// Do not flatten a custom tool into type "function" merely because its
			// wire envelope is JSON. Codex distinguishes custom_tool_call (raw
			// input) from function_call (JSON arguments), and losing Tool.Type here
			// makes a correctly selected custom tool impossible for the caller to
			// execute. A missing name is still unusable: clientPlugins skips it and
			// the validator cannot resolve an unnamed declaration, so ignore that
			// malformed shape explicitly instead of creating a ghost entry.
			if name == "" {
				continue
			}
			f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
			if name == "exec" {
				// exec additionally enables the local-workspace system policy below;
				// another custom tool remains callable but must not inherit rules that
				// specifically require checking paths and mutations through exec.
				hasCustomExec = true
			}
		} else if typ != "function" {
			continue
		}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: typ, Function: b})
	}
	if hasCustomExec {
		o.Messages = append([]oaiMsg{{Role: "system", Content: customExecInstruction(len(o.Tools) == 1)}}, o.Messages...)
	}
	return o, nil
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicToolResultText flattens a tool_result content value to text.
// Anthropic allows either a plain string or a list of content blocks, so a
// caller returning blocks must not lose its message to a failed type assertion.
func anthropicToolResultText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, raw := range v {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if encoded, err := json.Marshal(content); err == nil {
		return string(encoded)
	}
	return fmt.Sprint(content)
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
				content := b["content"]
				// is_error is the client's own verdict on the call, and it is the
				// only authoritative one. Dropping it left the ledger to guess from
				// the result text, and toolResultLooksFailed guesses wrong in both
				// directions: a failed Read whose message reads "File does not
				// exist." was recorded as a success, so the retry was suppressed and
				// the model was told the file had been read.
				//
				// The OpenAI wire format has no is_error field, so the flag is
				// carried as an explicit prefix on the tool content. toolLooksFailed
				// keys on a leading "error"/"failed" for observational tools and this
				// prefix satisfies that, while staying readable to the model, which
				// also has to understand that the call failed.
				if isError, ok := b["is_error"].(bool); ok && isError {
					content = "Error: " + anthropicToolResultText(content)
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: content})
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
