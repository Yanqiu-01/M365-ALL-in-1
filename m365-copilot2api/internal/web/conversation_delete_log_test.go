package web

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLog 把标准 log 输出接到 buffer 上，用来断言「日志说的和接口做的一致」。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags, writer := log.Flags(), log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return &buf
}

func postDeleteConversation(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/delete", strings.NewReader(`{"id":"`+id+`"}`))
	rec := httptest.NewRecorder()
	s.deleteConversation(rec, req)
	return rec
}

// 删除一个根本不存在的 ID：以前 conversationManager.Delete 无条件打印
// "deleted conversation <id>"，接口随后回 404。日志说删了，接口说没找到，
// 两边至多一个是真的 —— 排查时无从分辨真假删除。
func TestDeleteMissingConversationDoesNotLogADeletion(t *testing.T) {
	s := cleanupTestServer(t)
	logs := captureLog(t)

	rec := postDeleteConversation(t, s, "conv-does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "deleted conversation") {
		t.Fatalf("log claims a deletion that never happened while the response was 404: %q", logs.String())
	}
}

// 真的删掉时，日志必须记下来，接口必须回 deleted。
func TestDeleteExistingConversationLogsAndReports(t *testing.T) {
	s := cleanupTestServer(t)
	s.conversationManager.Record("conv-real", "acc-1", "标题")
	saved := s.sessions.upsert(conversation{ID: "conv-real", ConversationID: "conv-real", AccountID: "acc-1"})
	logs := captureLog(t)

	rec := postDeleteConversation(t, s, saved.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "deleted conversation conv-real") {
		t.Fatalf("a real deletion went unlogged: %q", logs.String())
	}
	if _, ok := s.sessions.get(saved.ID); ok {
		t.Fatal("conversation still present in the session index")
	}
	if len(s.conversationManager.List()) != 0 {
		t.Fatalf("conversation still tracked by the manager: %+v", s.conversationManager.List())
	}
}

// ID 只存在于 conversationManager（会话索引里没有）时，记录确实被删掉了，
// 接口就不能回 404 —— 否则调用方以为什么都没发生，实际状态已经变了。
func TestDeleteConversationKnownOnlyToTheManagerReportsDeleted(t *testing.T) {
	s := cleanupTestServer(t)
	s.conversationManager.Record("manager-only", "acc-1", "标题")

	rec := postDeleteConversation(t, s, "manager-only")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; the record was removed yet the caller was told 404: %s", rec.Code, rec.Body.String())
	}
	if len(s.conversationManager.List()) != 0 {
		t.Fatalf("manager still tracks the conversation: %+v", s.conversationManager.List())
	}
}

// Delete 的返回值必须如实反映有没有删掉东西。
func TestConversationManagerDeleteReportsWhetherItRemovedAnything(t *testing.T) {
	s := cleanupTestServer(t)
	if s.conversationManager.Delete("never-recorded") {
		t.Fatal("Delete reported a removal for an unknown id")
	}
	s.conversationManager.Record("recorded", "acc-1", "标题")
	if !s.conversationManager.Delete("recorded") {
		t.Fatal("Delete reported no removal for a recorded id")
	}
	if s.conversationManager.Delete("recorded") {
		t.Fatal("Delete reported a second removal for the same id")
	}
}

// Mode/ShouldCleanup 与 SetMode 并发时不得竞争（-race 下会直接报告）。
func TestConversationManagerModeIsRaceFree(t *testing.T) {
	s := cleanupTestServer(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(50 * time.Millisecond)
		for time.Now().Before(deadline) {
			s.conversationManager.SetMode(CleanupKeepN)
			s.conversationManager.SetMode(CleanupAfterResponse)
		}
	}()
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = s.conversationManager.Mode()
		_ = s.conversationManager.ShouldCleanup()
	}
	<-done
}
