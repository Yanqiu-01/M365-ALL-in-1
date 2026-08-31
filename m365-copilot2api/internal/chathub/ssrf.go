package chathub

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxAttachmentRedirects 限制附件下载允许的重定向跳数。Go 默认是 10，这里收紧：
// 合法的图片 CDN 极少需要超过三跳，而每一跳都是一次绕过校验的机会。
const maxAttachmentRedirects = 3

// safeDownloadClient 返回一个会对每一跳重新做 SSRF 校验的客户端。
//
// 为什么必须逐跳校验：validateRemoteDownloadURL 只看调用方给的那个 URL，而
// http.Client 默认跟随重定向。于是一个通过校验的公网地址可以 302 到
// http://169.254.169.254/（云元数据）或 127.0.0.1，请求照样发出去 —— 首次校验被完整
// 绕过。CheckRedirect 是唯一能看到中间跳的钩子。
//
// 这里对传入的客户端做浅拷贝而不是原地修改：它是全局共享的出站客户端，改它会影响所有
// 其它调用方。Transport 仍然复用，所以连接池和代理配置不变。
func safeDownloadClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxAttachmentRedirects {
			return fmt.Errorf("attachment download exceeded %d redirects", maxAttachmentRedirects)
		}
		return validateRemoteDownloadURL(req.URL.String())
	}
	return c
}

// validateRemoteDownloadURL blocks SSRF: only https and public routable
// addresses are accepted, with a lookup-time recheck against private,
// loopback, link-local and cloud metadata ranges.
//
// 单独调用它只覆盖一个 URL。跟随重定向的下载必须走 safeDownloadClient，否则中间跳
// 不受任何检查。
func validateRemoteDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid attachment URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("attachment download requires https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("attachment URL has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("attachment host does not resolve")
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return nil
}

func ipUnsafe(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 link-local is covered above on Go >= 1.17;
		// 100.64.0.0/10 (CGNAT) is not private per IP.IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	// 169.254.169.254 cloud metadata is link-local; belt and braces.
	if strings.HasPrefix(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}
