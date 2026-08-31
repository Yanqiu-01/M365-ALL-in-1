package turnstile

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// 联网探针：默认跳过。它真的开浏览器、真的过 Turnstile，但停在提交之前 ——
// 站点限制每个 IP 每天只能成功注册 1 次，探针不该把这个额度用掉。
//
//	M365_LIVE_TURNSTILE=1 M365_LIVE_PAGE=https://... M365_LIVE_PROXY=http://ip:port \
//	  go test ./internal/turnstile -run TestLiveChromeGetsTurnstileToken -v
func TestLiveChromeGetsTurnstileToken(t *testing.T) {
	if os.Getenv("M365_LIVE_TURNSTILE") != "1" {
		t.Skip("set M365_LIVE_TURNSTILE=1 to run the networked probe")
	}
	page := strings.TrimSpace(os.Getenv("M365_LIVE_PAGE"))
	if page == "" {
		t.Skip("set M365_LIVE_PAGE to the register page URL")
	}
	proxy := strings.TrimSpace(os.Getenv("M365_LIVE_PROXY"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	session, err := launchChrome(ctx, proxy, os.Getenv("M365_LIVE_HEADFUL") != "1")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer session.Close()

	var ua string
	_ = session.eval(ctx, "navigator.userAgent", &ua)
	t.Logf("userAgent=%s", ua)

	started := time.Now()
	if err := session.navigate(ctx, page); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Logf("navigate ok in %s", time.Since(started).Round(time.Millisecond))
	if err := session.waitForForm(ctx); err != nil {
		t.Fatalf("form: %v", err)
	}
	t.Logf("form ready in %s", time.Since(started).Round(time.Millisecond))

	if err := session.fillRegisterForm(ctx, ChromeRequest{
		DisplayName: "ProbeUser", Username: "probe-does-not-submit",
		Password: "Probe-Only-Never-Submitted-1", PlanID: "1", DomainID: "1",
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}

	token, err := session.awaitTurnstileToken(ctx)
	if err != nil {
		var state map[string]any
		_ = session.eval(ctx, widgetStateJS, &state)
		t.Fatalf("token: %v (widget state: %#v)", err, state)
	}
	t.Logf("token acquired in %s, length=%d, prefix=%.12s...", time.Since(started).Round(time.Millisecond), len(token), token)
	if len(token) < 20 {
		t.Fatalf("token looks too short: %q", token)
	}
}
