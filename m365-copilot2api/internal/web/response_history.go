package web

import "time"

// cloneResponseMessages is deliberately separate from the session resolver's
// cloneMessages helper. Responses history can contain nested JSON values in
// Content and ToolCalls; a slice-only copy would let a later request mutate a
// previously stored response through a shared map or slice.
func cloneResponseMessages(messages []oaiMsg) []oaiMsg {
	if messages == nil {
		return nil
	}
	out := make([]oaiMsg, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = cloneResponseValue(message.Content)
		out[i].ToolCalls = cloneResponseToolCalls(message.ToolCalls)
	}
	return out
}

func cloneResponseToolCalls(calls []map[string]any) []map[string]any {
	if calls == nil {
		return nil
	}
	out := make([]map[string]any, len(calls))
	for i, call := range calls {
		out[i] = cloneResponseMap(call)
	}
	return out
}

func cloneResponseMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = cloneResponseValue(value)
	}
	return out
}

// cloneResponseValue copies the JSON-shaped values produced by the request
// decoder and response normalizer. Scalars are immutable, while maps and
// slices are recursively copied.
func cloneResponseValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneResponseMap(typed)
	case []any:
		if typed == nil {
			return ([]any)(nil)
		}
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneResponseValue(item)
		}
		return out
	case []map[string]any:
		return cloneResponseToolCalls(typed)
	case map[string]string:
		if typed == nil {
			return (map[string]string)(nil)
		}
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			out[key] = item
		}
		return out
	case []string:
		if typed == nil {
			return ([]string)(nil)
		}
		return append([]string(nil), typed...)
	case []byte:
		if typed == nil {
			return ([]byte)(nil)
		}
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

// reasoningContent reads the internal OpenAI-style reasoning field without
// assuming that every upstream message contains one. The Responses history
// store keeps this field so later turns preserve the same context as the
// streaming path.
func reasoningContent(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	if value, ok := msg["reasoning_content"].(string); ok {
		return value
	}
	if value, ok := msg["reasoning"].(string); ok {
		return value
	}
	return ""
}

// loadResponseHistory resolves a previous_response_id for one tenant. It
// enforces the same retention window as rememberResponse at read time, rather
// than allowing an expired entry to remain usable until a later write happens.
func (s *Server) loadResponseHistory(tenant, id string) ([]oaiMsg, bool) {
	if s == nil || id == "" {
		return nil, false
	}

	now := time.Now()
	s.responseMu.Lock()
	defer s.responseMu.Unlock()

	bucket := s.responseMessages[tenant]
	if bucket == nil {
		return nil, false
	}
	history, ok := bucket[id]
	if !ok {
		return nil, false
	}
	if now.Sub(history.At) > time.Hour {
		delete(bucket, id)
		return nil, false
	}
	return cloneResponseMessages(history.Messages), true
}

// rememberResponse stores the normalized message history behind a public
// Responses API id. The store is process-local by design, but it is kept
// tenant-scoped so one API key can never resolve another key's response id.
//
// This helper is shared by the buffered and streaming Responses paths. Keeping
// the retention and eviction rules in one place prevents the two paths from
// diverging as new response features are added.
func (s *Server) rememberResponse(tenant, id string, messages []oaiMsg) {
	if s == nil || id == "" {
		return
	}

	now := time.Now()
	s.responseMu.Lock()
	defer s.responseMu.Unlock()

	if s.responseMessages == nil {
		s.responseMessages = make(map[string]map[string]respHistory)
	}
	bucket := s.responseMessages[tenant]
	if bucket == nil {
		bucket = make(map[string]respHistory)
		s.responseMessages[tenant] = bucket
	}

	// Expired entries do not count toward the per-tenant cap. Use the same
	// one-hour retention window as the existing buffered Responses path.
	for responseID, history := range bucket {
		if now.Sub(history.At) > time.Hour {
			delete(bucket, responseID)
		}
	}

	// Replacing an existing id must not evict an unrelated live response.
	if _, exists := bucket[id]; !exists && len(bucket) >= maxResponsesPerTenant {
		oldestID := ""
		var oldestAt time.Time
		for responseID, history := range bucket {
			if oldestID == "" || history.At.Before(oldestAt) {
				oldestID = responseID
				oldestAt = history.At
			}
		}
		if oldestID != "" {
			delete(bucket, oldestID)
		}
	}

	bucket[id] = respHistory{
		At:       now,
		Messages: cloneResponseMessages(messages),
	}
}
