package web

// 「注册成功但拿不到 token」的号必须在任务终态上看得见。
//
// 原先长跑任务只把 report.Imported 累进状态，pending / failed 一个都不进，逐个号的原因也
// 从不落日志。而 Imported 在健康的一批里恒为 0 —— 号在注册时已经内联授权过，补齐把它们
// 全部判成 skipped —— 所以「全都好」和「一个都没拿到」在面板上长得一模一样。
//
// pending 尤其不能当成「稍后会好」：它意味着 ROPC 被拒、退到了设备码或 PKCE，两条都要人
// 去另一台设备上点，而长跑是无人值守的。号已经建在站点上了，token 永远拿不到。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"m365-copilot2api/internal/auth"
)

// refusingAuthority 起一个假的 Microsoft 端点：ROPC 一律拒绝，设备码正常发放。
// 这正是 pending 的成因 —— 拿到一串要人去网页上敲的码，而没有任何 token 落库。
func refusingAuthority(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "invalid_grant",
				"error_description": "AADSTS50126: 凭据无效",
			})
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/devicecode"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "device-code-value",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://microsoft.test/devicelogin",
				"expires_in":       900,
				"interval":         5,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("M365_AUTHORITY", server.URL+"/common")
}

// oauthVisibilityServer 造一个 token store 为空的 Server 和一个指向 root 的 manager。
func oauthVisibilityServer(t *testing.T, root string) (*Server, *nativePanelManager) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{tokens: store}, newNativePanelManager(nativePanelConfig{Root: root})
}

// 补齐跑完之后仍然没有 token 的号必须被数出来，并且逐个把原因写进日志。
func TestBackfillOAuthCountsAccountsLeftWithoutToken(t *testing.T) {
	refusingAuthority(t)
	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	// email_prefix=24s05、email_start_num=5026，所以 5026 / 5027 两个号在清单里。
	if err := os.WriteFile(filepath.Join(root, "data", "credentials.txt"),
		[]byte("24s055026@office.example.test----Passw0rd!\n24s055027@office.example.test----Passw0rd!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, manager := oauthVisibilityServer(t, root)
	job := &registerJob{}

	server.backfillOAuth(context.Background(), manager, job, 5026, 5027)

	state := job.snapshot()
	if state.OAuthMissing != 2 {
		t.Fatalf("oauthMissing = %d，两个号都没拿到 token，想要 2：%#v", state.OAuthMissing, state)
	}
	if state.OAuthErrors != 0 {
		t.Fatalf("oauthErrors = %d，补齐调用本身是成功的，想要 0", state.OAuthErrors)
	}
	if state.Imported != 0 {
		t.Fatalf("imported = %d，一个 token 都没拿到，想要 0", state.Imported)
	}
	// 逐个号的原因是事后唯一能知道「该重授权哪些号、为什么没成」的东西。
	notes := strings.Join(state.Notes, "\n")
	for _, email := range []string{"24s055026@office.example.test", "24s055027@office.example.test"} {
		if !strings.Contains(notes, email) {
			t.Fatalf("日志里没有 %s：%q", email, notes)
		}
	}
	if !strings.Contains(notes, "AADSTS50126") {
		t.Fatalf("日志里没有拿不到 token 的原因：%q", notes)
	}
	// 终态说明也得带上这个数：内存里那 40 条 note 会被后面的批次挤掉。
	if suffix := registerJobOAuthSuffix(&state); !strings.Contains(suffix, "2 个号没拿到 token") {
		t.Fatalf("终态后缀 = %q，想要带上「2 个号没拿到 token」", suffix)
	}
}

// 整批补齐调用直接出错时同样不能静默：要计数、要带号段。
func TestBackfillOAuthCountsWholeBatchErrors(t *testing.T) {
	root := t.TempDir()
	writePanelConfig(t, root, "http://127.0.0.1:1")
	// 清单为空 -> emailsInRange 报「没有编号在 5026-5027 之间的账号」，整批调用失败。
	if err := os.WriteFile(filepath.Join(root, "data", "credentials.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server, manager := oauthVisibilityServer(t, root)
	job := &registerJob{}

	server.backfillOAuth(context.Background(), manager, job, 5026, 5027)

	state := job.snapshot()
	if state.OAuthErrors != 1 {
		t.Fatalf("oauthErrors = %d，整批补齐失败了一次，想要 1：%#v", state.OAuthErrors, state)
	}
	// 整批出错时 report 是空的，这一段有几个号没 token 无从得知 —— 不能编一个数出来。
	if state.OAuthMissing != 0 {
		t.Fatalf("oauthMissing = %d，整批出错时号数是未知的，不该编一个数，想要 0", state.OAuthMissing)
	}
	notes := strings.Join(state.Notes, "\n")
	if !strings.Contains(notes, "5026-5027") {
		t.Fatalf("日志没带号段，事后不知道该补哪一段：%q", notes)
	}
	if suffix := registerJobOAuthSuffix(&state); !strings.Contains(suffix, "1 批补齐调用出错") {
		t.Fatalf("终态后缀 = %q，想要带上「1 批补齐调用出错」", suffix)
	}
}
