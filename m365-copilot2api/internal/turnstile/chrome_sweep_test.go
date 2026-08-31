package turnstile

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 出口普查：默认跳过。逐个出口跑到「拿到 token」为止就停，不提交表单 ——
// 站点限制每个 IP 每天只能成功注册 1 次，普查不该把额度用掉。
//
// 它回答的是一个经验问题：池子里到底有没有出口能过 Turnstile，比例是多少。
// 换出口重试的价值完全取决于这个比例。
//
//	M365_LIVE_TURNSTILE=1 M365_LIVE_PAGE=https://... \
//	  M365_LIVE_EXITS=/path/to/exits.txt M365_LIVE_SWEEP_MAX=8 \
//	  go test ./internal/turnstile -run TestLiveSweepExitsForToken -v -timeout 40m
func TestLiveSweepExitsForToken(t *testing.T) {
	if os.Getenv("M365_LIVE_TURNSTILE") != "1" {
		t.Skip("set M365_LIVE_TURNSTILE=1 to run the networked sweep")
	}
	page := strings.TrimSpace(os.Getenv("M365_LIVE_PAGE"))
	listFile := strings.TrimSpace(os.Getenv("M365_LIVE_EXITS"))
	if page == "" || listFile == "" {
		t.Skip("set M365_LIVE_PAGE and M365_LIVE_EXITS")
	}
	raw, err := os.ReadFile(listFile)
	if err != nil {
		t.Fatalf("read exits: %v", err)
	}
	var exits []string
	for _, line := range strings.Split(string(raw), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			exits = append(exits, s)
		}
	}
	max := 8
	if v := strings.TrimSpace(os.Getenv("M365_LIVE_SWEEP_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	if max > len(exits) {
		max = len(exits)
	}

	var ok, failed int
	for i := 0; i < max; i++ {
		exit := exits[i]
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			defer cancel()
			session, err := launchChrome(ctx, exit, false)
			if err != nil {
				failed++
				t.Logf("[%d/%d] %s launch failed: %v", i+1, max, exit, err)
				return
			}
			defer session.Close()
			started := time.Now()
			if err := session.navigate(ctx, page); err != nil {
				failed++
				t.Logf("[%d/%d] %s navigate failed: %v", i+1, max, exit, err)
				return
			}
			if err := session.waitForForm(ctx); err != nil {
				failed++
				t.Logf("[%d/%d] %s form failed: %v", i+1, max, exit, err)
				return
			}
			if err := session.fillRegisterForm(ctx, ChromeRequest{
				DisplayName: "SweepProbe", Username: "sweep-does-not-submit",
				Password: "Sweep-Only-Never-Submitted-1", PlanID: "1", DomainID: "1",
			}); err != nil {
				failed++
				t.Logf("[%d/%d] %s fill failed: %v", i+1, max, exit, err)
				return
			}
			token, err := session.awaitTurnstileToken(ctx)
			if err != nil {
				failed++
				t.Logf("[%d/%d] %s TOKEN-FAIL after %s: %v", i+1, max, exit, time.Since(started).Round(time.Second), err)
				return
			}
			ok++
			t.Logf("[%d/%d] %s TOKEN-OK in %s len=%d", i+1, max, exit, time.Since(started).Round(time.Second), len(token))
		}()
	}
	t.Logf("sweep result: token-ok=%d token-fail=%d of %d tried", ok, failed, max)
	if ok == 0 {
		t.Fatalf("没有任何出口拿到 token（试了 %d 个）：换出口重试帮不上忙，问题在别处", max)
	}
}
