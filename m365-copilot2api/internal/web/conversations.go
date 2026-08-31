package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, map[string]any{"conversations": s.sessions.list()})
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// 两个 store 各删一次，然后按「到底删掉了什么」回答。
	//
	// 原来的顺序是先无条件调用 Delete（它内部无条件打印 "deleted
	// conversation <id>"），再用 s.sessions.delete 的返回值决定要不要回 404。
	// 于是删一个不存在的 ID 会同时产生一条「已删除」日志和一个 404 —— 日志说
	// 删了，接口说没找到，两边至多有一个是真的。反过来，ID 只存在于
	// conversationManager 时，记录已经被移除却仍回 404，调用方会以为什么都没
	// 发生。现在日志只在真的删掉时打印，404 只在两边都没有这个 ID 时返回。
	removedManaged := s.conversationManager.Delete(body.ID)
	removedSession := s.sessions.delete(body.ID)
	if !removedManaged && !removedSession {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) conversationCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		KeepN int    `json:"keep_n"`
	}
	// 空请求体（完全不带参数）是合法调用，只是「按当前配置清理一次」。
	// 其余解码错误必须拒绝：以前所有解码失败都被忽略，然后照样回
	// "cleaned"，调用方无从知道自己的参数根本没被读到。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if body.Mode != "" {
		mode := ConversationCleanupMode(body.Mode)
		if !validCleanupMode(mode) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"mode must be one of after_response, on_exit, keep_n, max_age")
			return
		}
		s.conversationManager.SetMode(mode)
	}
	// keep_n 以前被解码后从未使用，接口却照样回 "cleaned"，看上去像是生效了。
	// 现在真的写进 conversationManager（keep_n 模式下的保留条数），非法值直接
	// 拒绝而不是默默丢掉。
	if body.KeepN != 0 {
		if body.KeepN < 1 || body.KeepN > maxCleanupKeepN {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("keep_n must be between 1 and %d", maxCleanupKeepN))
			return
		}
		s.conversationManager.SetKeepN(body.KeepN)
	}
	cleaned := s.conversationManager.Cleanup()
	jsonOut(w, map[string]any{
		"status": "cleaned",
		"mode":   string(s.conversationManager.Mode()),
		// 回显真正生效的 keep_n，调用方据此确认参数被采纳。
		"keep_n":    s.conversationManager.KeepN(),
		"deleted":   cleaned,
		"remaining": len(s.conversationManager.List()),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions := s.sessionResolver.ListSessions()
		jsonOut(w, map[string]any{
			"object": "list",
			"data":   sessions,
		})
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sess, ok := s.sessionResolver.GetSession(body.SessionID)
		if !ok {
			jsonOut(w, map[string]any{
				"object":     "session",
				"id":         body.SessionID,
				"created":    time.Now().Unix(),
				"expires_in": 1800,
				"status":     "created",
			})
			return
		}
		jsonOut(w, map[string]any{
			"object":          "session",
			"id":              sess.SessionID,
			"conversation_id": sess.ConversationID,
			"created":         sess.CreatedAt.Unix(),
			"status":          "active",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats := cacheStats.GetStats()
	jsonOut(w, map[string]any{
		"object": "cache_stats",
		"stats":  stats,
	})
}

func (s *Server) handleCacheStatsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cacheStats.Reset()
	jsonOut(w, map[string]any{"status": "reset"})
}

func (s *Server) handleM365Conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil && len(s.sessionResolver.ListSessions()) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	rows := make(map[string]map[string]any)
	var cloudErr error
	if m365CloudClient != nil {
		var chats []map[string]any
		chats, cloudErr = m365CloudClient.ListConversations()
		for _, chat := range chats {
			conversationID, _ := chat["conversationId"].(string)
			if conversationID != "" {
				rows[conversationID] = chat
			}
		}
	}
	if cloudErr != nil && len(s.sessionResolver.ListSessions()) == 0 {
		err := cloudErr
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	for _, session := range s.sessionResolver.ListSessions() {
		row, ok := rows[session.ConversationID]
		if !ok {
			row = map[string]any{}
			rows[session.ConversationID] = row
		}
		row["conversationId"] = session.ConversationID
		row["sessionId"] = session.SessionID
		row["accountId"] = session.AccountID
		row["createTimeUtc"] = session.CreatedAt.UnixMilli()
		row["updateTimeUtc"] = session.LastUsedAt.UnixMilli()
		row["messageCount"] = len(session.ContextHistory)
		row["historyAvailable"] = len(session.ContextHistory) > 0
		row["source"] = "gateway"
		if account, found := s.tokens.Get(session.AccountID); found {
			row["accountEmail"] = account.Email
		}
		if name, _ := row["chatName"].(string); strings.TrimSpace(name) == "" {
			row["chatName"] = conversationTitle(session.ContextHistory)
		}
	}

	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, row)
	}
	sort.Slice(data, func(i, j int) bool {
		return conversationTimestamp(data[i]) > conversationTimestamp(data[j])
	})
	response := map[string]any{"object": "list", "data": data, "count": len(data)}
	if cloudErr != nil {
		response["warning"] = cloudErr.Error()
	}
	jsonOut(w, response)
}

func conversationTitle(messages []oaiMsg) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		text := strings.TrimSpace(contentToString(message.Content))
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > 120 {
			return string(runes[:120]) + "..."
		}
		return text
	}
	return "Untitled conversation"
}

func conversationTimestamp(row map[string]any) int64 {
	for _, key := range []string{"updateTimeUtc", "createTimeUtc"} {
		switch value := row[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		}
	}
	return 0
}

func (s *Server) handleM365Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ConversationID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := m365CloudClient.DeleteConversation(body.ConversationID); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	s.dropConversation(body.ConversationID)
	jsonOut(w, map[string]any{"status": "deleted", "conversation_id": body.ConversationID})
}

func (s *Server) handleM365Cleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		MaxAgeHours int `json:"max_age_hours"`
		KeepN       int `json:"keep_n"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	maxAge := time.Duration(body.MaxAgeHours) * time.Hour
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	keepN := body.KeepN
	if keepN <= 0 {
		keepN = 5
	}

	deleted, err := m365CloudClient.CleanupOldConversations(maxAge, keepN)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "cleaned", "deleted": deleted})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if s.sessionResolver.DeleteSession(sessionID) {
		jsonOut(w, map[string]any{"status": "deleted", "session_id": sessionID})
	} else {
		http.Error(w, "session not found", http.StatusNotFound)
	}
}

type conversationWhitelistRequest struct {
	ConversationID string `json:"conversation_id"`
	Add            bool   `json:"add"`
}

func (s *Server) conversationWhitelist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body conversationWhitelistRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.ConversationID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Add {
		s.conversationManager.Whitelist(body.ConversationID)
	} else {
		s.conversationManager.Unwhitelist(body.ConversationID)
	}
	jsonOut(w, map[string]any{"status": "updated", "conversation_id": body.ConversationID, "whitelisted": body.Add})
}
