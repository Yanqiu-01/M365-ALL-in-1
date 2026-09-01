package turnstile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/phonecdp"
	"m365-copilot2api/internal/procwin"
)

// TestPhoneFingerprintNoiseLive 查 Cromite 是不是在给指纹加噪。
//
// 为什么查这个：同一个出口 IP 下，PC 的 Chrome 走手机 SOCKS 隧道 8.2 秒就拿到 token，而手
// 机上的 Cromite 六次全部失败（每次 75 秒）。IP 和站点都被这个对照排除了，剩下的唯一变量
// 就是浏览器本身。Cromite 是主打反指纹的 Chromium 分支，而 Turnstile 本质上就是一次指纹
// 采集 —— canvas 读回来的像素被掺了随机噪声的话，Cloudflare 拿不到稳定指纹，就会一直不发
// token：控件正常铺开、脚本一路 200、也没有任何错误文案，正是我们看到的样子。
//
// 判据是「同一段绘制内容连续读两次，结果是否一致」。真实浏览器必然一致；加噪的实现每次
// 读都不同。
//
//	$env:M365_PHONE_LIVE=1; $env:M365_PHONE_ADB='<adb.exe 绝对路径>'
//	go test -C <repo> ./internal/turnstile -run TestPhoneFingerprintNoiseLive -v -count=1
// wipePhoneBrowser 把手机浏览器的数据整个擦掉再拉起来。
//
// pm clear 会连 cookie、localStorage、IndexedDB、缓存和各种偏好一起清掉，是「真正干净的
// profile」唯一可靠的做法 —— CDP 那几个 clear* 命令只能按来源清，永远盖不全。
// /data/local/tmp/chrome-command-line 不在应用数据里，pm clear 不会动它（实测确认）。
func wipePhoneBrowser(ctx context.Context, adb string) error {
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, adb, args...)
		procwin.HideWindow(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("shell", "pm", "clear", phonecdp.DefaultPackage); err != nil {
		return err
	}
	if err := run("shell", "am", "start", "-n",
		phonecdp.DefaultPackage+"/"+phonecdp.DefaultActivity,
		"-a", "android.intent.action.VIEW", "-d", "about:blank"); err != nil {
		return err
	}
	// pm clear 之后浏览器是冷启动，调试 socket 要过几秒才在。EnsureCromite 自己会重试，但
	// 冷启动比它的预算慢，这里先等一等，省掉一次没必要的失败。
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(12 * time.Second):
	}
	return nil
}

func TestPhoneFingerprintNoiseLive(t *testing.T) {
	if os.Getenv("M365_PHONE_LIVE") == "" {
		t.Skip("需要真机；设置 M365_PHONE_LIVE=1 再跑")
	}
	adb := strings.TrimSpace(os.Getenv("M365_PHONE_ADB"))
	if adb == "" {
		t.Fatal("请用 M365_PHONE_ADB 给出 adb.exe 的绝对路径")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	page := strings.TrimSpace(os.Getenv("M365_PHONE_PAGE"))
	if page == "" {
		page = "https://office.965007.xyz/"
	}
	phone, err := AttachPhone(ctx, phonecdp.Config{ADB: adb}, originOf(page))
	if err != nil {
		t.Fatalf("接管手机浏览器失败：%v", err)
	}
	defer phone.Close()
	if err := phone.sess.navigate(ctx, page); err != nil {
		t.Fatalf("导航失败：%v", err)
	}

	// 同一段绘制读三次。加噪的实现每次给的 dataURL 都不一样。
	var fp struct {
		A       string `json:"a"`
		B       string `json:"b"`
		C       string `json:"c"`
		Rects   string `json:"rects"`
		Rects2  string `json:"rects2"`
		WebGL   string `json:"webgl"`
		UA      string `json:"ua"`
		Plugins int    `json:"plugins"`
	}
	if err := phone.sess.eval(ctx, `(() => {
	  const draw = () => {
	    const c = document.createElement('canvas');
	    c.width = 200; c.height = 50;
	    const g = c.getContext('2d');
	    g.textBaseline = 'top';
	    g.font = '14px Arial';
	    g.fillStyle = '#f60';
	    g.fillRect(0, 0, 100, 25);
	    g.fillStyle = '#069';
	    g.fillText('turnstile-fp-probe', 2, 15);
	    return c.toDataURL();
	  };
	  const r = () => {
	    const d = document.createElement('div');
	    d.style.cssText = 'position:absolute;left:13.37px;top:42.42px;width:57.13px;height:19.79px';
	    document.body.appendChild(d);
	    const b = d.getBoundingClientRect();
	    d.remove();
	    return [b.x, b.y, b.width, b.height].join(',');
	  };
	  let webgl = 'n/a';
	  try {
	    const gl = document.createElement('canvas').getContext('webgl');
	    const dbg = gl && gl.getExtension('WEBGL_debug_renderer_info');
	    if (dbg) webgl = gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL);
	  } catch (e) { webgl = 'err:' + e.message; }
	  return {
	    a: draw(), b: draw(), c: draw(),
	    rects: r(), rects2: r(),
	    webgl: webgl,
	    ua: navigator.userAgent,
	    plugins: (navigator.plugins || []).length
	  };
	})()`, &fp); err != nil {
		t.Fatalf("读指纹失败：%v", err)
	}

	t.Logf("UA=%s", fp.UA)
	t.Logf("WebGL renderer=%q plugins=%d", fp.WebGL, fp.Plugins)
	t.Logf("canvas 长度=%d/%d/%d", len(fp.A), len(fp.B), len(fp.C))
	t.Logf("clientRects: %s | %s", fp.Rects, fp.Rects2)

	canvasStable := fp.A == fp.B && fp.B == fp.C
	rectsStable := fp.Rects == fp.Rects2
	t.Logf("canvas 三次一致=%v，getBoundingClientRect 两次一致=%v", canvasStable, rectsStable)

	if !canvasStable {
		t.Errorf("canvas 每次读都不同 —— Cromite 在给 canvas 加噪。Turnstile 拿不到稳定指纹，"+
			"这就是 token 一直不来的原因。样本前缀：\n  a=%.60s\n  b=%.60s\n  c=%.60s",
			fp.A, fp.B, fp.C)
	}
	if !rectsStable {
		t.Errorf("getBoundingClientRect 两次不同 —— Cromite 也在给布局尺寸加噪：%s vs %s",
			fp.Rects, fp.Rects2)
	}
	if canvasStable && rectsStable {
		t.Log("两项都稳定：反指纹加噪不是原因，得换别的方向查")
	}
}
