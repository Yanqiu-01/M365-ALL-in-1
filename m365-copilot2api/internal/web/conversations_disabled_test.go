package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 对话管理面板默认关闭（M365_CONVERSATION_PANEL=1 才开）。这一组测试守两件
// 事：默认真的是关的，以及关闭时那几条端点一步都不往下走 —— 不问
// sessionResolver、不写 historyArchive、不碰 M365 云端。
//
// 为什么值得单独一组：省内存这件事全靠「关闭时什么都不做」。只要有一条端点
// 在返回 503 之前顺手 ListSessions() 了一次，或者哪个破坏性端点漏了守卫，
// 这个开关就等于白关 —— 而这类回归从响应体上完全看不出来。
//
// panelDisabledResponse 解析统一的停用响应，并断言它是机器可读的。
func panelDisabledResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	// Some unrelated test packages enable the public-identity rewrite globally.
	// The panel contract must still advertise the actual recovery environment
	// variable, so isolate that optional presentation policy here.
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("停用响应不是 JSON，调用方无法分辨原因: %v (%s)", err, rec.Body.String())
	}
	if out.Error.Type != "conversation_panel_disabled" {
		t.Errorf("error.type=%q want conversation_panel_disabled", out.Error.Type)
	}
}

func TestConversationPanelDefaultsOff(t *testing.T) {
	t.Setenv(envConversationPanel, "")
	if conversationPanelEnabled() {
		t.Fatal("面板必须默认关闭，只能显式打开")
	}
	// 无法识别的值一律按关闭处理 —— 与仓库里其他布尔开关同一套保守取值。
	for _, v := range []string{"0", "no", "off", "false", "maybe", " "} {
		t.Setenv(envConversationPanel, v)
		if conversationPanelEnabled() {
			t.Errorf("M365_CONVERSATION_PANEL=%q 被当成了打开", v)
		}
	}
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		t.Setenv(envConversationPanel, v)
		if !conversationPanelEnabled() {
			t.Errorf("M365_CONVERSATION_PANEL=%q 应当打开面板", v)
		}
	}
}

// 关键断言：停用时端点不碰 resolver / archive / 云端。
//
// 手法是把这三样全部留成 nil —— sessionResolver 为 nil、historyArchive 为
// nil、m365CloudClient 为 nil。守卫漏了的话，下游要么 panic 要么走进
// session_store_unavailable / m365_not_configured 这些别的分支，两种情况都
// 会被这里抓住；只有「一步都没往下走」才能得到干净的 503 + panel_disabled。
func TestDisabledConversationEndpointsDoNotTouchResolverOrCloud(t *testing.T) {
	t.Setenv(envConversationPanel, "")
	// Error formatting is evaluated while the handler runs, so neutralize this
	// unrelated presentation policy before issuing the guarded requests.
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	oldCloudClient := m365CloudClient
	m365CloudClient = nil
	defer func() { m365CloudClient = oldCloudClient }()

	s := &Server{}
	for _, test := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		request *http.Request
	}{
		{
			name:    "list",
			handler: s.handleM365Conversations,
			request: httptest.NewRequest(http.MethodGet, "/api/m365/conversations", nil),
		},
		{
			// remote=1 是会真的打云端的那条路径。
			name:    "list-remote",
			handler: s.handleM365Conversations,
			request: httptest.NewRequest(http.MethodGet, "/api/m365/conversations?remote=1", nil),
		},
		{
			name:    "detail",
			handler: s.conversationDetail,
			request: httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=conv-1", nil),
		},
		{
			// 缺 id 时也必须先答「面板关了」：调用方需要知道的是功能停用，
			// 而不是先被引去修一个此刻毫无意义的参数。
			name:    "detail-without-id",
			handler: s.conversationDetail,
			request: httptest.NewRequest(http.MethodGet, "/api/conversations/detail", nil),
		},
		{
			name:    "capture",
			handler: s.captureConversations,
			request: httptest.NewRequest(http.MethodPost, "/api/conversations/capture", strings.NewReader(`{"limit":10}`)),
		},
		{
			// 以下两条是破坏性的：一个没刷新的旧标签页或旧脚本仍然能 POST
			// 过来，停用期间一条云端对话都不许被删。
			name:    "m365-delete",
			handler: s.handleM365Delete,
			request: httptest.NewRequest(http.MethodPost, "/api/m365/conversations/delete", strings.NewReader(`{"conversation_id":"conv-1"}`)),
		},
		{
			name:    "m365-cleanup",
			handler: s.handleM365Cleanup,
			request: httptest.NewRequest(http.MethodPost, "/api/m365/conversations/cleanup", strings.NewReader(`{"keep_n":1}`)),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			test.handler(rec, test.request)
			panelDisabledResponse(t, rec)
		})
	}
}

