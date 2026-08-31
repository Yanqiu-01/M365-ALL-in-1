package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionStore 与 sessionResolver 以前都读 M365_SESSION_CACHE，但写入的 JSON
// 形状互不兼容（前者是对象，后者是数组）。设了这个变量之后两者指向同一个文件，
// 各自 flush 时整文件覆写对方的内容，重启时 Unmarshal 失败且错误被丢弃，于是
// 至少有一方静默读出空数据。docker-compose.yml 的
// M365_SESSION_CACHE=/data/sessions.json 正好命中。
func TestSessionStoreAndResolverDoNotShareOneFile(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", shared)
	t.Setenv("M365_CONVERSATION_INDEX_CACHE", "")

	store := openSessionStore()
	resolver := openSessionResolver()

	if store.path == resolver.path {
		t.Fatalf("both stores write the same file %q; one silently overwrites the other", store.path)
	}
	// 两个路径都必须留在临时目录内，一个变量仍然能把两份状态一起挪走。
	for name, path := range map[string]string{"sessionStore": store.path, "sessionResolver": resolver.path} {
		if !strings.HasPrefix(path, dir) {
			t.Fatalf("%s path %q escaped the temp dir %q", name, path, dir)
		}
	}
	if resolver.path != shared {
		t.Fatalf("resolver path=%q, want the documented M365_SESSION_CACHE value %q", resolver.path, shared)
	}
}

// 端到端：两个 store 各写各的，落盘后重新打开，两份数据都必须还在。
func TestSessionStoreAndResolverBothSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_INDEX_CACHE", "")

	store := openSessionStore()
	saved := store.upsert(conversation{AccountID: "acc-1", ConversationID: "conv-1", SessionID: "sess-1"})
	if err := store.persist.flushNowBlocking(); err != nil {
		t.Fatalf("flush session store: %v", err)
	}

	resolver := openSessionResolver()
	resolver.Bind("sess-1", "conv-1", "acc-1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "第一轮问题"}}},
		"第一轮回答",
		resolverTestRequest("203.0.113.20", "client-a", "alice"))
	if err := resolver.persist.flushNowBlocking(); err != nil {
		t.Fatalf("flush resolver: %v", err)
	}

	// 重新打开两者：各自的数据都必须读回来。
	reopenedStore := openSessionStore()
	if _, ok := reopenedStore.get(saved.ID); !ok {
		data, _ := os.ReadFile(reopenedStore.path)
		t.Fatalf("conversation index lost its record after a reload; %s = %s", reopenedStore.path, data)
	}
	reopenedResolver := openSessionResolver()
	if _, ok := reopenedResolver.GetSession("sess-1"); !ok {
		data, _ := os.ReadFile(reopenedResolver.path)
		t.Fatalf("session binding lost after a reload; %s = %s", reopenedResolver.path, data)
	}
}

// 显式变量优先，且未配置任何变量时不落在仓库工作目录里。
func TestConversationIndexPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "index.json")
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_INDEX_CACHE", explicit)
	if got := conversationIndexPath(); got != explicit {
		t.Fatalf("explicit override ignored: got %q want %q", got, explicit)
	}

	t.Setenv("M365_CONVERSATION_INDEX_CACHE", "")
	t.Setenv("M365_SESSION_CACHE", "")
	if got := conversationIndexPath(); !strings.HasPrefix(got, os.TempDir()) {
		t.Fatalf("unconfigured default %q should live under the temp dir, not the working directory", got)
	}
}
