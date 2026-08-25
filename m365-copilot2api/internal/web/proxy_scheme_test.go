package web

import (
	"testing"
)

// 买来的 IP 单子有的带 socks5:// 前缀、有的是裸 1.2.3.4:8080。
// 裸的过去被 outbound.New 直接拒掉（"must be a complete ... URL"），整批 500。
func TestFallbackSchemeOnlyFillsMissingPrefix(t *testing.T) {
	cases := []struct {
		in       string
		fallback string
		want     string
	}{
		// 裸 ip:port 补上兜底协议
		{"45.33.99.1:8080", "socks5", "socks5://45.33.99.1:8080"},
		{"45.33.99.2:3128", "http", "http://45.33.99.2:3128"},
		// 已有前缀的一律原样保留，混合单子里每行以自己写的为准
		{"socks5://45.33.99.3:1080", "http", "socks5://45.33.99.3:1080"},
		{"http://45.33.99.4:8080", "socks5", "http://45.33.99.4:8080"},
		{"https://45.33.99.5:443", "socks5", "https://45.33.99.5:443"},
		// 带凭据的也不能被破坏
		{"user:pass@45.33.99.6:8080", "http", "http://user:pass@45.33.99.6:8080"},
		{"socks5://user:pass@45.33.99.7:1080", "http", "socks5://user:pass@45.33.99.7:1080"},
		// 空兜底退回 http
		{"45.33.99.8:8080", "", "http://45.33.99.8:8080"},
		// 空输入不炸
		{"", "http", ""},
	}
	for _, c := range cases {
		got := applyFallbackProxyScheme(c.in, c.fallback)
		if got != c.want {
			t.Errorf("applyFallbackProxyScheme(%q, %q) = %q, want %q", c.in, c.fallback, got, c.want)
		}
	}
}

// 混合单子应当整批可用：不能因为其中几行没前缀就整批失败。
func TestMixedPrefixListAllNormalized(t *testing.T) {
	raw := []string{
		"45.33.99.10:8080",
		"socks5://45.33.99.11:1080",
		"https://45.33.99.12:443",
		"45.33.99.13:3128",
	}
	for _, in := range raw {
		got := applyFallbackProxyScheme(in, "socks5")
		if !hasSchemePrefix(got) {
			t.Fatalf("%q 归一化后仍无协议前缀：%q", in, got)
		}
	}
}

func hasSchemePrefix(v string) bool {
	for _, p := range []string{"http://", "https://", "socks5://"} {
		if len(v) >= len(p) && v[:len(p)] == p {
			return true
		}
	}
	return false
}
