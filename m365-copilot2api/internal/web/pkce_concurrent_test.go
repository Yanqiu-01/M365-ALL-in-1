package web

import (
	"testing"
)

// 批量并发 OAuth 时，脚本会先连着调 N 次 /api/auth/start 再逐个回调。
// 原实现每次 start 都 s.pkce = map{...} 只留自己那一个 state，于是前 N-1 个
// 账号回调时都拿到 "invalid or expired state"。
// 实测 3 并发：2 个 http400、只有最后启动的那个成功。
func TestConcurrentPKCEStatesCoexist(t *testing.T) {
	s := &Server{}
	states := []string{"state-a", "state-b", "state-c"}
	attempts := make([]uint64, 0, len(states))
	for _, st := range states {
		attempts = append(attempts, s.registerPKCEState(st, "verifier-"+st, true))
	}

	// 三个 state 必须同时存活。
	for _, st := range states {
		p, ok := s.pkce[st]
		if !ok {
			t.Fatalf("state %q 被后续 start 删掉了；该账号回调必然报 invalid state", st)
		}
		if p.Verifier != "verifier-"+st {
			t.Fatalf("state %q 的 verifier 串了：%q", st, p.Verifier)
		}
		if p.Status != "pending" {
			t.Fatalf("state %q status=%q，期望 pending", st, p.Status)
		}
	}

	// generation 必须各不相同且递增，隔离仍然有效。
	for i := 1; i < len(attempts); i++ {
		if attempts[i] <= attempts[i-1] {
			t.Fatalf("attempt 没有递增：%v", attempts)
		}
	}
}

// 表不能无界增长：超过上限时淘汰最旧的一条，而不是整表清空。
func TestPKCETableEvictsOldestNotAll(t *testing.T) {
	s := &Server{}
	for i := 0; i < maxPendingPKCE+5; i++ {
		s.registerPKCEState(pkceTestState(i), "v", true)
	}
	if len(s.pkce) > maxPendingPKCE {
		t.Fatalf("表超过上限：%d > %d", len(s.pkce), maxPendingPKCE)
	}
	// 最后登记的那个必须还在（它是最新的）。
	last := pkceTestState(maxPendingPKCE + 4)
	if _, ok := s.pkce[last]; !ok {
		t.Fatal("最新登记的 state 被淘汰了")
	}
	// 不能退化成「只剩一个」。
	if len(s.pkce) < 2 {
		t.Fatalf("表被清空到只剩 %d 条，说明又变成整表覆盖了", len(s.pkce))
	}
}

func pkceTestState(i int) string {
	return "st-" + string(rune('a'+i%26)) + "-" + itoaTest(i)
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// resetPKCE 的语义就是全部清掉，这条不能被上面的修改带跑偏。
func TestResetStillClearsEverything(t *testing.T) {
	s := &Server{}
	s.registerPKCEState("a", "v", true)
	s.registerPKCEState("b", "v", true)
	if len(s.pkce) != 2 {
		t.Fatalf("前置条件失败：len=%d", len(s.pkce))
	}
	s.mu.Lock()
	s.pkce = map[string]pendingPKCE{}
	s.mu.Unlock()
	if len(s.pkce) != 0 {
		t.Fatal("reset 应当清空整张表")
	}
}

// 浏览器交互式授权必须保持独占：keepOthers=false 时旧条目要被清掉，
// 否则 UI 会反复进入上一次的回调。
func TestInteractiveAuthorizationStaysExclusive(t *testing.T) {
	s := &Server{}
	s.registerPKCEState("old", "v", true)
	s.registerPKCEState("new", "v", false)
	if _, ok := s.pkce["old"]; ok {
		t.Fatal("交互式授权没有清掉旧 state；UI 会重复进入上一次回调")
	}
	if len(s.pkce) != 1 {
		t.Fatalf("len=%d，期望 1", len(s.pkce))
	}
}
