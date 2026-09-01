package turnstile

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/phonecdp"
)

// TestPhoneAttachLive 真机验证接管路径：确保转发、拉起 Cromite、建隔离上下文、打开注册页、
// 等表单渲染出来。
//
// 默认跳过 —— 它要一台插着的手机，普通 go test 不该依赖外设。跑法：
//
//	$env:M365_PHONE_LIVE=1; $env:M365_PHONE_ADB='<adb.exe 绝对路径>'
//	go test -C <repo> ./internal/turnstile -run TestPhoneAttachLive -v
//
// 刻意停在「表单出现」，不填表也不提交：提交会真的用掉手机当前 IP 的当日额度，还可能在
// 站点侧建出一个本地没记密码的孤儿号。要验的是接管本身，不需要付那个代价。
func TestPhoneAttachLive(t *testing.T) {
	if os.Getenv("M365_PHONE_LIVE") == "" {
		t.Skip("需要真机；设置 M365_PHONE_LIVE=1 再跑")
	}
	adb := strings.TrimSpace(os.Getenv("M365_PHONE_ADB"))
	if adb == "" {
		t.Fatal("请用 M365_PHONE_ADB 给出 adb.exe 的绝对路径（Windows 上它一般不在 PATH 里）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	phone, err := AttachPhone(ctx, phonecdp.Config{ADB: adb}, "https://office.965007.xyz")
	if err != nil {
		t.Fatalf("接管手机浏览器失败：%v", err)
	}
	defer phone.Close()
	t.Logf("已接管：%s", phone.Describe())

	var ua string
	if err := phone.sess.eval(ctx, "navigator.userAgent", &ua); err != nil {
		t.Fatalf("读 UA 失败：%v", err)
	}
	t.Logf("UA = %s", ua)
	// 这是「页面真的在手机上」的证据。少了它，整套改造的前提就不成立 —— 页面在本机加载的话
	// 出口就是家里的宽带，而那不会报错，只会表现为号一直注册不上。
	if !strings.Contains(ua, "Android") {
		t.Fatalf("UA 里没有 Android，说明这个页面不是在手机上跑：%s", ua)
	}
	// headless 下这个站点的 Turnstile 解不出来（实测挑战一直转），所以有头是前提，
	// 顺手确认接管到的不是某个无头实例。
	if strings.Contains(ua, "HeadlessChrome") {
		t.Fatalf("接管到的是无头浏览器，Turnstile 解不出来：%s", ua)
	}

	page := strings.TrimSpace(os.Getenv("M365_PHONE_PAGE"))
	if page == "" {
		page = "https://office.965007.xyz/"
	}
	if err := phone.sess.navigate(ctx, page); err != nil {
		t.Fatalf("导航到 %s 失败：%v", page, err)
	}
	if err := phone.sess.waitForForm(ctx); err != nil {
		t.Fatalf("注册表单没出现：%v", err)
	}
	// 轮询等 window.turnstile 出现，而不是立刻断言。
	//
	// 脚本是异步从 challenges.cloudflare.com 拉的，页面刚出表单时它一定还没到 —— 立刻断
	// 言只会误报。而「它到底能不能到」是这条路线最实在的风险：Cromite 自带 Adblock，如果
	// 把 Cloudflare 的挑战脚本拦了，注册在手机上就无解，那要在写更多代码之前先知道。
	var state struct {
		Box     bool   `json:"box"`
		Hidden  bool   `json:"hidden"`
		Input   bool   `json:"input"`
		API     bool   `json:"api"`
		Message string `json:"message"`
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if err := phone.sess.eval(ctx, widgetStateJS, &state); err != nil {
			t.Fatalf("读 Turnstile 部件状态失败：%v", err)
		}
		if state.API {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("部件状态：box=%v hidden=%v input=%v api=%v message=%q",
		state.Box, state.Hidden, state.Input, state.API, state.Message)
	if !state.API {
		t.Fatalf("等了 25 秒 window.turnstile 还没出现，手机上过不了挑战（先查 Cromite 的 Adblock 是否拦了 challenges.cloudflare.com）；页面提示：%q", state.Message)
	}
}
