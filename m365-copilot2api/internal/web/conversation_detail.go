package web

import (
	"net/http"
	"strings"
)

// conversationDetailMessage 是详情端点里的单条消息。字段只暴露仓库里
// 确实存在的信息：sessionBinding.ContextHistory 保存的是 oaiMsg，没有
// 逐条时间戳，因此时间只在会话层给出（createdAt / lastUsedAt），不伪造
// 每条消息的时间。
type conversationDetailMessage struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
	// Text 是内容的纯文本投影，便于前端直接渲染。
	Text string `json:"text"`
	// Content 是原始 content 字段（可能是字符串或多模态数组），供需要
	// 完整结构的前端使用。
	Content          any              `json:"content,omitempty"`
	Name             string           `json:"name,omitempty"`
	ReasoningContent string           `json:"reasoningContent,omitempty"`
	ToolCallID       string           `json:"toolCallId,omitempty"`
	ToolCalls        []map[string]any `json:"toolCalls,omitempty"`
}

// conversationDetail 按会话或云端对话 id 返回完整消息内容。
//
// GET /api/conversations/detail?id=<conversationId|sessionId>
//
// 查询顺序：先按 conversationId 匹配，未命中再按 sessionId 匹配，这样
// 前端可以直接把列表里的任一个 id 传进来。
func (s *Server) conversationDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 守在解析 id 之前：这条端点是面板里最贵的一笔 —— 命中后要把整段
	// ContextHistory（正文 + reasoning + tool_calls）编成 JSON。停用期间
	// 一次都不该发生，也不去问 sessionResolver 要东西。
	if !conversationPanelEnabled() {
		conversationPanelDisabled(w)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "id is required")
		return
	}
	if s.sessionResolver == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session resolver is not initialized")
		return
	}
	session, ok := s.sessionResolver.GetConversation(id)
	if !ok {
		session, ok = s.sessionResolver.GetSession(id)
	}
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}

	messages := make([]conversationDetailMessage, 0, len(session.ContextHistory))
	for i, message := range session.ContextHistory {
		messages = append(messages, conversationDetailMessage{
			Index:            i,
			Role:             message.Role,
			Text:             contentToString(message.Content),
			Content:          message.Content,
			Name:             message.Name,
			ReasoningContent: message.ReasoningContent,
			ToolCallID:       message.ToolCallID,
			ToolCalls:        message.ToolCalls,
		})
	}

	out := map[string]any{
		"object":         "conversation.detail",
		"conversationId": session.ConversationID,
		"sessionId":      session.SessionID,
		"accountId":      session.AccountID,
		"createdAt":      session.CreatedAt,
		"lastUsedAt":     session.LastUsedAt,
		"title":          conversationTitle(session.ContextHistory),
		"messageCount":   len(messages),
		"messages":       messages,
	}
	if s.tokens != nil {
		if account, found := s.tokens.Get(session.AccountID); found {
			out["accountEmail"] = account.Email
		}
	}
	jsonOut(w, out)
}
