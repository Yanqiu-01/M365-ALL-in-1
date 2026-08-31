package turnstile

import (
	"strings"
	"testing"
)

// 用户在 PC 上点注册，收到的是「内置 FlareSolverr 需要 App 数据目录。请打开修改版
// M365 后再注册」—— 一句在 PC 上无法执行的指示。根因是内置求解器只是把任务转交给
// App 内 WebView 的中继，桌面端没有那个 WebView，它必然解不出 token。
//
// 这组用例钉住两件事：桌面端必须明确不可用，且给出的替代方案必须是用户照着就能做
// 的。同时确认 Android 侧行为没被改动。

func TestWebViewSupportedRejectsDesktopWithActionableAdvice(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		err := webViewSupported(goos)
		if err == nil {
			t.Fatalf("%s: built-in solver must be reported unavailable", goos)
		}
		msg := err.Error()
		if !strings.Contains(msg, goos) {
			t.Errorf("%s: the message should name the platform, got %q", goos, msg)
		}
		// 必须给出可执行的出路，而不是让 PC 用户去开一个安卓 App。
		for _, want := range []string{"FlareSolverr", "flaresolverr_url"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message lacks %q: %q", goos, want, msg)
			}
		}
		if strings.Contains(msg, "打开修改版M365") {
			t.Errorf("%s: still tells a desktop user to open the Android app: %q", goos, msg)
		}
	}
}

func TestWebViewSupportedAllowsAndroid(t *testing.T) {
	if err := webViewSupported("android"); err != nil {
		t.Fatalf("android must keep the in-app WebView path: %v", err)
	}
}

// 桌面端不启动内置服务，8191 才能留给真正的 FlareSolverr —— 那是它的默认端口。
func TestWebViewAvailableFollowsHostOS(t *testing.T) {
	t.Setenv("M365_TURNSTILE_FORCE_OS", "windows")
	if WebViewAvailable() {
		t.Error("desktop must not claim the built-in solver is available")
	}
	t.Setenv("M365_TURNSTILE_FORCE_OS", "android")
	if !WebViewAvailable() {
		t.Error("android must keep the built-in solver")
	}
}

// solveLocal 在桌面端要在碰协作目录之前就拒绝：错误必须是平台不支持，而不是
// 「缺少 App 数据目录」——后者会把用户引向去设置一个 PC 上毫无意义的变量。
func TestSolveLocalOnDesktopFailsBeforeTouchingDataDir(t *testing.T) {
	t.Setenv("M365_TURNSTILE_FORCE_OS", "windows")
	t.Setenv("M365_DATA_DIR", t.TempDir()) // 目录齐备，仍必须拒绝
	_, err := solveLocal(nil, "https://office.example.test/", "d", "u", "p", "")
	if err == nil {
		t.Fatal("desktop solveLocal must fail")
	}
	if strings.Contains(err.Error(), "需要 App 数据目录") {
		t.Errorf("wrong diagnosis: the data dir exists, the platform is the problem: %q", err)
	}
	if !strings.Contains(err.Error(), "FlareSolverr") {
		t.Errorf("message should point at a real FlareSolverr: %q", err)
	}
}
