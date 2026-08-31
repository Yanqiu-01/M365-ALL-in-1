package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clashConfigTestRoot 把面板数据目录、账号存储与存储密钥全部钉进 TempDir。
//
// 三者必须一起隔离：面板会在 root 下建目录并写 config.json，而 isolateStoreKey
// 负责 auth.storeKeyPath 那条「os.UserHomeDir() 成功时忽略传入路径」的分支 ——
// 少了它测试会读写真机上的 m365-store.key。
func clashConfigTestRoot(t *testing.T) string {
	t.Helper()
	isolateStoreKey(t) // 先调：它自己 Setenv 了 M365_DATA_DIR
	dir := t.TempDir()
	t.Setenv(nativePanelRootEnv, dir)
	t.Setenv("M365_CONFIG", filepath.Join(dir, "config.json"))
	// 自证隔离生效，而不是假定它生效。
	if root := os.Getenv(nativePanelRootEnv); !strings.HasPrefix(root, dir) {
		t.Fatalf("panel root %q escaped %q", root, dir)
	}
	return dir
}

// clashConfigTestManager 造一个数据目录钉在 TempDir 的面板管理器。
func clashConfigTestManager(t *testing.T) *nativePanelManager {
	t.Helper()
	dir := clashConfigTestRoot(t)
	manager := newNativePanelManager(nativePanelConfig{Root: dir})
	if manager == nil {
		t.Fatal("newNativePanelManager returned nil")
	}
	paths, _, err := manager.panelData()
	if err != nil {
		t.Fatalf("panelData: %v", err)
	}
	if !strings.HasPrefix(paths.configPath, dir) {
		t.Fatalf("config path %q escaped the temp root %q", paths.configPath, dir)
	}
	return manager
}

