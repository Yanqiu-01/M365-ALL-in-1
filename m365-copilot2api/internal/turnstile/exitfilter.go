package turnstile

import (
	"net"
	"net/url"
	"strings"
)

// 两个求解后端能用的出口不是同一批，这是实测出来的差别，不是猜的：
//
//   - Chrome 跑在宿主机上，回环代理（clash 的 127.0.0.1:7897）和 socks5 都能用。
//   - FlareSolverr 跑在 Docker 容器里。容器里的 127.0.0.1 是容器自己，宿主的 clash
//     根本不在那里 —— 递给它只会得到 ERR_PROXY_CONNECTION_FAILED，也就是用户反复贴
//     的那条报错。同一批出口用 curl 从容器内测：http 出口回 302，socks5 出口回 000
//     （socks5h 也是 000，所以不是 DNS 的事）。
//
// 于是把「谁能用什么出口」写成一个函数，而不是让 panel_register 那边按经验去猜。

// ExitUsableByFlareSolverr 报告一个出口能否交给容器里的 FlareSolverr。
func ExitUsableByFlareSolverr(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return false
	}
	return !isLoopbackHost(parsed.Hostname())
}

// ExitUsableByChrome 报告一个出口能否交给本机 Chrome。
//
// 本机浏览器能连回环，也认 socks5；空串表示不带代理，那是直连 —— 用户明确要求注册
// 不能走直连，所以这里同样判否，由调用方去拿一个真出口。
func ExitUsableByChrome(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return true
	default:
		return false
	}
}

// FilterExits 按给定判据筛一份出口列表，保持原有顺序（池子已按 tier + score 排好）。
func FilterExits(raws []string, usable func(string) bool) []string {
	if usable == nil {
		return nil
	}
	out := make([]string, 0, len(raws))
	seen := make(map[string]struct{}, len(raws))
	for _, raw := range raws {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || !usable(trimmed) {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	switch host {
	case "", "localhost", "localhost.localdomain":
		return host != ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return strings.HasSuffix(host, ".localhost")
}
