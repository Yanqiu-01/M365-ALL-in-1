package web

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// saturateLoginAttempts 用 maxLoginAttemptEntries 条「活的」记录填满登录尝试表：
// 窗口未衰减、锁定未过期，所以清扫这一轮什么也回收不了。
func saturateLoginAttempts(s *Server, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < maxLoginAttemptEntries; i++ {
		ip := fmt.Sprintf("198.51.%d.%d", i/256, i%256)
		s.loginAttempts[ip] = loginAttempt{
			Failures:    1,
			WindowStart: now,
			LockedUntil: now.Add(loginLockoutWindow),
		}
	}
}

// 表被填满时 recordLoginFailure 以前 fail open：对没有记录的新来源返回
// (false, 0)，也就是「未锁定」，于是锁定策略对所有新来源彻底失效。这是可被
// 刻意触发的 —— 攻击者先用 maxLoginAttemptEntries 个地址各失败一次（回环反代
// 后面还可以直接伪造 X-Forwarded-For），再从第 4097 个地址开始无限次爆破。
func TestRecordLoginFailureFailsClosedWhenTableIsSaturated(t *testing.T) {
	s := lockoutTestServer("correct-password")
	now := time.Now()
	saturateLoginAttempts(s, now)

	policy := loginPolicyFor(false)
	// 连续多次：每一次都必须被判为锁定，而不是白送一次尝试机会。
	for i := 1; i <= 50; i++ {
		locked, wait := s.recordLoginFailure("203.0.113.250", now, policy)
		if !locked {
			t.Fatalf("attempt %d from an untracked source was not locked; the lockout stopped applying once the table saturated", i)
		}
		if wait <= 0 {
			t.Fatalf("attempt %d returned a non-positive wait %v", i, wait)
		}
	}
}

// 端到端：表满时错误口令必须收到 429（带 Retry-After），而不是 401 后无限重试。
func TestSaturatedLoginTableStillRateLimitsNewSources(t *testing.T) {
	s := lockoutTestServer("correct-password")
	saturateLoginAttempts(s, time.Now())

	for i := 1; i <= 10; i++ {
		rec := tryAdminLogin(t, s, "203.0.113.251:44100", "wrong")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=%d want 429 while the attempt table is saturated", i, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("attempt %d: missing Retry-After", i)
		}
	}
}

// 失败关闭不能把合法管理员挡在外面：adminLogin 先校验口令，正确口令在表满时
// 依然能登录成功。这是这个方向安全的前提，必须钉住。
func TestCorrectPasswordStillSucceedsWhenLoginTableIsSaturated(t *testing.T) {
	s := lockoutTestServer("correct-password")
	saturateLoginAttempts(s, time.Now())

	rec := tryAdminLogin(t, s, "203.0.113.252:44200", "correct-password")
	if rec.Code != http.StatusOK {
		t.Fatalf("correct password rejected while the table was saturated: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 表里存在可回收的陈旧记录时，清扫必须先腾出空间，让正常的计数继续走
// —— 失败关闭只适用于「真的腾不出空间」的情形。
func TestRecordLoginFailureReclaimsDecayedEntries(t *testing.T) {
	s := lockoutTestServer("correct-password")
	now := time.Now()
	// 填满，但全部是早已衰减的记录：清扫应当把它们全部回收。
	s.mu.Lock()
	for i := 0; i < maxLoginAttemptEntries; i++ {
		ip := fmt.Sprintf("198.51.%d.%d", i/256, i%256)
		s.loginAttempts[ip] = loginAttempt{
			Failures:    1,
			WindowStart: now.Add(-2 * loginLockoutWindow),
			LockedUntil: now.Add(-loginLockoutWindow),
		}
	}
	s.mu.Unlock()

	policy := loginPolicyFor(false)
	locked, _ := s.recordLoginFailure("203.0.113.253", now, policy)
	if locked {
		t.Fatal("first failure from a new source was locked even though stale entries were reclaimable")
	}
	// 记录确实被登记下来了，计数照常推进。
	if got := s.attemptFor("203.0.113.253"); got.Failures != 1 {
		t.Fatalf("failure was not recorded after the sweep: %+v", got)
	}
}
