package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"

	"github.com/google/uuid"
)

// chatStreamWithEvents is kept as a narrow handler seam so stream framing can
// be regression-tested without an upstream account, model, or network call.
// Production always delegates to the account-aware ChatWithEvents path.
var chatStreamWithEvents = func(ctx context.Context, s *Server, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	return s.chatWithAccountEvents(ctx, accountID, account, request, onEvent)
}

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	acc, err := s.resolveAccount(body.AccountID)
	if err != nil {
		if isAccountResolveFailure(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}

	request := chathub.Request{
		Text: text, Tone: body.Tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments,
	}
	// Make the IDs available to the first SSE frame as well as the final done
	// frame. This preserves ChatHub's original first-turn behavior, which marks
	// a request as started whenever either ID was absent.
	if request.SessionID == "" {
		request.SessionID = uuid.NewString()
		request.Started = true
	}
	if request.ConversationID == "" {
		request.ConversationID = uuid.NewString()
		request.Started = true
	}

	streamStarted := false
	startStream := func() {
		if streamStarted {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		streamStarted = true
	}
	writeStreamEvent := func(name string, value any) error {
		startStream()
		return writeSSE(r, w, flusher, name, value)
	}

	var streamWriteErr error
	streamIndex := 0
	identityWritten := false
	onEvent := func(event chathub.StreamEvent) error {
		switch event.Kind {
		case "text":
			event.Text = sanitizePublicAssistantTextWithState(event.Text, &identityWritten)
			if event.Text == "" {
				return nil
			}
		case "reasoning":
			event.Text = sanitizePublicReasoningText(event.Text)
		}

		payload := map[string]any{
			"index":          streamIndex,
			"type":           "chathub.event",
			"event":          publicChatStreamEvent(event),
			"conversationId": request.ConversationID,
			"sessionId":      request.SessionID,
		}
		if err := writeStreamEvent("event", payload); err != nil {
			streamWriteErr = err
			return err
		}
		streamIndex++
		return nil
	}

	// ChatHub owns the five-minute upstream completion window. Do not impose a
	// shorter handler timeout here: doing so turns an otherwise valid long
	// stream into a premature 502 after 120 seconds.
	res, err := chatStreamWithEvents(r.Context(), s, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, request, onEvent)
	if err != nil {
		// A client disconnect/write failure has already ended the response. Do
		// not try to append an error frame to a dead connection.
		if streamWriteErr != nil || r.Context().Err() != nil {
			return
		}
		if streamStarted {
			// Headers and one or more incremental frames are already committed, so
			// an HTTP status can no longer describe this failure. Terminate the
			// SSE sequence with an error event and deliberately omit done.
			_ = writeStreamEvent("error", map[string]any{
				"type":    "error",
				"message": upstreamError(err),
				"status":  upstreamStatus(err),
			})
			return
		}
		writeUpstreamError(w, err)
		return
	}
	res.ConversationID = firstNonEmpty(res.ConversationID, request.ConversationID)
	res.SessionID = firstNonEmpty(res.SessionID, request.SessionID)
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)

	if err := writeStreamEvent("done", map[string]any{
		"type": "done", "text": res.Text,
		"conversationId": res.ConversationID, "sessionId": res.SessionID, "requestId": res.RequestID,
		"throttling": res.Throttling,
	}); err != nil {
		return
	}
}

// publicChatStreamEvent keeps live frames JSON-compatible with the existing
// lower-camel event contract. chathub.StreamEvent deliberately has no JSON
// tags because it is an internal callback type, so writing it directly would
// expose capitalized field names to SSE clients.
func publicChatStreamEvent(event chathub.StreamEvent) map[string]any {
	payload := map[string]any{"kind": event.Kind}
	if event.Text != "" {
		payload["text"] = event.Text
	}
	if event.MessageType != "" {
		payload["messageType"] = event.MessageType
	}
	if event.ContentType != "" {
		payload["contentType"] = event.ContentType
	}
	if event.ToolName != "" {
		payload["toolName"] = event.ToolName
	}
	if len(event.Arguments) > 0 {
		payload["arguments"] = event.Arguments
	}
	if len(event.Raw) > 0 {
		payload["raw"] = event.Raw
	}
	return payload
}

// writeSSE emits one SSE frame, returning when the client has disconnected
// (request context canceled) or the write fails so the handler can abort
// instead of blocking a goroutine against a dead socket.
func writeSSE(r *http.Request, w http.ResponseWriter, f http.Flusher, name string, value any) error {
	if err := r.Context().Err(); err != nil {
		return err
	}
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}
