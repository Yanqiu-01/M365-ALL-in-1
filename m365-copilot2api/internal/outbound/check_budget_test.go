package outbound

import (
	"testing"
	"time"
)

// 这组用例源自一次实测取证：PC 上同样的节点列表只剩 3-4 个可用，而且越用越少。
//
// 根因链是：手工检查的墙钟预算写死 30s，而单轮探测需要 probeRoundBudget(10s)=35s。
// 父预算比子预算小，父超时永远先到，探测总在半途被掐断；而 CheckSelected 当时无条
// 件调用 applyProbe，于是「网关自己超时」被记成出口的真实失败。日志实测 1207 条失败
// 里 396 条是 context deadline exceeded/canceled，占 32%。consecutiveFailures 不随
// 时间衰减，攒够 guardEvictAfter=3 就永久驱逐，恢复又要 guardRestoreAfter=2 次连续
// 成功 —— 在后台巡检被关掉的机器上永远等不到，池子于是单向棘轮到底。

// 最基本的不变式：预算必须装得下至少一整轮，否则连一个出口都探不完。
func TestCheckAllBudgetFitsAtLeastOneRound(t *testing.T) {
	round := probeRoundBudget(defaultProbeTimeout)
	cases := []struct{ targets, concurrency int }{
		{1, 1}, {1, 48}, {48, 48}, {180, 48}, {0, 0}, {5, 0}, {0, 6},
	}
	for _, c := range cases {
		got := checkAllBudget(c.targets, c.concurrency)
		if got < round {
			t.Errorf("checkAllBudget(%d,%d) = %v, shorter than one probe round (%v); "+
				"a probe cut short by its own parent gets recorded against the exit",
				c.targets, c.concurrency, got, round)
		}
	}
}

// 回归钉子：旧实现是写死的 30s，比一轮 35s 还短。任何回到「常量且小于一轮」的改动
// 都要在这里失败。
func TestCheckAllBudgetIsNotTheOldFixedThirtySeconds(t *testing.T) {
	if got := checkAllBudget(180, 48); got == 30*time.Second {
		t.Fatal("budget is back to the fixed 30s that could not fit a single 35s round")
	}
	if probeRoundBudget(defaultProbeTimeout) <= 30*time.Second {
		t.Skip("probe round now fits in 30s; this regression pin no longer applies")
	}
}

// 预算随需要的轮数增长：180 个出口、并发 48，需要 4 轮，不能只给 1 轮的时间。
func TestCheckAllBudgetScalesWithWaves(t *testing.T) {
	round := probeRoundBudget(defaultProbeTimeout)
	one := checkAllBudget(48, 48)
	four := checkAllBudget(180, 48)
	if one != round {
		t.Errorf("a single wave should get exactly one round, got %v want %v", one, round)
	}
	if four <= one {
		t.Errorf("four waves (%v) must get more time than one (%v)", four, one)
	}
	if want := 4 * round; four != want && four != checkAllMaxBudget {
		t.Errorf("checkAllBudget(180,48) = %v, want %v or the cap %v", four, want, checkAllMaxBudget)
	}
}

// 上限仍要生效，管理端接口不能被池子大小拖到无限久。
func TestCheckAllBudgetHonoursTheCap(t *testing.T) {
	got := checkAllBudget(100000, 1)
	if got != checkAllMaxBudget {
		t.Errorf("checkAllBudget with a huge pool = %v, want the cap %v", got, checkAllMaxBudget)
	}
	if got > checkAllMaxBudget {
		t.Error("budget exceeded its own cap")
	}
}

// 单轮预算的构成没有变：三次串行请求加余量。若有人改小它，上面几条的前提就不成立。
func TestProbeRoundBudgetCoversThreeSequentialRequests(t *testing.T) {
	timeout := 10 * time.Second
	got := probeRoundBudget(timeout)
	if got < 3*timeout {
		t.Errorf("probeRoundBudget(%v) = %v, cannot cover three sequential %v requests",
			timeout, got, timeout)
	}
}
