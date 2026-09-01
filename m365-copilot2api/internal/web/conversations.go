package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// 对话管理面板（侧栏「对话管理」/ #page-conversations）默认关闭，用
// M365_CONVERSATION_PANEL=1 重新打开。
//
// 为什么默认关；也为什么别指望它省下已经占住的内存：
// 常驻堆在 sessionResolver 里 —— 每个 sessionBinding 都带着完整的
// ContextHistory，面板开不开它都在那儿占着。关掉面板省下的是「每次打开
// 面板时的瞬时分配」：handleM365Conversations 会 ListSessions() 把全部
// 会话拷一份出来、conversationDetail 会把整段消息正文（含 reasoning、
// tool_calls）编成 JSON 再写给浏览器、captureConversations 还会顺手把
// 快照落盘。这几笔在会话多、上下文长的时候相当可观，而且是按「点一次
// 加一笔」累积的。真正要降常驻堆得去动 sessionResolver 的保留策略，不是
// 关这个面板。
//
// 开关只读环境变量、不进 settings.json：改完重启网关即生效，不需要重新
// 编译；也不会因为 settings.json 里存了个旧值而和运维的预期打架。
const envConversationPanel = "M365_CONVERSATION_PANEL"

// conversationPanelEnabled 的取值与仓库里其他布尔开关一致
// （见 outbound.allowFakeIPSource、proxy_pool_sources.go），
// 未设置 / 空串 / 无法识别的值一律视为关闭 —— 默认保守。
func conversationPanelEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envConversationPanel)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// conversationPanelDisabled 是这批端点统一的「功能已关闭」出口。
//
// 状态码选 503 而不是 404：路由确实注册着（server.go 一直在 mux 里挂着这
// 几条），资源也没被删掉 —— 只是被管理员按下了开关。404 会撒谎说「这个
// 接口不存在」，既掩盖了它一条环境变量就能回来的事实，也和
// conversationDetail 里真正的「对话不存在」404 撞在一起，调用方分不清是
// 自己的 id 错了还是整个功能被关了。503 + 明确的 type 与仓库既有的
// m365_not_configured / session_store_unavailable 是同一路数：
// 「服务在，但当前不可用」。
//
// 返回前不碰 sessionResolver、不碰 historyArchive、不碰 M365 云端 ——
// 这正是关掉面板要省下的那部分开销，顺带也保证停用期间不会有任何删除。
func conversationPanelDisabled(w http.ResponseWriter) {
	writeOpenAIError(w, http.StatusServiceUnavailable, "conversation_panel_disabled",
		"Conversation management panel is disabled. Set "+envConversationPanel+"=1 and restart the gateway to re-enable it.")
}

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
	if !conversationPanelEnabled() {
		conversationPanelDisabled(w)
		return
	}
	// probe=1 是前端唯一的开关探测口：index.html 是从磁盘直接送出的静态
	// 页面，服务端没有模板渲染的机会，前端只能问一次后端才知道开关状态。
	// 这里在 ListSessions 之前就返回，探测本身不产生任何会话遍历开销 ——
	// 关闭时上面那个分支已经先答了 503，前端把「非 200」一律当关闭处理，
	// 于是旧版网关（不认识 probe 参数）也不会出现「入口在、接口不通」。
	//
	// 复用既有路由是刻意的：新增一条路由要改 server.go，而那个文件此刻
	// 由别人在改。
	if r.URL.Query().Get("probe") == "1" {
		jsonOut(w, map[string]any{"object": "conversation.panel", "enabled": true})
		return
	}
	sessions := []sessionBinding{}
	if s.sessionResolver != nil {
		sessions = s.sessionResolver.ListSessions()
	}
	remote := r.URL.Query().Get("remote") == "1" || strings.EqualFold(r.URL.Query().Get("remote"), "true")
	if remote && m365CloudClient == nil && len(sessions) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	rows := make(map[string]map[string]any)
	var cloudErr error
	if remote && m365CloudClient != nil {
		var chats []map[string]any
		chats, cloudErr = m365CloudClient.ListConversations()
		for _, chat := range chats {
			conversationID, _ := chat["conversationId"].(string)
			if conversationID != "" {
				rows[conversationID] = chat
			}
		}
	}
	if cloudErr != nil && len(sessions) == 0 {
		err := cloudErr
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	for _, session := range sessions {
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
		if s.tokens != nil {
			if account, found := s.tokens.Get(session.AccountID); found {
				row["accountEmail"] = account.Email
			}
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
	response := map[string]any{"object": "list", "data": data, "count": len(data), "remoteFetched": remote}
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
	// 面板关掉后这条也必须闭嘴：删除按钮只长在 #page-conversations 里，
	// 但一个没刷新的旧标签页、或者照着旧文档写的脚本仍然能直接 POST 过来。
	// 停用期间不接受任何云端删除 —— 守在这里，云端一次都不会被碰。
	if !conversationPanelEnabled() {
		conversationPanelDisabled(w)
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
	// 比 handleM365Delete 更该守：CleanupOldConversations 是循环删除，一次
	// 调用就能清掉一批云端对话。停用期间绝不放它进去 —— 用户要的是「别再
	// 占内存」，不是「顺手把历史删了」。
	//
	// 注意这里守的只是「面板的云端清理入口」。后台按配置跑的自动清理
	// （StartAutoCleanup）和本地 /api/conversations/cleanup 都不受影响，它们
	// 不属于这个面板，也不该被这个开关连带停掉。
	if !conversationPanelEnabled() {
		conversationPanelDisabled(w)
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
