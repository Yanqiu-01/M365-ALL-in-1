package turnstile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 审计发现 #36：webviewMu 是无条件阻塞锁，排队期间 ctx 过期也照样往协作目录写一份
// 新 job —— 给 WebView 派一个调用方已经放弃的任务，还会留下 job/result 残留干扰下
// 一次求解。这组用例把「过期就不提交」钉住。

func TestAcquireWebViewRespectsAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquireWebView(ctx); err == nil {
		releaseWebView()
		t.Fatal("an already-cancelled request must not be queued at all")
	}
	// 槽位必须没被占用，否则后续请求会被一个不存在的任务挡住。
	select {
	case webviewSlot <- struct{}{}:
		<-webviewSlot
	default:
		t.Error("slot was consumed by a request that never ran")
	}
}

func TestAcquireWebViewTimesOutWhileQueued(t *testing.T) {
	// 先占住槽位，模拟前一个任务还在跑。
	if err := acquireWebView(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	defer releaseWebView()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := acquireWebView(ctx)
	if err == nil {
		releaseWebView()
		t.Fatal("a queued request whose deadline passed must fail, not proceed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap the context cause, got %v", err)
	}
	// 必须是等待后超时，而不是立刻返回 —— 前者说明它真的在排队。
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned after %v; it should have waited for the deadline", elapsed)
	}
}

func TestAcquireWebViewSerializesAndReleases(t *testing.T) {
	if err := acquireWebView(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	releaseWebView()
	// 释放后必须能立刻再取得，否则并发注册会永久卡死。
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := acquireWebView(ctx); err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
	releaseWebView()
}

// releaseWebView 必须可以在未持有槽位时安全调用，否则错误路径上的 defer 会阻塞。
func TestReleaseWebViewIsSafeWhenNotHeld(t *testing.T) {
	done := make(chan struct{})
	go func() {
		releaseWebView()
		releaseWebView()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("releaseWebView blocked when the slot was not held")
	}
}

// 端到端：ctx 过期时 solveViaWebView 不得在协作目录里留下任何文件。
func TestSolveViaWebViewWritesNothingWhenContextExpired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "flare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := solveViaWebView(ctx, dir, "https://office.example.test/", "d", "u", "p", ""); err == nil {
		t.Fatal("expected failure for a cancelled request")
	}
	for _, name := range []string{"job", "job.tmp", "result", "status"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written for a request nobody is waiting on", name)
		}
	}
}