// 停用时捕获端点一个字节都不该写进 History 目录。
func TestDisabledCaptureWritesNothingToTheArchive(t *testing.T) {
	t.Setenv(envConversationPanel, "")
	dir := t.TempDir()
	sr := newHistoryTestResolver()
	sr.Bind("sess-1", "conv-1", "acc-1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}, "", historyTestRequest())
	s := &Server{sessionResolver: sr, historyArchive: &historyArchiveStore{dir: dir, maxSnapshots: 10}}

	rec := httptest.NewRecorder()
	s.captureConversations(rec, httptest.NewRequest(http.MethodPost, "/api/conversations/capture", strings.NewReader(`{"limit":10}`)))
	panelDisabledResponse(t, rec)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("停用的捕获端点仍然往归档目录写了文件: %v", entries)
	}
}

// 已有会话在停用期间必须原样留着：这个开关只停面板，不删任何数据。
func TestDisablingThePanelDeletesNoSessionData(t *testing.T) {
	t.Setenv(envConversationPanel, "")
	sr := newHistoryTestResolver()
	sr.Bind("sess-keep", "conv-keep", "acc-1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "keep me"}}}, "", historyTestRequest())
	s := &Server{sessionResolver: sr}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/m365/conversations", nil),
		httptest.NewRequest(http.MethodGet, "/api/conversations/detail?id=conv-keep", nil),
	} {
		rec := httptest.NewRecorder()
		if strings.Contains(request.URL.Path, "detail") {
			s.conversationDetail(rec, request)
		} else {
			s.handleM365Conversations(rec, request)
		}
		panelDisabledResponse(t, rec)
	}

	if _, ok := sr.GetConversation("conv-keep"); !ok {
		t.Fatal("会话在面板停用后消失了；这个开关不该删任何数据")
	}
	if got := len(sr.ListSessions()); got != 1 {
		t.Fatalf("会话数=%d want 1", got)
	}
}

// probe=1 是前端唯一的开关探测口。打开时必须回 200 + enabled:true，关闭时走
// 统一的 503 —— 前端据此「非 200 一律按关闭」，旧版网关也不会误判。
func TestConversationPanelProbeReportsSwitchState(t *testing.T) {
	s := &Server{}

	t.Setenv(envConversationPanel, "")
	rec := httptest.NewRecorder()
	s.handleM365Conversations(rec, httptest.NewRequest(http.MethodGet, "/api/m365/conversations?probe=1", nil))
	panelDisabledResponse(t, rec)

	// 打开后探测必须在 ListSessions 之前就答：resolver 留成 nil，一旦探测走
	// 到列表逻辑就会暴露（handleM365Conversations 对 nil resolver 是容错的，
	// 所以这里靠响应体断言 —— 拿到的必须是 panel 对象而不是 list）。
	t.Setenv(envConversationPanel, "1")
	rec = httptest.NewRecorder()
	s.handleM365Conversations(rec, httptest.NewRequest(http.MethodGet, "/api/m365/conversations?probe=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object  string `json:"object"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "conversation.panel" || !out.Enabled {
		t.Fatalf("probe response=%s", rec.Body.String())
	}
}
