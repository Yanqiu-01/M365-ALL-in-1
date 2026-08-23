package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lockoutTestServer 构造一个只带登录所需字段的最小 Server，
// 不经过 New()，因此不读环境变量、不触碰真实配置目录。
func lockoutTestServer(password string) *Server {
	return &Server{
		adminPassword: password,
		adminSessions: map[string]time.Time{},
		loginAttempts: map[string]loginAttempt{},
	}
}

// tryAdminLogin 直接驱动 adminLogin，remoteAddr 可控，
// 从而能同时覆盖回环与非回环两种来源。
func tryAdminLogin(t *testing.T, s *Server, remoteAddr, password string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(string(payload)))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.adminLogin(rec, req)
	return rec
}

func (s *Server) attemptFor(ip string) loginAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginAttempts[ip]
}

// 证明 (a)：IP 处于锁定期时，正确口令仍然能登录成功，并且成功后锁定被清除。
// 这正是原缺陷的核心 —— 跑飞的本机客户端把计数打满后，合法管理员在
// 口令校验之前就被 429 挡掉，永远进不来。
func TestCorrectPasswordSucceedsWhileIPLockedAndClearsLockout(t *testing.T) {
	for _, tc := range []struct {
		name, remoteAddr, ip string
		lockFor              time.Duration
	}{
		{"loopback", "127.0.0.1:51500", "127.0.0.1", 2 * time.Minute},
		{"remote", "203.0.113.7:41000", "203.0.113.7", 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := lockoutTestServer("correct-password")
			now := time.Now()
			// 模拟已经被打满的锁定状态（失败次数远超阈值）。
			s.loginAttempts[tc.ip] = loginAttempt{
				Failures:    640,
				WindowStart: now.Add(-time.Minute),
				LockedUntil: now.Add(tc.lockFor),
			}

			rec := tryAdminLogin(t, s, tc.remoteAddr, "correct-password")
			if rec.Code != http.StatusOK {
				t.Fatalf("correct password during lockout: status=%d body=%s", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["status"] != "authenticated" {
				t.Fatalf("login payload=%#v", out)
			}
			if !strings.Contains(rec.Header().Get("Set-Cookie"), "m365_admin_session=") {
				t.Fatalf("no session cookie issued: %q", rec.Header().Get("Set-Cookie"))
			}
			// 一次成功登录立即清零该来源的失败预算。
			if got := s.attemptFor(tc.ip); got.Failures != 0 || !got.LockedUntil.IsZero() {
				t.Fatalf("lockout not cleared after success: %+v", got)
			}
			// 清零之后紧接着的错误口令回到 401，说明预算真的重置了。
			if rec := tryAdminLogin(t, s, tc.remoteAddr, "wrong"); rec.Code != http.StatusUnauthorized {
				t.Fatalf("post-success wrong password: status=%d", rec.Code)
			}
		})
	}
}

// 证明 (b)：锁定期内错误口令仍被 429 拒绝，且持续失败既不重置也不延长锁定窗口。
// 窗口不被延长是关键：原实现每次失败都把 LockedUntil 推后，
// 每 2-3 秒重试一次的客户端因此制造出永久锁定。
func TestLockedIPRejectsWrongPasswordWithoutExtendingWindow(t *testing.T) {
	s := lockoutTestServer("correct-password")
	now := time.Now()
	deadline := now.Add(90 * time.Second)
	s.loginAttempts["127.0.0.1"] = loginAttempt{
		Failures:    20,
		WindowStart: now,
		LockedUntil: deadline,
	}

	for i := 0; i < 200; i++ {
		rec := tryAdminLogin(t, s, "127.0.0.1:51501", "wrong")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d during lockout: status=%d, want 429", i+1, rec.Code)
		}
		// 锁定期内必须是 429 而不是 401：401 会暴露"这次只是口令错了"。
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("attempt %d: missing Retry-After", i+1)
		}
	}

	got := s.attemptFor("127.0.0.1")
	if !got.LockedUntil.Equal(deadline) {
		t.Fatalf("lockout deadline moved: got=%v want=%v", got.LockedUntil, deadline)
	}
	if got.Failures <= 20 {
		t.Fatalf("failures not counted during lockout: %d", got.Failures)
	}
	// 窗口到期后自然衰减，不需要外部干预。
	s.mu.Lock()
	s.loginAttempts["127.0.0.1"] = loginAttempt{
		Failures:    got.Failures,
		WindowStart: now.Add(-30 * time.Minute),
		LockedUntil: now.Add(-time.Second),
	}
	s.mu.Unlock()
	if rec := tryAdminLogin(t, s, "127.0.0.1:51501", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after window expiry, wrong password status=%d, want 401", rec.Code)
	}
}

// 证明 (c)：回环与非回环使用不同阈值/锁定时长，且回环同样存在上限（不是豁免）。
func TestLoopbackAndRemoteLockoutThresholdsDiffer(t *testing.T) {
	if loopbackLoginFailureThreshold <= loginFailureThreshold {
		t.Fatalf("loopback threshold=%d must be looser than remote=%d", loopbackLoginFailureThreshold, loginFailureThreshold)
	}
	if loopbackLoginLockoutWindow >= loginLockoutWindow {
		t.Fatalf("loopback window=%v must be shorter than remote=%v", loopbackLoginLockoutWindow, loginLockoutWindow)
	}

	// 非回环：维持现有 5 次 / 15 分钟。
	remote := lockoutTestServer("correct-password")
	remoteLocked := 0
	for i := 1; i <= loginFailureThreshold; i++ {
		rec := tryAdminLogin(t, remote, "198.51.100.9:33000", "wrong")
		if rec.Code == http.StatusTooManyRequests {
			remoteLocked = i
			if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs <= int(loopbackLoginLockoutWindow.Seconds()) {
				t.Fatalf("remote Retry-After=%q err=%v, want > %v", rec.Header().Get("Retry-After"), err, loopbackLoginLockoutWindow)
			}
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("remote attempt %d: status=%d", i, rec.Code)
		}
	}
	if remoteLocked != loginFailureThreshold {
		t.Fatalf("remote locked at attempt %d, want %d", remoteLocked, loginFailureThreshold)
	}

	// 回环：同一次数下仍未锁定，说明阈值确实被放宽。
	loop := lockoutTestServer("correct-password")
	for i := 1; i <= loginFailureThreshold; i++ {
		if rec := tryAdminLogin(t, loop, "127.0.0.1:51502", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("loopback attempt %d: status=%d, want 401 (looser budget)", i, rec.Code)
		}
	}
	// 但回环不是无限次：继续失败最终一定会撞上上限。
	loopLocked := 0
	for i := loginFailureThreshold + 1; i <= loopbackLoginFailureThreshold; i++ {
		rec := tryAdminLogin(t, loop, "127.0.0.1:51502", "wrong")
		if rec.Code == http.StatusTooManyRequests {
			loopLocked = i
			if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs > int(loopbackLoginLockoutWindow.Seconds())+1 {
				t.Fatalf("loopback Retry-After=%q err=%v, want <= %v", rec.Header().Get("Retry-After"), err, loopbackLoginLockoutWindow)
			}
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("loopback attempt %d: status=%d", i, rec.Code)
		}
	}
	if loopLocked != loopbackLoginFailureThreshold {
		t.Fatalf("loopback locked at attempt %d, want cap at %d", loopLocked, loopbackLoginFailureThreshold)
	}
}
