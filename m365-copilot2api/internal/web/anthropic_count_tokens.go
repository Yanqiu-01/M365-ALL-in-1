package web

import (
	"encoding/json"
	"log"
	"net/http"

	"m365-copilot2api/internal/chathub"
)

// anthropicCountTokens implements POST /v1/messages/count_tokens.
//
// Claude Code calls this before a turn to decide how much history it can send.
// The route was never registered, so it fell through to the catch-all "/" and
// answered 404 -- measured 45 times in one session of live traffic. A client
// that cannot measure a turn either refuses to send it or falls back to a
// guess, and on a long agentic exchange a wrong guess is what pushes a request
// past the window, which is felt as the gateway ignoring tools.
//
// The count is an estimate from the same tokenizer the context budget uses, so
// this endpoint and trimMessagesToContext can never disagree about whether a
// turn fits. Anthropic's own count is exact and ours is not; agreeing with our
// own trimming matters more here than matching Anthropic digit for digit.
func (s *Server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	total := 0
	for _, m := range o.Messages {
		total += messageTokenCost(m, body.Model)
	}
	// Tool schemas are part of the input the model has to read, so a count that
	// omitted them would let a client fill the window with history and then be
	// pushed over by its own tool declarations -- 36 tools is not a rounding
	// error. anthropicRequest carries its own tool type; convert to the shape
	// toolSchemaTokenCost serializes.
	if len(body.Tools) > 0 || body.ToolChoice != nil {
		tools := make([]chathub.Tool, 0, len(body.Tools))
		for _, t := range body.Tools {
			// chathub.Tool carries the function as raw JSON, and
			// toolSchemaTokenCost counts the serialized form -- so the schema has
			// to be marshalled here rather than handed over as a Go map, or its
			// bytes would not be counted at all. A tool whose schema fails to
			// marshal still contributes its name and description.
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.InputSchema != nil {
				fn["parameters"] = t.InputSchema
			}
			raw, marshalErr := json.Marshal(fn)
			if marshalErr != nil {
				raw, _ = json.Marshal(map[string]any{"name": t.Name, "description": t.Description})
			}
			tools = append(tools, chathub.Tool{Type: "function", Function: raw})
		}
		total += toolSchemaTokenCost(tools, body.ToolChoice, body.Model)
	}
	if total < 1 {
		total = 1
	}

	log.Printf("[count-tokens] messages=%d tools=%d input_tokens=%d", len(o.Messages), len(body.Tools), total)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": total})
}
