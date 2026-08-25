package outbound

import (
	"testing"
	"time"
)

// 冷却中的出口 state 仍然是 live，但选路时它已经是 evicted。面板必须拿到
// tier 才能把它排到后面 —— 按 state 分组会把一个刚刚断掉的出口摆在健康列表最前。
func TestListExposesSelectionTierNotJustState(t *testing.T) {
	now := time.Now()
	e := &poolEntry{raw: "http://198.51.100.7:3128", cooldown: now.Add(2 * time.Minute)}
	if e.stateName() != stateLive {
		t.Fatalf("precondition: state = %s, want live", e.stateName())
	}
	if got := e.tier(now); got != tierEvicted {
		t.Fatalf("tier = %d, want tierEvicted (%d)", got, tierEvicted)
	}
	if name := tierName(e.tier(now)); name != "evicted" {
		t.Fatalf("tierName = %q, want evicted", name)
	}
	if ms := cooldownRemainingMs(e.cooldown, now); ms <= 0 {
		t.Fatalf("cooldownRemainingMs = %d, want > 0", ms)
	}
}

func TestCooldownRemainingZeroWhenNotCooling(t *testing.T) {
	now := time.Now()
	if ms := cooldownRemainingMs(time.Time{}, now); ms != 0 {
		t.Fatalf("zero cooldown -> %d, want 0", ms)
	}
	if ms := cooldownRemainingMs(now.Add(-time.Minute), now); ms != 0 {
		t.Fatalf("expired cooldown -> %d, want 0", ms)
	}
}

// 超时熔断和「秒拒」是两种相反的故障：拒绝会一直拒绝，超时可能只是慢。
func TestRefusalDistinguishedFromTimeout(t *testing.T) {
	refusals := []string{
		"dial tcp 1.2.3.4:3128: connect: connection refused",
		"proxy CONNECT substrate.office.com:443: 407 Proxy Authentication Required",
		"read: connection reset by peer",
	}
	for _, e := range refusals {
		if !isRefusalError(e) {
			t.Fatalf("isRefusalError(%q) = false, want true", e)
		}
	}
	timeouts := []string{
		"context deadline exceeded",
		"dial tcp 1.2.3.4:3128: i/o timeout",
		"",
	}
	for _, e := range timeouts {
		if isRefusalError(e) {
			t.Fatalf("isRefusalError(%q) = true, want false", e)
		}
	}
}

// List 必须把这些字段真的放进 map，否则面板拿不到。
func TestListPayloadCarriesTierFields(t *testing.T) {
	pool, err := NewPool([]string{"http://198.51.100.8:3128"})
	if err != nil {
		t.Fatal(err)
	}
	items := pool.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	for _, key := range []string{"tier", "tierName", "cooldownRemainingMs", "refused"} {
		if _, ok := items[0][key]; !ok {
			t.Fatalf("List() is missing %q; the dashboard cannot group without it", key)
		}
	}
}
