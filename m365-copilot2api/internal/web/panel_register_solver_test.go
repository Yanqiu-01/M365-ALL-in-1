package web

import (
	"strings"
	"testing"

	"m365-copilot2api/internal/turnstile"
)

// solver 这个键存在的意义是让「用哪个后端」成为可表达的意图。默认偏向浏览器是因为
// FlareSolverr 在这个站点上结构性地拿不到 token（只能回页面快照，而 token 在 DOM
// property 上），但用户显式写了 flaresolverr 时不能被代码悄悄改掉。
func TestUseChromeSolverHonoursExplicitChoice(t *testing.T) {
	var cfg nativePanelFileConfig

	cfg.Register.Solver = "flaresolverr"
	if useChromeSolver(cfg) {
		t.Error("solver=flaresolverr must not be silently upgraded to the browser")
	}
	cfg.Register.Solver = "flare"
	if useChromeSolver(cfg) {
		t.Error("solver=flare should be accepted as the same intent")
	}

	// auto 与 chrome 都取决于本机有没有浏览器 —— 在没有浏览器的机器上跑 CI 时，
	// 这两种都必须回落，而不是硬撑着走一条必然失败的路。
	available := turnstile.ChromeAvailable()
	for _, value := range []string{"", "auto", "chrome", "browser", "edge", "AUTO"} {
		cfg.Register.Solver = value
		if got := useChromeSolver(cfg); got != available {
			t.Errorf("solver=%q => %v, want %v (ChromeAvailable=%v)", value, got, available, available)
		}
	}
}

// 出口判据必须跟着后端走：递给容器里的 FlareSolverr 一个回环地址或 socks5 出口，
// 报错就是用户反复贴的 ERR_PROXY_CONNECTION_FAILED，而那条消息指向浏览器，看不出是
// 出口挑错了。
func TestSolverExitPredicateMatchesBackend(t *testing.T) {
	pool := []string{
		"http://127.0.0.1:7897",
		"socks5://127.0.0.1:1081",
		"http://49.234.4.115:80",
		"socks5://1.2.3.4:1080",
	}
	flare := turnstile.FilterExits(pool, turnstile.ExitUsableByFlareSolverr)
	if len(flare) != 1 || flare[0] != "http://49.234.4.115:80" {
		t.Fatalf("FlareSolverr candidates = %#v; only non-loopback http exits work from the container", flare)
	}
	chrome := turnstile.FilterExits(pool, turnstile.ExitUsableByChrome)
	if len(chrome) != 4 {
		t.Fatalf("Chrome candidates = %#v; the local browser reaches loopback and socks5 too", chrome)
	}
}

// 出口会进 Notes，而 Notes 原样进管理接口的响应，也就是进浏览器的 devtools。
// 池子里的条目可能带 user:pass，不能整条抄进去。
func TestRedactExitForNoteHidesCredentials(t *testing.T) {
	cases := map[string]string{
		"http://user:secret@1.2.3.4:8080": "http://***@1.2.3.4:8080",
		"http://1.2.3.4:8080":             "http://1.2.3.4:8080",
		"socks5://1.2.3.4:1080":           "socks5://1.2.3.4:1080",
		"":                                "(已隐藏)",
		"::::":                            "(已隐藏)",
	}
	for in, want := range cases {
		got := redactExitForNote(in)
		if got != want {
			t.Errorf("redactExitForNote(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("redactExitForNote(%q) leaked the password: %q", in, got)
		}
	}
}
