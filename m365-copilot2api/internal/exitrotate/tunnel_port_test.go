package exitrotate

import "testing"

// 隧道端口必须和注册实际使用的地址同源。
//
// 原先 EnsureTunnel 把本地端口写死成 1081，只是碰巧和 DefaultPhoneSOCKS 相同。面板把
// phone_socks 换到别的端口后，adb forward / phone-socks 仍建在 1081，而探测和注册打的是
// 配置里的端口 —— 那个端口上有别的代理在听时，隧道报「一切正常」，注册却从另一个出口发出。
func TestSocksPortIsTheSingleSourceOfTheTunnelPort(t *testing.T) {
	if got := socksPort(DefaultPhoneSOCKS); got != 1081 {
		t.Fatalf("socksPort(DefaultPhoneSOCKS) = %d，想要 1081", got)
	}
	cases := map[string]int{
		"socks5://127.0.0.1:1080":           1080,
		"socks5h://user:pw@127.0.0.1:19999": 19999,
		"127.0.0.1:1082":                    1082,
		"  socks5://[::1]:1083  ":           1083,
		"socks5://127.0.0.1":                0,
		"":                                  0,
		"not a url at all":                  0,
	}
	for raw, want := range cases {
		if got := socksPort(raw); got != want {
			t.Fatalf("socksPort(%q) = %d，想要 %d", raw, got, want)
		}
	}
}
