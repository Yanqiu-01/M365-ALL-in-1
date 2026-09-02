package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

// 2026-09-02 17:44:45 线上抓到的 502（req b291e07b，POST /v1/messages，
// status=502 bytes=82 total_ms=6262）：调度器选中一个账号后，EnsureValid 去
// login.microsoftonline.com 兑换 refresh token，那次 POST 走的出口把 CONNECT
// 拒了，日志里是裸的
//
//	[account-route] resolve failed requested="" err=Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": Bad Gateway
//
// 请求在到达 router 之前就结束了（同一 id 只有 http_start / body_parsed /
// prompt_flattened 三条，没有 account-route selected、没有 chathub timing start）。
//
// 关键证据是 7 秒后那条 7ac0ef2c：raw_bytes=395095 messages=80 tools=18 与失败那条
// 一字节不差，换到另一个账号就 200 了。所以当时池子里有健康账号，只是这条路径不会
// 去找。outbound 侧也兜不住 —— POST 不幂等，safeRetryMethod（pool.go:817）对它返回
// false，那个 token POST 只有一次机会、一个出口。
//
// 顺带排除体积因素：同一失败类里还有一条 214 字节、2 条消息、0 个工具的请求，
// 体积不是判据。

// failingTokenEndpoint 把刷新 token 的目标指到本地，返回线上那次失败的形状。
//
// 走 M365_TOKEN_ENDPOINT（auth/config.go:49）注入。不这么做的话，过期账号会让
// EnsureValid 真的去打 login.microsoftonline.com —— 第一版就是这样写的，跑出来是
// AADSTS9002313 带 Trace ID，说明请求真的到了微软。那样测试既依赖外网，又是拿假
// refresh token 去换真端点，失败形状还和线上不是一回事（应用层 400 而不是传输层
// 502）。
// 端点指向一个已经关掉的监听口，于是 outbound.HTTPClient().Do 在传输层就返错
// （token.go:165-167 那一行返回），和线上「Do 返回裸 Bad Gateway」同一层。
//
// 起一个 httptest.Server 只为借它的端口号，随即关掉：这样拿到的一定是本机没人监听
// 的端口，不用猜也不会撞上别的服务。若改成让 handler 返 502，请求其实是成功的，
// 失败发生在 JSON 解码（token.go:174），比线上晚一层。
func failingTokenEndpoint(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	t.Setenv("M365_TOKEN_ENDPOINT", url+"/common/oauth2/v2.0/token")
}

func expiredAndValidAccounts(t *testing.T) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// u-1 已过期，EnsureValid 必然要去发刷新 POST，而端点被指到必败的本地服务，
	// 正是线上那次失败的形状。u-2 / u-3 未过期，EnsureValid 直接返回、不发请求。
	//
	// 零在途时 selectLeastInflight（resource_scheduler.go:62）按 ID 最小选，nil 调度器
	// 走的就是它（resource_scheduler.go:88），所以调度器必定先挑中 u-1 —— 这个测试
	// 要的就是「调度器选中的那个刷不动」。
	for _, tok := range []auth.TokenSet{
		{HomeOID: "u-1", Email: "one@example.com", AccessToken: "tok1", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour)},
		{HomeOID: "u-2", Email: "two@example.com", AccessToken: "tok2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)},
		{HomeOID: "u-3", Email: "three@example.com", AccessToken: "tok3", RefreshToken: "r3", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if _, err := store.Upsert(tok); err != nil {
			t.Fatalf("upsert %s: %v", tok.HomeOID, err)
		}
	}
	return store
}

func TestResolveAccountFailsOverWhenTokenRefreshFails(t *testing.T) {
	failingTokenEndpoint(t)
	s := &Server{tokens: expiredAndValidAccounts(t), accountPool: newAccountHealth()}

	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("调度器选中的账号刷 token 失败后应当换账号，却直接把错误抛给客户端"+
			"（线上就是这样返 502 的）: %v", err)
	}
	if acc.ID == "u-1" {
		t.Fatalf("failover 应当避开刷不动的那个账号，仍然拿到 %s", acc.ID)
	}
	if acc.ID == "" || acc.AccessToken == "" {
		t.Fatalf("failover 必须交出一个已验证的账号，拿到 id=%q token_present=%t",
			acc.ID, acc.AccessToken != "")
	}
}

// 客户端显式点名某个账号时不能悄悄换人：那是答非所问，而且会让按账号分流的客户端
// 拿到别人的会话。
func TestResolveAccountDoesNotFailOverAnExplicitlyRequestedAccount(t *testing.T) {
	failingTokenEndpoint(t)
	s := &Server{tokens: expiredAndValidAccounts(t), accountPool: newAccountHealth()}

	if acc, err := s.resolveAccount("u-1"); err == nil {
		t.Fatalf("显式指定 u-1 时不该 failover，却拿到了 %s", acc.ID)
	}
}

// failover 也失败时要抛原始错误。原始错误才带着正确的状态码映射（429 会被
// writeUpstreamError 补上 Retry-After，errors.go:132），换成 nextHealthyAccount 的
// 「no healthy account available for failover」会把客户端应有的退避提示弄丢，
// 让它把冷却期当上游故障立刻重试 —— server.go:1191 那段注释防的就是这件事。
func TestResolveAccountKeepsTheOriginalErrorWhenFailoverAlsoFails(t *testing.T) {
	failingTokenEndpoint(t)
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// 只有一个账号，且已过期：failover 无处可去。
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID: "solo", Email: "solo@example.com", AccessToken: "tok", RefreshToken: "r",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	_, err = s.resolveAccount("")
	if err == nil {
		t.Fatal("唯一账号刷不动且无处 failover 时必须报错")
	}
	if strings.Contains(err.Error(), "no healthy account available for failover") {
		t.Errorf("抛出的是 failover 的错误而不是原始错误，状态码映射和 Retry-After 会丢: %v", err)
	}
}