// clash 模式注册要求 cfg.Register.ClashNodes 非空，可这个字段以前根本没有写入
// 入口：nativePanelRegisterConfigRequest 里没有对应字段，配置结构里的
// clash_nodes 又是匿名结构体，saveRegisterConfig 连值都构造不出来。于是界面上
// 选「Clash 节点」必然以 "clash api, group and node are required" 失败。
func TestSaveRegisterConfigPersistsClashNodes(t *testing.T) {
	manager := clashConfigTestManager(t)
	cfg, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		ClashAPI:   "http://127.0.0.1:9090/",
		ClashGroup: "Proxy",
		ClashNodes: &[]nativePanelClashNodeRequest{
			{Name: "香港01", ExpectIP: "203.0.113.7"},
			{Name: " 日本02 "},
			{Name: "   "},  // 空名必须丢掉：rotateClash 对空节点名一律报错
			{Name: "香港01"}, // 重复必须去掉，否则轮换会在同一个出口上停两轮
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := clashNodes(cfg)
	if len(nodes) != 2 {
		t.Fatalf("节点数 = %d，期望 2（空名与重复要被丢掉）：%+v", len(nodes), nodes)
	}
	if nodes[0].Name != "香港01" || nodes[0].ExpectIP != "203.0.113.7" {
		t.Fatalf("第一个节点 = %+v", nodes[0])
	}
	if nodes[1].Name != "日本02" {
		t.Fatalf("第二个节点名没有 trim：%q", nodes[1].Name)
	}
	if nodes[1].ExpectIP != "" {
		t.Fatalf("未填的 expect_ip 不该被造出值：%q", nodes[1].ExpectIP)
	}

	// 必须真的落盘：下一次注册读的是文件，不是内存。
	paths, reloaded, err := manager.panelData()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(clashNodes(reloaded)); got != 2 {
		t.Fatalf("重新读取配置后节点数 = %d，期望 2", got)
	}
	body, err := os.ReadFile(paths.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"clash_nodes"`) || !strings.Contains(string(body), "203.0.113.7") {
		t.Fatalf("config.json 里没有节点表：%s", body)
	}
}

// 没提 clashNodes 字段就保持不变；显式传空数组才是清空。
// 两者必须区分，否则任何一次不相关的配置保存都会把节点表抹掉。
func TestSaveRegisterConfigClashNodesOmittedKeepsExisting(t *testing.T) {
	manager := clashConfigTestManager(t)
	if _, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		ClashNodes: &[]nativePanelClashNodeRequest{{Name: "香港01"}},
	}); err != nil {
		t.Fatal(err)
	}
	// 不提 ClashNodes 的一次保存。
	cfg, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{SiteURL: "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(clashNodes(cfg)) != 1 {
		t.Fatal("没有提到 clashNodes 的保存把已存的节点表清掉了")
	}
	// 显式空数组 = 清空。
	empty := []nativePanelClashNodeRequest{}
	cfg, err = manager.saveRegisterConfig(nativePanelRegisterConfigRequest{ClashNodes: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if len(clashNodes(cfg)) != 0 {
		t.Fatal("显式传空数组应当清空节点表")
	}
}

// 配置往返必须闭合：state 要回传节点清单，否则界面只知道有几个而无法显示或编辑。
func TestPanelStateReturnsClashNodes(t *testing.T) {
	manager := clashConfigTestManager(t)
	if _, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		ClashAPI:    "http://127.0.0.1:9090",
		ClashGroup:  "Proxy",
		ClashSecret: "s3cret",
		ClashNodes:  &[]nativePanelClashNodeRequest{{Name: "香港01", ExpectIP: "203.0.113.7"}},
	}); err != nil {
		t.Fatal(err)
	}
	server, _ := panelTestServer(t)
	state := manager.state(server)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "香港01") || !strings.Contains(string(encoded), "203.0.113.7") {
		t.Fatalf("state 没有回传节点清单：%s", encoded)
	}
	if total, _ := state["clash_node_total"].(int); total != 1 {
		t.Fatalf("clash_node_total = %v，期望 1", state["clash_node_total"])
	}
	// 密钥绝不回显明文。
	if strings.Contains(string(encoded), "s3cret") {
		t.Fatalf("state 回显了 clash 密钥明文：%s", encoded)
	}
}

// clashNodes 走 HTTP 也必须收得下。nativePanelDecodeJSON 用了
// DisallowUnknownFields，字段不存在时整个请求会被 400 拒掉 —— 这正是界面无法
// 配置节点表的直接原因。
func TestPanelConfigEndpointAcceptsClashNodes(t *testing.T) {
	dir := clashConfigTestRoot(t)
	server, cookie := panelTestServer(t)
	manager := newNativePanelManager(nativePanelConfig{Root: dir})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		newNativePanelController(server, manager).ServeHTTP(recorder, request)
		return recorder
	}
	body := `{"clashApi":"http://127.0.0.1:9090","clashGroup":"Proxy",` +
		`"clashNodes":[{"name":"香港01","expectIp":"203.0.113.7"},{"name":"日本02"}]}`
	rec := do(http.MethodPost, "/api/admin/panel/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	state := do(http.MethodGet, "/api/admin/panel/state", "")
	if state.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", state.Code, state.Body.String())
	}
	if !strings.Contains(state.Body.String(), "香港01") {
		t.Fatalf("保存后的节点没有出现在 state 里：%s", state.Body.String())
	}
	if !strings.Contains(state.Body.String(), "203.0.113.7") {
		t.Fatalf("expect_ip 没有回传：%s", state.Body.String())
	}
}

// clash 模式缺配置时必须一次性说清缺什么、去哪里填，而不是让 rotateClash 在
// 第二个账号那里抛 "clash api, group and node are required"。
func TestRunRegisterRejectsIncompleteClashConfigUpFront(t *testing.T) {
	manager := clashConfigTestManager(t)
	if _, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		SiteURL:     "https://example.test",
		EmailDomain: "example.test",
		EmailPrefix: "u",
		Password:    "pw",
	}); err != nil {
		t.Fatal(err)
	}
	var s *Server
	_, err := s.runRegister(nil, manager, panelRegisterRequest{Mode: "clash", Count: 2})
	if err == nil {
		t.Fatal("clash 模式缺 api/group/nodes 时必须直接拒绝")
	}
	for _, want := range []string{"clash_api", "clash_group", "clash_nodes", "clashNodes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q：%v", want, err)
		}
	}
}

// 配齐之后这道前置检查必须放行 —— 它不能变成一道永远过不去的门。
func TestRunRegisterAcceptsCompleteClashConfig(t *testing.T) {
	manager := clashConfigTestManager(t)
	if _, err := manager.saveRegisterConfig(nativePanelRegisterConfigRequest{
		SiteURL:     "https://example.test",
		EmailDomain: "example.test",
		EmailPrefix: "u",
		Password:    "pw",
		ClashAPI:    "http://127.0.0.1:9090",
		ClashGroup:  "Proxy",
		ClashNodes:  &[]nativePanelClashNodeRequest{{Name: "香港01"}},
	}); err != nil {
		t.Fatal(err)
	}
	var s *Server
	_, err := s.runRegister(nil, manager, panelRegisterRequest{Mode: "clash", Count: 1})
	if err != nil && strings.Contains(err.Error(), "clash 模式配置不完整") {
		t.Fatalf("配置已齐备却仍被前置检查拒绝：%v", err)
	}
}
