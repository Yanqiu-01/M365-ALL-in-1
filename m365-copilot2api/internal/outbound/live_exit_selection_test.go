package outbound

import (
	"testing"
	"time"
)

// 用户实测：注册报 ERR_PROXY_CONNECTION_FAILED（FlareSolverr 容器里的 Chrome 连不上
// 网关递过去的代理）。
//
// 成因不在池子的数据，而在挑选：注册用 PickRawURL，它返回 bestLocked 的结果，而 tier()
// 把已驱逐和正在冷却的都归为 tierEvicted —— 没有更好选择时 bestLocked 仍会把它交出来。
// 对聊天来说那是合理的降级，对「把出口交给外部求解器」不成立：Chrome 拿到不通的代理只
// 会失败，而报错指向浏览器，看不出是出口挑错了。
//
// 池子自己知道哪些是 live（入池时 ValidateProxyCandidate 探过 L2，之后由巡检维护），
// 所以这两个接口存在的意义是「问它要 live 的」。

func poolWith(t *testing.T, entries ...*poolEntry) *Pool {
	t.Helper()
	p := &Pool{}
	p.entries = entries
	return p
}

func liveEntry(raw string, score float64) *poolEntry {
	return &poolEntry{raw: raw, score: score}
}

func evictedEntry(raw string, score float64) *poolEntry {
	e := &poolEntry{raw: raw, score: score}
	e.state = stateEvicted
	return e
}

func coolingEntry(raw string, score float64) *poolEntry {
	e := &poolEntry{raw: raw, score: score}
	e.cooldown = time.Now().Add(2 * time.Minute)
	return e
}

func withPool(t *testing.T, p *Pool) {
	t.Helper()
	clientsMu.Lock()
	prev := proxyPool
	proxyPool = p
	clientsMu.Unlock()
	t.Cleanup(func() {
		clientsMu.Lock()
		proxyPool = prev
		clientsMu.Unlock()
	})
}

// 决定性用例：池子里只有不可用的出口时，必须返回空而不是交出一个已驱逐的。
func TestPickLiveRawURLRefusesToHandOutADeadExit(t *testing.T) {
	withPool(t, poolWith(t,
		evictedEntry("http://evicted.example:8080", 0.9),
		coolingEntry("http://cooling.example:8080", 0.8),
	))
	if got := PickLiveRawURL(); got != "" {
		t.Errorf("PickLiveRawURL() = %q; a dead exit handed to FlareSolverr only yields "+
			"ERR_PROXY_CONNECTION_FAILED, so the caller must be told there is none", got)
	}
	// 对照：PickRawURL 仍然会交出来，这正是原先的问题。
	if got := PickRawURL(); got == "" {
		t.Error("PickRawURL is expected to still degrade to a dead exit; that contrast is the point")
	}
}

func TestPickLiveRawURLPrefersLiveOverHigherScoredDead(t *testing.T) {
	withPool(t, poolWith(t,
		evictedEntry("http://dead-but-high.example:8080", 0.99),
		liveEntry("http://live-but-low.example:8080", 0.10),
	))
	if got := PickLiveRawURL(); got != "http://live-but-low.example:8080" {
		t.Errorf("PickLiveRawURL() = %q, want the live exit even though a dead one scores higher", got)
	}
}

func TestLiveProxyPoolRawURLsExcludesDeadAndOrdersByScore(t *testing.T) {
	withPool(t, poolWith(t,
		liveEntry("http://low.example:8080", 0.2),
		evictedEntry("http://evicted.example:8080", 0.95),
		liveEntry("http://high.example:8080", 0.8),
		coolingEntry("http://cooling.example:8080", 0.9),
	))
	got := LiveProxyPoolRawURLs()
	if len(got) != 2 {
		t.Fatalf("got %d live exits %v, want 2 (the evicted and cooling ones must be excluded)", len(got), got)
	}
	if got[0] != "http://high.example:8080" || got[1] != "http://low.example:8080" {
		t.Errorf("order = %v, want best score first so rotation starts with the best exit", got)
	}
}

// 空池和 nil 池都不得 panic，调用方靠返回空来决定回落。
func TestLiveSelectionHandlesAnEmptyPool(t *testing.T) {
	withPool(t, poolWith(t))
	if got := PickLiveRawURL(); got != "" {
		t.Errorf("empty pool returned %q", got)
	}
	if got := LiveProxyPoolRawURLs(); len(got) != 0 {
		t.Errorf("empty pool returned %v", got)
	}
	withPool(t, nil)
	if got := PickLiveRawURL(); got != "" {
		t.Errorf("nil pool returned %q", got)
	}
	if got := LiveProxyPoolRawURLs(); len(got) != 0 {
		t.Errorf("nil pool returned %v", got)
	}
}
