package web

import (
	"os"
	"strings"
	"testing"
)

// 账密清单的分隔符在三处出现：native_panel.go 解析凭据文件、credential_sync.go
// 的 cred_file，以及前端「账密一键回调」的批量列表。三者必须是同一个分隔符，
// 否则同一份 credentials.txt 在页面里粘贴后会整批解析失败，而后端毫无察觉。
// 这里锁定该不变式，而不是断言某段 UI 文案存在。
const credentialListSeparator = "----"

func TestCredentialSeparatorSharedByPanelAndUI(t *testing.T) {
	panelSrc, err := os.ReadFile("native_panel.go")
	if err != nil {
		t.Fatal(err)
	}
	// 后端逐行解析用的就是这个分隔符。
	if !strings.Contains(string(panelSrc), `strings.Cut(strings.TrimSpace(scanner.Text()), "`+credentialListSeparator+`")`) {
		t.Fatalf("native_panel.go 不再用 %q 切分凭据行，前端批量列表会随之失效", credentialListSeparator)
	}

	page, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(page)
	if !strings.Contains(ui, "const CRED_SEP='"+credentialListSeparator+"';") {
		t.Errorf("index.html 的 CRED_SEP 必须是 %q，与后端解析保持一致", credentialListSeparator)
	}
}

// 批量回调只允许复用既有单账号端点，不得新增协议。若哪天前端改成打一个
// 不存在的 /api/accounts/web/run-scripts-bulk，这里会立刻失败。
func TestBulkCredentialCallbackReusesSingleAccountEndpoint(t *testing.T) {
	page, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(page)

	for _, required := range []string{
		"async function submitBulkRunScripts(",
		"function parseBulkCreds(",
		"function importBulkFile(",
		`id="rsBulk"`,
		`id="rsBulkFile"`,
	} {
		if !strings.Contains(ui, required) {
			t.Errorf("批量账密回调缺少 %q", required)
		}
	}

	if strings.Contains(ui, "run-scripts-bulk") {
		t.Error("批量回调不应新增端点，必须逐项复用 /api/accounts/web/run-scripts")
	}

	// 单账号路径必须保留：批量只是新增入口，不是替换。
	for _, kept := range []string{`id="rsEmail"`, `id="rsPassword"`, "/api/accounts/web/run-scripts"} {
		if !strings.Contains(ui, kept) {
			t.Errorf("单账号回调路径被破坏，缺少 %q", kept)
		}
	}
}

// 「添加账号」按用户要求默认收起，但手动 OAuth 能力必须仍在页面里，
// 且折叠必须由 HTML 初始 class 决定（避免首屏展开后被 JS 收回的闪动）。
func TestAddAccountCardCollapsedByDefaultWithoutLosingOAuth(t *testing.T) {
	page, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(page)

	idx := strings.Index(ui, `id="addAccountCard"`)
	if idx < 0 {
		t.Fatal(`index.html 缺少 id="addAccountCard"`)
	}
	// 折叠状态写在同一个 div 的 class 上，而不是等 JS 后补。
	open := strings.LastIndex(ui[:idx], "<div")
	if open < 0 {
		t.Fatal("找不到 addAccountCard 的起始标签")
	}
	tag := ui[open:idx]
	if !strings.Contains(tag, "is-collapsed") || !strings.Contains(tag, "is-collapsible") {
		t.Errorf("添加账号卡片首屏应带 is-collapsible is-collapsed，实际标签为 %q", tag)
	}

	if !strings.Contains(ui, "function toggleCardCollapse(") {
		t.Error("缺少展开/收起处理函数，卡片将无法手动展开")
	}
	if !strings.Contains(ui, ".card.is-collapsed>.card-body{display:none}") {
		t.Error("缺少折叠样式，is-collapsed 不会真正收起卡片内容")
	}

	// 折叠不得删除任何既有授权能力。
	for _, kept := range []string{"startPKCE(true)", "submitCallback()", "resetPKCE()", `id="callbackInput"`} {
		if !strings.Contains(ui, kept) {
			t.Errorf("折叠改动误删了授权能力 %q", kept)
		}
	}
}
