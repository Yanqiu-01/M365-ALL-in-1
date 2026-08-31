package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanupTestServer 造一个只带对话相关字段的最小 Server，两个 store 都钉在
// 临时目录里，不经过 New()，因此不碰真实配置目录，也不碰存储密钥。
func cleanupTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_INDEX_CACHE", filepath.Join(dir, "index.json"))
	cm := openConversationManager()
	if !strings.HasPrefix(cm.path, dir) {
		t.Fatalf("conversation manager path %q escaped %q", cm.path, dir)
	}
	sessions := openSessionStore()
	if !strings.HasPrefix(sessions.path, dir) {
		t.Fatalf("session store path %q escaped %q", sessions.path, dir)
	}
	return &Server{conversationManager: cm, sessions: sessions}
}

func postCleanup(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/cleanup", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.conversationCleanup(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// keep_n 以前被解码后从未使用，接口却照样回 "cleaned"：调用方以为保留条数生效了。
// 这里在 keep_n 模式下放 6 条对话，请求 keep_n=2，必须真的只剩 2 条。
func TestConversationCleanupHonoursKeepN(t *testing.T) {
	s := cleanupTestServer(t)
	base := time.Now().UTC()
	for i := 0; i < 6; i++ {
		s.conversationManager.mu.Lock()
		id := "conv-" + string(rune('a'+i))
		s.conversationManager.data[id] = managedConversation{
			ID:         id,
			CreatedAt:  base.Add(time.Duration(i) * time.Minute),
			LastUsedAt: base.Add(time.Duration(i) * time.Minute),
		}
		s.conversationManager.mu.Unlock()
	}

	rec, out := postCleanup(t, s, `{"mode":"keep_n","keep_n":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.conversationManager.KeepN(); got != 2 {
		t.Fatalf("keep_n was decoded but never applied: manager keepN=%d, want 2", got)
	}
	if remaining, _ := out["remaining"].(float64); int(remaining) != 2 {
		t.Fatalf("remaining=%v want 2; body=%s", out["remaining"], rec.Body.String())
	}
	if len(s.conversationManager.List()) != 2 {
		t.Fatalf("manager still holds %d conversations, want 2", len(s.conversationManager.List()))
	}
	// 响应必须回显真正生效的 keep_n，否则调用方无从确认参数被采纳。
	if echoed, _ := out["keep_n"].(float64); int(echoed) != 2 {
		t.Fatalf("response does not echo the applied keep_n: %s", rec.Body.String())
	}
}

// 越界与非法的 keep_n 必须被拒绝，而不是解码后丢掉再回 "cleaned"。
func TestConversationCleanupRejectsOutOfRangeKeepN(t *testing.T) {
	for _, body := range []string{
		`{"mode":"keep_n","keep_n":-1}`,
		`{"mode":"keep_n","keep_n":100000}`,
	} {
		s := cleanupTestServer(t)
		rec, _ := postCleanup(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d want 400 (%s)", body, rec.Code, rec.Body.String())
		}
		// 被拒的请求不得改动已生效的配置。
		if got := s.conversationManager.KeepN(); got != 5 {
			t.Fatalf("rejected request changed keepN to %d", got)
		}
	}
}

// 未实现的清理模式必须被拒绝：Cleanup 的 switch 对未知模式什么也不做，
// 接受任意字符串等于悄悄把自动清理关掉，而接口仍然回 "cleaned"。
func TestConversationCleanupRejectsUnknownMode(t *testing.T) {
	s := cleanupTestServer(t)
	rec, _ := postCleanup(t, s, `{"mode":"delete_everything"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
	}
	if got := s.conversationManager.Mode(); got != CleanupAfterResponse {
		t.Fatalf("unknown mode was applied: %q", got)
	}
}

// 不带参数与空请求体都是合法调用：按当前配置清理一次。
func TestConversationCleanupAcceptsEmptyBody(t *testing.T) {
	for _, body := range []string{"", "{}"} {
		s := cleanupTestServer(t)
		rec, out := postCleanup(t, s, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %q: status=%d want 200 (%s)", body, rec.Code, rec.Body.String())
		}
		if out["status"] != "cleaned" {
			t.Fatalf("body %q: payload=%v", body, out)
		}
	}
}

// 坏 JSON 不能被当成「没带参数」：以前所有解码错误都被忽略，然后照样回 "cleaned"。
func TestConversationCleanupRejectsBadJSON(t *testing.T) {
	s := cleanupTestServer(t)
	rec, _ := postCleanup(t, s, `{"mode":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
	}
}
