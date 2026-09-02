package web

import (
	"fmt"
	"strings"
	"testing"
)

// 出口连不通和上游故障是两件事，客户端消息里得分得开。
//
// 2026-09-02 的 m365-gateway.log 里同一个失败类出现过三种文本，其中裸 "Bad Gateway"
// 谁都不匹配，于是 upstreamError（errors.go:105）只能回落到无信息量的
// "upstream request failed" —— 可操作的细节只留在服务端日志里，客户端看不到。
func TestClassifyUpstreamNamesTunnelFailures(t *testing.T) {
	const want = "outbound exit failed to establish a tunnel"
	for _, err := range []error{
		// 线上原文（req b291e07b，17:44:45）：Do 返回代理的 reason phrase。
		fmt.Errorf(`Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": Bad Gateway`),
		fmt.Errorf(`Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": proxyconnect tcp: dial tcp 123.121.123.23:8888: i/o timeout`),
		fmt.Errorf(`Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": socks connect tcp 103.210.161.8:1080: unknown error`),
	} {
		if got := classifyUpstream(err); got != want {
			t.Errorf("classifyUpstream(%q)\n  = %q\n want %q", err, got, want)
		}
	}
}

// 真正来自上游的 502 不能被认成隧道问题。UpstreamHTTPError 的文本是
// "upstream http 502"（account_health.go:23），和裸 "bad gateway" 文本上就不同。
func TestClassifyUpstreamDoesNotMistakeARealUpstream502ForATunnelFailure(t *testing.T) {
	if got := classifyUpstream(&UpstreamHTTPError{Status: 502}); got == "outbound exit failed to establish a tunnel" {
		t.Errorf("上游自己返的 502 被归成隧道故障，会把运维方向指错: %q", got)
	}
}

// 隧道分类必须排在 403/401 那条之前：代理拒 CONNECT 时可能就是回 403，
// 落到后一条会被说成「上游拒绝了请求」。
func TestTunnelClassificationWinsOverTheGenericRejectionCase(t *testing.T) {
	err := fmt.Errorf(`Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": proxyconnect tcp: 403 Forbidden`)
	got := classifyUpstream(err)
	if got == "upstream rejected the request" {
		t.Error("代理拒 CONNECT 被归成上游拒绝请求；隧道那条应当先匹配")
	}
	if got != "outbound exit failed to establish a tunnel" {
		t.Errorf("classifyUpstream = %q，want 隧道分类", got)
	}
}

// 分类只回固定串，不能把代理地址带给客户端。完整错误进服务端日志
// （errors.go:109）就够运维用了。
func TestTunnelClassificationLeaksNoExitAddress(t *testing.T) {
	err := fmt.Errorf(`Post "https://login.microsoftonline.com/common/oauth2/v2.0/token": socks connect tcp 103.210.161.8:1080: unknown error`)
	msg := upstreamError(err)
	for _, leak := range []string{"103.210.161.8", "1080", "socks"} {
		if strings.Contains(msg, leak) {
			t.Errorf("客户端可见消息里出现了 %q: %q", leak, msg)
		}
	}
}
