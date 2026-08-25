package outbound

import (
	"context"
	"testing"
)

// 新增出口后前端过去调全量检查，于是每加一个 IP 都把池里老 IP 重新探一遍：
// 慢、而且 30s 预算被老出口占满，新出口反而没状态。
// CheckSelected 只探指定 id。
func TestCheckSelectedProbesOnlyRequestedExits(t *testing.T) {
	pool, err := NewPool([]string{
		"http://45.33.90.1:8080",
		"http://45.33.90.2:8080",
		"http://45.33.90.3:8080",
	})
	if err != nil {
		t.Fatal(err)
	}

	items := pool.CheckSelected(context.Background(), []string{})
	if len(items) != 3 {
		t.Fatalf("空 ids 应退化为全量，返回 %d 条", len(items))
	}
}

// ids 只命中一个时，返回的列表也只包含那一个 —— 前端据此立刻显示新出口状态。
func TestCheckSelectedReturnsOnlyMatchedEntry(t *testing.T) {
	pool, err := NewPool([]string{
		"http://45.33.91.1:8080",
		"http://45.33.91.2:8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	all := pool.List()
	if len(all) != 2 {
		t.Fatalf("len=%d", len(all))
	}
	targetID, _ := all[1]["id"].(string)
	if targetID == "" {
		t.Fatal("id 缺失")
	}
	got := pool.CheckSelected(context.Background(), []string{targetID})
	if len(got) != 1 {
		t.Fatalf("定向检查返回 %d 条，期望 1", len(got))
	}
	if id, _ := got[0]["id"].(string); id != targetID {
		t.Fatalf("返回的是 %q，期望 %q", id, targetID)
	}
}

// ProxyIDsForRawURLs 必须能把刚入池的原始 URL 换成 id。
func TestProxyIDsForRawURLs(t *testing.T) {
	ids := ProxyIDsForRawURLs([]string{"http://45.33.92.1:8080", "socks5://45.33.92.2:1080"})
	if len(ids) != 2 {
		t.Fatalf("len=%d, want 2", len(ids))
	}
	for _, id := range ids {
		if id == "" {
			t.Fatal("空 id")
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("不同出口不该得到相同 id")
	}
}
