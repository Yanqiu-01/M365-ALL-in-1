package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// captureConversations is the only history operation that refreshes M365 cloud
// metadata. The normal conversation page is local-only; on demand, this
// endpoint fetches the recent cloud rows once and writes JSON snapshots under
// History. The currently known M365 API provides navigation metadata, not a
// verified full-message-history API, so cloud-only rows are archived honestly
// as metadata-only snapshots.
func (s *Server) captureConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 面板停用 = 不再产生新的捕获工作。守在 sessionResolver / historyArchive
	// / m365CloudClient 之前：这条端点会遍历会话、拉一次云端元数据、还要往
	// History 目录写快照，是三者里唯一同时碰到内存、网络和磁盘的。
	//
	// 已经落盘的历史快照一个都不动 —— 停用只是不再往里加，不做任何删除。
	if !conversationPanelEnabled() {
		conversationPanelDisabled(w)
		return
	}
	if s.sessionResolver == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session resolver is not initialized")
		return
	}

	var body struct {
		Limit int `json:"limit"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	err := decoder.Decode(&body)
	if err != nil && err != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	limit := body.Limit
	if limit == 0 {
		limit = defaultHistoryCaptureLimit
	}
	if limit < 1 || limit > maxHistoryCaptureLimit {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("limit must be between 1 and %d", maxHistoryCaptureLimit))
		return
	}

	archive := s.historyArchive
	if archive == nil {
		// Tests and small embedded callers sometimes construct Server directly
		// instead of going through New. Keep the endpoint useful in that case
		// without changing the production initialisation path.
		archive = openHistoryArchive()
	}
	result := historyCaptureResult{
		Requested: limit,
		Directory: archive.dir,
		Files:     []string{},
	}
	warnings := []string{}
	seen := map[string]bool{}
	localByConversation := s.sessionResolver.captureSessionsByConversation()

	captureLocal := func(sess sessionBinding) {
		result.Selected++
		if len(sess.ContextHistory) == 0 {
			result.Skipped++
			return
		}
		accountEmail := ""
		if s.tokens != nil {
			if account, ok := s.tokens.Get(sess.AccountID); ok {
				accountEmail = account.Email
			}
		}
		path, added, captureErr := archive.captureSession(sess, accountEmail)
		if captureErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", sess.ConversationID, captureErr))
			return
		}
		if path != "" {
			result.Files = append(result.Files, filepath.Base(path))
		}
		if added {
			result.Captured++
		}
	}
	captureMetadata := func(chat map[string]any) {
		conversationID := strings.TrimSpace(cloudString(chat, "conversationId"))
		if conversationID == "" {
			result.Skipped++
			return
		}
		result.Selected++
		path, added, captureErr := archive.captureMetadata(
			conversationID,
			cloudString(chat, "sessionId"),
			cloudString(chat, "accountId"),
			cloudString(chat, "accountEmail"),
			cloudString(chat, "chatName"),
			cloudTime(chat, "createTimeUtc"),
			cloudTime(chat, "updateTimeUtc"),
		)
		if captureErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", conversationID, captureErr))
			return
		}
		if path != "" {
			result.Files = append(result.Files, filepath.Base(path))
		}
		if added {
			result.Captured++
			result.MetadataOnly++
		}
	}

	remoteFetched := false
	if m365CloudClient != nil {
		chats, cloudErr := m365CloudClient.ListConversations()
		if cloudErr != nil {
			warnings = append(warnings, "M365 云端元数据本次未获取；已仅捕获网关本地上下文。")
		} else {
			remoteFetched = true
			sort.Slice(chats, func(i, j int) bool {
				return conversationTimestamp(chats[i]) > conversationTimestamp(chats[j])
			})
			for _, chat := range chats {
				if result.Selected >= limit {
					break
				}
				conversationID := strings.TrimSpace(cloudString(chat, "conversationId"))
				if conversationID == "" || seen[conversationID] {
					result.Skipped++
					continue
				}
				seen[conversationID] = true
				if sess, ok := localByConversation[conversationID]; ok && len(sess.ContextHistory) > 0 {
					captureLocal(sess)
				} else {
					captureMetadata(chat)
				}
			}
		}
	} else {
		warnings = append(warnings, "M365 云端元数据未配置；已仅捕获网关本地上下文。")
	}

	// Keep the button useful when cloud metadata is unavailable or a local
	// active conversation has not appeared in the current navigation page.
	if result.Selected < limit {
		candidates, _ := s.sessionResolver.captureCandidates(limit)
		for _, sess := range candidates {
			if result.Selected >= limit {
				break
			}
			if seen[sess.ConversationID] {
				continue
			}
			seen[sess.ConversationID] = true
			captureLocal(sess)
		}
	}

	if remoteFetched && result.MetadataOnly > 0 {
		warnings = append(warnings, "云端接口只返回对话元数据；云端独有对话的 JSON 不包含消息正文。")
	}
	if result.Selected == 0 {
		warnings = append(warnings, "没有可捕获的本地上下文或云端对话元数据。")
	}

	response := map[string]any{
		"object":        "history.capture",
		"status":        "captured",
		"requested":     result.Requested,
		"selected":      result.Selected,
		"captured":      result.Captured,
		"metadataOnly":  result.MetadataOnly,
		"skipped":       result.Skipped,
		"directory":     result.Directory,
		"files":         result.Files,
		"remoteFetched": remoteFetched,
	}
	if len(result.Errors) > 0 {
		response["errors"] = result.Errors
		warnings = append(warnings, "部分对话未能写入本地归档。")
	}
	if len(warnings) > 0 {
		response["warning"] = strings.Join(warnings, " ")
	}
	jsonOut(w, response)
}

func cloudString(row map[string]any, key string) string {
	value, _ := row[key].(string)
	return value
}

func cloudTime(row map[string]any, key string) time.Time {
	var milliseconds int64
	switch value := row[key].(type) {
	case float64:
		milliseconds = int64(value)
	case int64:
		milliseconds = value
	case int:
		milliseconds = int64(value)
	}
	if milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}
