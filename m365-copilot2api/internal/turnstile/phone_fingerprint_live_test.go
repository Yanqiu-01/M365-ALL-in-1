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

// wipePhoneBrowser 把手机浏览器的数据整个擦掉再拉起来。
//
// 擦除本身走生产路径 phonecdp.WipeBrowser，不在测试里另写一份 —— 早先这里自己拼
// pm clear + am start，而生产路径压根没擦，两边不一致正是「测试里能过、跑批就死」的来源。
//
// 这里只额外多做一件事：等冷启动。pm clear 之后调试 socket 要过几秒才在，EnsureCromite
// 自己会轮询 25 秒，对注册够用；但本文件里的用例擦完就直接连，先等一等省掉一次没必要的失败。
func wipePhoneBrowser(ctx context.Context, adb string) error {
	if err := phonecdp.WipeBrowser(ctx, phonecdp.Config{ADB: adb}); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, adb, "shell", "am", "start", "-n",
		phonecdp.DefaultPackage+"/"+phonecdp.DefaultActivity,
		"-a", "android.intent.action.VIEW", "-d", "about:blank")
	procwin.HideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("拉起浏览器失败：%w (%s)", err, strings.TrimSpace(string(out)))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(12 * time.Second):
	}
	return nil
}

// TestPhoneFingerprintNoiseLive 查 Cromite 是不是在给指纹加噪。
//
// 结论先写在这：**它确实在加噪，但这不影响 Turnstile**。同一段绘制内容连续 toDataURL 三次给
// 出三个不同结果（3586/3578/3626），每次读都重新随机，pm clear 擦不掉，命令行和 chrome://flags
// 两种极性都关不掉。所以这个用例按设计是失败的，留着只为把「噪声存在」这件事钉住。
//
// 别再拿它当「解不出 Turnstile」的原因 —— 那次真因是清状态漏了 challenges.cloudflare.com，
// 加上生产路径每个号之间没擦浏览器。当时的对照实验也不成立：PC 侧每次都是新的 user-data-dir，
// 手机侧却是一个跑过六次失败的长驻 profile，两边干净程度根本不一样。
//
//	$env:M365_PHONE_LIVE=1; $env:M365_PHONE_ADB='<adb.exe 绝对路径>'
//	go test -C <repo> ./internal/turnstile -run TestPhoneFingerprintNoiseLive -v -count=1

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
