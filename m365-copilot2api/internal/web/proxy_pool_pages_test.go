package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 快代理每页只有 12 行，只抓首页最多就 12 条。用户想要几十上百个候选，
// 必须翻页 —— 这是「怎么只能拉 12 个」的直接原因。
func TestFreeProxyPageURLPathPagination(t *testing.T) {
	cases := []struct {
		raw  string
		page int
		want string
	}{
		{"https://www.kuaidaili.com/free/inha/", 2, "https://www.kuaidaili.com/free/inha/2/"},
		{"https://www.kuaidaili.com/free/inha/", 7, "https://www.kuaidaili.com/free/inha/7/"},
		{"https://www.kuaidaili.com/free/inha", 3, "https://www.kuaidaili.com/free/inha/3"},
		// 末段已经是页号时应替换而不是继续追加。
		{"https://www.kuaidaili.com/free/inha/2/", 5, "https://www.kuaidaili.com/free/inha/5/"},
		// 查询参数分页。
		{"https://example.com/list?page=1", 4, "https://example.com/list?page=4"},
		{"https://example.com/list?pn=1&x=2", 3, "https://example.com/list?pn=3&x=2"},
		// 第 1 页永远原样返回。
		{"https://www.kuaidaili.com/free/inha/", 1, "https://www.kuaidaili.com/free/inha/"},
	}
	for _, c := range cases {
		got, ok := freeProxyPageURL(c.raw, c.page)
		if !ok {
			t.Fatalf("freeProxyPageURL(%q,%d) not ok", c.raw, c.page)
		}
		if got != c.want {
			t.Fatalf("freeProxyPageURL(%q,%d) = %q, want %q", c.raw, c.page, got, c.want)
		}
	}
}

func TestNormalizeFreeProxyPagesClamps(t *testing.T) {
	if got := normalizeFreeProxyPages(0); got != 1 {
		t.Fatalf("pages 0 -> %d, want 1 (旧行为)", got)
	}
	if got := normalizeFreeProxyPages(-5); got != 1 {
		t.Fatalf("pages -5 -> %d, want 1", got)
	}
	if got := normalizeFreeProxyPages(freeProxyMaxPages + 50); got != freeProxyMaxPages {
		t.Fatalf("pages over cap -> %d, want %d", got, freeProxyMaxPages)
	}
}

// 翻页抓取要把每页正文都拼进来，最终候选数应该随页数增长。
func TestFetchFreeProxyPagesAggregatesEveryPage(t *testing.T) {
	fastFreeProxyPages(t)
	var mu sync.Mutex
	var seen []string
	setFreeProxyTestSeams(t, func(ctx context.Context, target string) ([]byte, error) {
		mu.Lock()
		seen = append(seen, target)
		mu.Unlock()
		// 每页 2 条，页号决定网段，便于确认真的抓了不同页。
		page := 1
		if n := strings.TrimSuffix(strings.TrimPrefix(target, "https://src.example/list/"), "/"); n != "" {
			fmt.Sscanf(n, "%d", &page)
		}
		body := fmt.Sprintf("45.33.%d.10:8080\n45.33.%d.11:8081\n", page*2, page*2+1)
		return []byte(body), nil
	}, nil)

	payload, fetched, err := fetchFreeProxyPages(context.Background(), "https://src.example/list/", 5)
	if err != nil {
		t.Fatal(err)
	}
	if fetched != 5 {
		t.Fatalf("fetched = %d, want 5", fetched)
	}
	if len(seen) != 5 {
		t.Fatalf("fetch calls = %d, want 5: %v", len(seen), seen)
	}
	parsed, _ := parseFreeProxyCandidates(payload, "http")
	if len(parsed) != 10 {
		t.Fatalf("candidates = %d, want 10 (2 per page x 5 pages)", len(parsed))
	}
}

// 单页失败不该丢掉其它页已抓到的结果。
func TestFetchFreeProxyPagesToleratesOnePageFailure(t *testing.T) {
	fastFreeProxyPages(t)
	setFreeProxyTestSeams(t, func(ctx context.Context, target string) ([]byte, error) {
		if strings.Contains(target, "/3/") {
			return nil, errors.New("HTTP 503")
		}
		return []byte("45.33.40.20:8080\n"), nil
	}, nil)

	_, fetched, err := fetchFreeProxyPages(context.Background(), "https://src.example/list/", 4)
	if err != nil {
		t.Fatalf("one bad page must not fail the whole fetch: %v", err)
	}
	if fetched != 3 {
		t.Fatalf("fetched = %d, want 3 (4 pages, 1 failed)", fetched)
	}
}

// 所有页都失败时才报错。
func TestFetchFreeProxyPagesFailsWhenNoPageWorks(t *testing.T) {
	fastFreeProxyPages(t)
	setFreeProxyTestSeams(t, func(ctx context.Context, target string) ([]byte, error) {
		return nil, errors.New("unreachable")
	}, nil)
	if _, _, err := fetchFreeProxyPages(context.Background(), "https://src.example/list/", 3); err == nil {
		t.Fatal("expected an error when every page fails")
	}
}

// pages<=1 必须保持旧行为：只请求给定 URL，一次。
func TestFetchFreeProxySinglePageUnchanged(t *testing.T) {
	calls := 0
	setFreeProxyTestSeams(t, func(ctx context.Context, target string) ([]byte, error) {
		calls++
		if target != "https://src.example/list/" {
			t.Fatalf("unexpected target %q", target)
		}
		return []byte("45.33.40.30:8080\n"), nil
	}, nil)
	if _, fetched, err := fetchFreeProxyPages(context.Background(), "https://src.example/list/", 1); err != nil || fetched != 1 {
		t.Fatalf("fetched=%d err=%v", fetched, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// fastFreeProxyPages 把页间间隔与重试退避调成 0。
// 不这么做的话，一个「抓 5 页」的测试要真等 10 秒 —— 生产值是实测得出的
// 反节流间隔（见 proxy_pool_sources.go），但测试不该为它付时间。
func fastFreeProxyPages(t *testing.T) {
	t.Helper()
	oldDelay, oldRetry := freeProxyPageDelay, freeProxyPageRetryDelay
	freeProxyPageDelay, freeProxyPageRetryDelay = 0, 0
	t.Cleanup(func() {
		freeProxyPageDelay, freeProxyPageRetryDelay = oldDelay, oldRetry
	})
}
