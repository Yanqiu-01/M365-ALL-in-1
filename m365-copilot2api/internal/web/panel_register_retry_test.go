package web

import "testing"

func TestNextExitAfterAdvancesAndWraps(t *testing.T) {
	pool := []string{"http://a:1", "http://b:2", "http://c:3"}
	cases := []struct{ current, want string }{
		{"http://a:1", "http://b:2"},
		{"http://b:2", "http://c:3"},
		{"http://c:3", "http://a:1"}, // 绕回开头，让重试能穿过整个列表
	}
	for _, c := range cases {
		if got := nextExitAfter(pool, c.current); got != c.want {
			t.Errorf("nextExitAfter(pool, %q) = %q, want %q", c.current, got, c.want)
		}
	}
}

// 调用方钉了一个池外的出口时，重试要能落回池子里，而不是无处可去。
func TestNextExitAfterFallsBackWhenCurrentNotInPool(t *testing.T) {
	pool := []string{"http://a:1", "http://b:2"}
	if got := nextExitAfter(pool, "http://pinned:9"); got != "http://a:1" {
		t.Fatalf("nextExitAfter = %q, want http://a:1", got)
	}
	// 池外出口恰好等于 pool[0] 时不能把同一个出口再交回去。
	if got := nextExitAfter([]string{"http://a:1", "http://b:2"}, "http://a:1"); got != "http://b:2" {
		t.Fatalf("nextExitAfter = %q, want http://b:2", got)
	}
}

// 候选不足 2 个就没有「下一个」可言：返回空串让调用方停下，而不是把同一个出口
// 再试一遍 —— 那只会把预算烧在一个已知坏出口上。
func TestNextExitAfterStopsWhenNoAlternative(t *testing.T) {
	if got := nextExitAfter(nil, "http://a:1"); got != "" {
		t.Errorf("nextExitAfter(nil) = %q, want empty", got)
	}
	if got := nextExitAfter([]string{"http://a:1"}, "http://a:1"); got != "" {
		t.Errorf("single-entry pool returned %q, want empty", got)
	}
	if got := nextExitAfter([]string{"http://a:1"}, ""); got != "http://a:1" {
		t.Errorf("empty current should take pool[0], got %q", got)
	}
}

func TestNextExitAfterTrimsWhitespace(t *testing.T) {
	if got := nextExitAfter([]string{" http://a:1 ", " http://b:2 "}, "http://a:1"); got != "http://b:2" {
		t.Fatalf("nextExitAfter = %q, want http://b:2 (trimmed)", got)
	}
}

// 重试上限要真的存在：没有它，一个站点侧的错误会把整个池子撞完，
// 每个出口都跑满一遍 Turnstile 预算。
func TestRegisterExitAttemptsIsBounded(t *testing.T) {
	if registerExitAttempts < 2 {
		t.Fatalf("registerExitAttempts = %d，至少要允许换一次出口", registerExitAttempts)
	}
	if registerExitAttempts > 10 {
		t.Fatalf("registerExitAttempts = %d，上限太高：真实错误会被埋在几十次重试后面", registerExitAttempts)
	}
}
