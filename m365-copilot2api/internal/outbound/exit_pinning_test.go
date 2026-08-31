package outbound

import (
	"testing"
)

// 批量授权需要按自己的节奏轮换出口。HTTPClient() 返回的是池子当前偏好的那一个，而 pick()
// 是确定性的 —— 连续调用拿到同一个出口。几百个账号的 ROPC 全部从同一个 IP 发出，是最容易
// 被上游判成异常的形态。HTTPClientForExit 让调用方按出口取客户端。

func TestHTTPClientForExitReturnsDistinctClientsPerExit(t *testing.T) {
	a := HTTPClientForExit("socks5://127.0.0.1:1080")
	b := HTTPClientForExit("socks5://127.0.0.1:1081")
	if a == nil || b == nil {
		t.Fatal("HTTPClientForExit must never return nil")
	}
	if a == b {
		t.Error("two different exits produced the same client; rotation would be a no-op")
	}
	if a.Transport == nil || b.Transport == nil {
		t.Error("a pinned client must carry its own transport, otherwise it does not route through the exit")
	}
}

// 空出口退回默认选择，这样单账号路径与轮换路径共用一份代码而行为不变。
func TestHTTPClientForExitEmptyFallsBackToDefault(t *testing.T) {
	if got := HTTPClientForExit(""); got == nil {
		t.Fatal("an empty exit must fall back to the pool default, not nil")
	}
	if got := HTTPClientForExit("   "); got == nil {
		t.Fatal("a whitespace-only exit must fall back to the pool default, not nil")
	}
}

// 出口在轮换途中被驱逐或本身不合法时，必须退回默认而不是让整批中断。
func TestHTTPClientForExitUnparseableFallsBackRatherThanFailing(t *testing.T) {
	for _, raw := range []string{
		"not-a-url",
		"://missing-scheme",
		"socks5://",
		"gopher://unsupported:70",
	} {
		if got := HTTPClientForExit(raw); got == nil {
			t.Errorf("HTTPClientForExit(%q) returned nil; a bad exit must degrade to the default "+
				"so the remaining accounts still get processed", raw)
		}
	}
}

// 同一个出口连续取两次要拿到等价客户端，否则每个账号都会新建连接池，白费握手。
func TestHTTPClientForExitIsStableForTheSameExit(t *testing.T) {
	const exit = "socks5://127.0.0.1:1080"
	first := HTTPClientForExit(exit)
	second := HTTPClientForExit(exit)
	if first == nil || second == nil {
		t.Fatal("nil client")
	}
	if first.Timeout != second.Timeout {
		t.Errorf("same exit produced different timeouts: %v vs %v", first.Timeout, second.Timeout)
	}
}
