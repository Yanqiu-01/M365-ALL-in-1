package turnstile

import "testing"

// 用户反复贴的报错是 ERR_PROXY_CONNECTION_FAILED —— 那是容器里的 Chrome 拿到一个它
// 到不了的代理。从容器内实测过：http 出口回 302，socks5 与 socks5h 都是 000，宿主的
// 127.0.0.1:7897 是容器自己的回环，永远连不上。这组用例把这条边界钉死，避免又把这类
// 出口递给 FlareSolverr。
func TestExitUsableByFlareSolverr(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://49.234.4.115:80", true},
		{"https://exit.example.test:8443", true},
		{"http://user:pass@1.2.3.4:8080", true},
		// 容器连不上宿主回环。这正是 clash 出口递进去的形态。
		{"http://127.0.0.1:7897", false},
		{"http://localhost:7897", false},
		{"http://[::1]:7897", false},
		{"http://0.0.0.0:7897", false},
		// socks5 在容器里实测 000。
		{"socks5://1.2.3.4:1080", false},
		{"socks5h://1.2.3.4:1080", false},
		{"socks5://127.0.0.1:1081", false},
		// 直连不是选项：用户明确要求注册不能走直连。
		{"", false},
		{"   ", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if got := ExitUsableByFlareSolverr(c.raw); got != c.want {
			t.Errorf("ExitUsableByFlareSolverr(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// 本机 Chrome 与容器的能力边界不同：它连得上回环，也认 socks5。把两者混成一套判据
// 会让 PC 白白丢掉大半个池子。
func TestExitUsableByChrome(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://49.234.4.115:80", true},
		{"http://127.0.0.1:7897", true},
		{"socks5://127.0.0.1:1081", true},
		{"socks5h://1.2.3.4:1080", true},
		{"", false},
		{"ftp://1.2.3.4:21", false},
		{"http://", false},
	}
	for _, c := range cases {
		if got := ExitUsableByChrome(c.raw); got != c.want {
			t.Errorf("ExitUsableByChrome(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// 池子已按 tier + score 排好，筛完必须保持原顺序 —— 否则「先用最好的」这件事就没了。
func TestFilterExitsKeepsOrderAndDedupes(t *testing.T) {
	in := []string{
		"http://a.example:80", "socks5://b.example:1080", "http://127.0.0.1:7897",
		"http://a.example:80", " http://c.example:8080 ", "",
	}
	got := FilterExits(in, ExitUsableByFlareSolverr)
	want := []string{"http://a.example:80", "http://c.example:8080"}
	if len(got) != len(want) {
		t.Fatalf("FilterExits() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FilterExits()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if FilterExits(in, nil) != nil {
		t.Error("nil predicate must not silently pass everything through")
	}
}

// chromeProxyArg：Chrome 会忽略 --proxy-server 里的 user:pass 然后弹认证框，凭据必须
// 走 Fetch.continueWithAuth；socks5h 是 curl 的写法，Chrome 只认 socks5。
func TestChromeProxyArg(t *testing.T) {
	cases := map[string]string{
		"http://1.2.3.4:8080":            "http://1.2.3.4:8080",
		"http://user:pass@1.2.3.4:8080":  "http://1.2.3.4:8080",
		"socks5h://user:pass@1.2.3.4:10": "socks5://1.2.3.4:10",
		"socks5://1.2.3.4:1080":          "socks5://1.2.3.4:1080",
		"1.2.3.4:8080":                   "1.2.3.4:8080",
	}
	for in, want := range cases {
		if got := chromeProxyArg(in); got != want {
			t.Errorf("chromeProxyArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// M365_CHROME_PATH 是用户唯一的手动出路，必须优先于内置清单。
func TestChromeCandidatesHonoursOverride(t *testing.T) {
	t.Setenv("M365_CHROME_PATH", `D:\browsers\chrome.exe`)
	got := chromeCandidates("windows")
	if len(got) != 1 || got[0] != `D:\browsers\chrome.exe` {
		t.Fatalf("override ignored: %#v", got)
	}
	t.Setenv("M365_CHROME_PATH", "")
	if len(chromeCandidates("windows")) == 0 {
		t.Error("windows must have built-in candidates")
	}
	if len(chromeCandidates("linux")) == 0 {
		t.Error("linux must have built-in candidates")
	}
}

// 成功文案里的 UPN 要取得出来，取不出来时必须返回空串让调用方回落到本地邮箱 ——
// 拿不到 UPN 不该把一个已经注册成功的号算成失败。
func TestUPNFromSuccessMessage(t *testing.T) {
	cases := map[string]string{
		"注册成功：24s055831@office.bo.edu.kg": "24s055831@office.bo.edu.kg",
		"registered: user@example.test":   "user@example.test",
		"24s055831@office.bo.edu.kg":      "24s055831@office.bo.edu.kg",
		"注册成功":                            "",
		"":                                "",
		"   ":                             "",
	}
	for in, want := range cases {
		if got := upnFromSuccessMessage(in); got != want {
			t.Errorf("upnFromSuccessMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
