package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/outbound"
)

// 用户报告：「添加 IP 的时候，一个节点不过的时候整条都被拒绝」。
// 原实现在第一个校验失败处直接 return，整批候选全部丢弃。
func TestManualAddAcceptsGoodExitsWhenOneFails(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)

	setFreeProxyTestSeams(t, nil, func(ctx context.Context, candidate string) error {
		if strings.Contains(candidate, "203.0.113.9") {
			return errors.New("connect: connection refused")
		}
		return nil
	})

	body := `{"urls":["http://198.51.100.1:3128","http://203.0.113.9:3128","http://198.51.100.2:3128"]}`
	r := httptest.NewRequest("POST", "/api/admin/proxy-pool", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.addManualProxies(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; one bad exit must not reject the batch. body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Added    int `json:"added"`
		Accepted int `json:"accepted"`
		Rejected []struct {
			Proxy  string `json:"proxy"`
			Reason string `json:"reason"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Added != 2 || out.Accepted != 2 {
		t.Fatalf("added=%d accepted=%d, want 2/2", out.Added, out.Accepted)
	}
	if len(out.Rejected) != 1 {
		t.Fatalf("rejected = %d, want exactly 1", len(out.Rejected))
	}
	if !strings.Contains(out.Rejected[0].Reason, "refused") {
		t.Fatalf("rejection reason not surfaced: %q", out.Rejected[0].Reason)
	}
	// 被拒的那条要能定位，但不能回显凭据。
	if !strings.Contains(out.Rejected[0].Proxy, "203.0.113.9") {
		t.Fatalf("rejected entry is not identifiable: %q", out.Rejected[0].Proxy)
	}

	urls := outbound.ProxyPoolRawURLs()
	if len(urls) != 2 {
		t.Fatalf("pool has %d exits, want 2: %v", len(urls), urls)
	}
	for _, u := range urls {
		if strings.Contains(u, "203.0.113.9") {
			t.Fatal("a rejected exit was persisted")
		}
	}
}

// 全部不通时仍应报 400：静默返回 200 会让用户以为添加成功了。
func TestManualAddStillFailsWhenEveryExitIsBad(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)
	setFreeProxyTestSeams(t, nil, func(ctx context.Context, candidate string) error {
		return errors.New("i/o timeout")
	})

	body := `{"urls":["http://198.51.100.3:3128","http://198.51.100.4:3128"]}`
	r := httptest.NewRequest("POST", "/api/admin/proxy-pool", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.addManualProxies(w, r)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 when nothing passes", w.Code)
	}
	if len(outbound.ProxyPoolRawURLs()) != 0 {
		t.Fatal("nothing should have been persisted")
	}
}

// skipCheck 仍然是那条明确的逃生门，不做探测。
func TestManualAddSkipCheckBypassesProbe(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	s := proxyPoolFeatureTestServer(t)
	probed := 0
	setFreeProxyTestSeams(t, nil, func(ctx context.Context, candidate string) error {
		probed++
		return errors.New("should not be called")
	})

	body := `{"urls":["http://198.51.100.5:3128"],"skipCheck":true}`
	r := httptest.NewRequest("POST", "/api/admin/proxy-pool", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.addManualProxies(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if probed != 0 {
		t.Fatalf("probe ran %d times despite skipCheck", probed)
	}
}
