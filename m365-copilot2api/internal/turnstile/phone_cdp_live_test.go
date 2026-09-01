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

// TestPhoneTurnstileTokenLive 把手机路径一直跑到「Turnstile 交出 token」为止。
//
// 和 TestPhoneAttachLive 的分工：那个只证明页面和控件起来了，而「控件起来了」离「拿到
// token」还差最关键的一步 —— 实测过不了 CF 时，控件照样铺开、脚本照样 200，卡的是发不发
// token。所以这个用例直接调 awaitTurnstileToken，走的是注册时一模一样的代码路径（同样的
// 预算、同样的点击补偿），失败时把六种文案里真正命中的那条原样打出来，不必再靠猜。
//
// 同样不填表、不提交：token 拿到就丢掉。拿 token 本身不消耗站点的当日注册额度，只有提交才
// 会 —— 所以这个用例可以反复跑，这正是排查需要的。
//
// 注意：手机上装的是 Cromite 时，这个用例**必然失败**，而且不是它自己的毛病。Cromite 给
// canvas 加噪（见 TestPhoneFingerprintNoiseLive），Cloudflare 拿不到稳定指纹就一直不发
// token。所以线上默认已经不走手机浏览器了（phone_browser_on 默认关）。留着这个用例是为了
// 在手机上换成普通 Chrome/Chromium 之后能一次性验完那条路。
//
//	$env:M365_PHONE_LIVE=1; $env:M365_PHONE_ADB='<adb.exe 绝对路径>'
//	go test -C <repo> ./internal/turnstile -run TestPhoneTurnstileTokenLive -v -count=1
func TestPhoneTurnstileTokenLive(t *testing.T) {
	if os.Getenv("M365_PHONE_LIVE") == "" {
		t.Skip("需要真机；设置 M365_PHONE_LIVE=1 再跑")
	}
	adb := strings.TrimSpace(os.Getenv("M365_PHONE_ADB"))
	if adb == "" {
		t.Fatal("请用 M365_PHONE_ADB 给出 adb.exe 的绝对路径（Windows 上它一般不在 PATH 里）")
	}
	// 比 90 秒宽：awaitTurnstileToken 自己的预算就有一分钟量级，再加接管和首屏，卡到超时
	// 的话得让它以自己的错误收场，而不是被外层 ctx 掐断成一句无信息的 context deadline。
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// M365_PHONE_WIPE=1 时先把 Cromite 的数据整个擦掉再跑。
	//
	// 这不是洁癖，是这个用例可信的前提。手机上只有一个长驻 profile，同一个 profile 连着解
	// 挑战会累积状态；早先我在一个已经失败过六次的 profile 上反复重试，把「解不出来」记到了
	// 浏览器头上，而对照用的 PC Chrome 每次都是全新的 user-data-dir —— 两边的干净程度根本
	// 不一样，那个对比不成立。要判断「这个浏览器能不能解」，就得从和 PC 一样干净的状态出发。
	if os.Getenv("M365_PHONE_WIPE") == "1" {
		if err := wipePhoneBrowser(ctx, adb); err != nil {
			t.Fatalf("擦除手机浏览器数据失败：%v", err)
		}
		t.Log("已 pm clear 手机浏览器并重启（本次从全新 profile 开始）")
	}

	page := strings.TrimSpace(os.Getenv("M365_PHONE_PAGE"))
	if page == "" {
		page = "https://office.965007.xyz/"
	}
	phone, err := AttachPhone(ctx, phonecdp.Config{ADB: adb}, originOf(page))
	if err != nil {
		t.Fatalf("接管手机浏览器失败：%v", err)
	}
	defer phone.Close()
	t.Logf("已接管：%s", phone.Describe())

	if err := phone.sess.navigate(ctx, page); err != nil {
		t.Fatalf("导航到 %s 失败：%v", page, err)
	}
	if err := phone.sess.waitForForm(ctx); err != nil {
		t.Fatalf("注册表单没出现：%v", err)
	}
	// 填表 + 等控件铺开：真实注册路径（chromeRegister）在等 token 之前就做了这两步，少哪
	// 一步这个用例就不是在验真实路径。填表尤其要紧 —— Turnstile 有的配置要等表单交互才开
	// 始跑挑战，不填表的话「token 不来」可能只是这个用例自己的毛病，跟线上失败无关。
	//
	// 用户名故意用一个不会去提交的值：这个用例只到拿 token，永远不调 submitRegisterForm。
	if err := phone.sess.fillRegisterForm(ctx, ChromeRequest{
		DisplayName: "ProbeUser", Username: "probe-does-not-submit",
		Password: "Probe-Only-Never-Submitted-1", PlanID: "1", DomainID: "1",
	}); err != nil {
		t.Fatalf("填表失败：%v", err)
	}
	if err := phone.sess.waitWidgetLaidOut(ctx); err != nil {
		t.Fatalf("控件没铺开：%v", err)
	}

	started := time.Now()
	token, tokenErr := phone.sess.awaitTurnstileToken(ctx)
	elapsed := time.Since(started).Round(time.Second)

	// 无论成败都把现场打全：部件状态和 iframe 几何是区分「脚本没加载」「控件没铺开」
	// 「铺开了但不发 token」这三种情况的唯一依据，而它们的处置完全不同。
	var state struct {
		Box     bool   `json:"box"`
		Hidden  bool   `json:"hidden"`
		Input   bool   `json:"input"`
		API     bool   `json:"api"`
		Message string `json:"message"`
	}
	if err := phone.sess.eval(ctx, widgetStateJS, &state); err == nil {
		t.Logf("部件状态：box=%v hidden=%v input=%v api=%v message=%q",
			state.Box, state.Hidden, state.Input, state.API, state.Message)
	}
	// 视口尺寸 + 滚动量 + iframe 的视口坐标一起打：判断「点击有没有落在控件上」只能靠这
	// 三个数一起看。iframe 的 y 必须落在 0..innerHeight 之间，否则点下去就是空气。
	var view struct {
		W       float64 `json:"w"`
		H       float64 `json:"h"`
		ScrollY float64 `json:"scrollY"`
	}
	if err := phone.sess.eval(ctx,
		`({w: innerWidth, h: innerHeight, scrollY: scrollY})`, &view); err == nil {
		t.Logf("视口：%.0fx%.0f scrollY=%.0f", view.W, view.H, view.ScrollY)
	}
	if rect, err := phone.sess.challengeIframeRect(ctx); err == nil {
		t.Logf("challenge iframe（视口坐标）：ok=%v %.0fx%.0f @(%.0f,%.0f) reason=%s",
			rect.OK, rect.Width, rect.Height, rect.X, rect.Y, rect.Reason)
		if rect.OK && (rect.Y < 0 || rect.Y > view.H) && view.H > 0 {
			t.Errorf("iframe 的 y=%.0f 在视口 0..%.0f 之外，点击必然落空", rect.Y, view.H)
		}
	}

	if tokenErr != nil {
		t.Fatalf("等了 %s 仍未拿到 token：%v", elapsed, tokenErr)
	}
	t.Logf("拿到 token，耗时 %s，长度 %d", elapsed, len(token))
}
