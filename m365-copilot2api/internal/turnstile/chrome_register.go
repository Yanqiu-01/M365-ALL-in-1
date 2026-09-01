package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"m365-copilot2api/internal/phonecdp"
)

// ChromeRequest 描述一次「本机浏览器打开注册页、填表、过 Turnstile、提交」。
//
// 和 FlareSolverr 路线的区别是这里全程握着页面：token 是从隐藏 input 的 DOM
// property 上读的，提交也在页面内完成 —— IP、UA、cookie、Referer 与拿 token 时
// 完全一致，站点侧看到的就是一次正常注册。
type ChromeRequest struct {
	PageURL     string
	Proxy       string
	DisplayName string
	Username    string
	Password    string
	PlanID      string
	DomainID    string
	Timeout     time.Duration
	// Headless 强制无界面。默认有头（窗口挪到屏幕外）—— headless 下 Turnstile 会一直
	// 转圈，见 chromeHeadless 的注释。零值就是「按环境变量决定」，也就是默认有头。
	Headless bool
	// Phone 非空时，这一轮不在本机开浏览器，而是接管手机上已经在跑的 Cromite（见
	// phone_cdp.go）。
	//
	// 它和 Proxy 是互斥的，而且必须是互斥的：页面在手机上加载，出口就是手机的运营商 IP，
	// 此时 Proxy 里的代理地址对这一轮毫无作用 —— 真正危险的是反过来被误读成「已经在走
	// 代理了」。所以下面 SolveWithChrome 里 Phone 优先，并且不去碰 Proxy。
	//
	// 给的是配置而不是一个已经建好的会话：接管要和 launchChrome 一样每号一次，这样每个号
	// 拿到的是全新的浏览器上下文（cookie/storage 独立）。共用一个会话能省一两秒，但要用
	// 上一个号的 cookie 去解下一个号的 Turnstile，那正是本机模式用「一次性 profile」在
	// 避免的事 —— AttachPhone 为此每号都把手机上的浏览器整个擦掉重启，代价是几秒冷启动。
	Phone *phonecdp.Config
}

// ChromeDefaultTimeout 是一个号的总预算：开浏览器、过 CF、填表、提交、读结果。
//
// 150 秒不够。实测过 CF 本身要 11~22 秒，而提交之后站点会把页面停在「正在创建 Office
// 账号，请等待成功响应后再进行下一步操作」—— 建号是后端慢操作，60 秒的等待预算会在站点
// 还在建的时候就放弃。放弃的代价不是「这个号失败」这么轻：账号很可能已经在站点侧建出来
// 了，只是本地没记下来，于是留下一个查不到密码的孤儿号（编号 5831 就是这么来的）。
// 宁可让一个号多等几分钟，也不要制造孤儿。
const ChromeDefaultTimeout = 360 * time.Second

// submitResultWait 是提交之后等站点给结果的预算。
//
// 单独拎出来是因为它和前面几个阶段的性质不同：提交之前放弃只是白跑一趟，提交之后放弃
// 会留下孤儿号。所以这一段给得比其它阶段宽得多。
const submitResultWait = 240 * time.Second

const tokenValueJS = `(() => { const el = document.querySelector('input[name="cf-turnstile-response"]'); return el ? (el.value || '') : ''; })()`

// widgetStateJS 不查 iframe：它在闭合 shadow root 里，JS 一定查不到，拿它当「有没有
// 渲染」的判据只会给出错误结论（实测 iframe 明明存在且回了 200）。
const widgetStateJS = `(() => {
  const box = document.getElementById('turnstileBox');
  const input = document.querySelector('input[name="cf-turnstile-response"]');
  const msg = document.getElementById('message');
  const r = box ? box.getBoundingClientRect() : null;
  return {
    box: !!box,
    hidden: box ? box.classList.contains('hidden') : false,
    laidOut: !!(r && r.width > 20 && r.height > 20),
    input: !!input,
    api: !!window.turnstile,
    message: msg ? (msg.textContent || '') : ''
  };
})()`

// SolveWithChrome 用本机浏览器走完一个号的注册，返回 SUBMITTED:<upn> 形式的结果。
//
// 复用 Solution.Token 上的 SUBMITTED: 约定，是为了让 completeRegister 那边不必分两
// 套逻辑：Android 的 WebView 早就是这么回话的。
func SolveWithChrome(ctx context.Context, request ChromeRequest) (Solution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	page := strings.TrimSpace(request.PageURL)
	if page == "" {
		return Solution{}, errors.New("缺少注册页地址")
	}
	if strings.TrimSpace(request.Username) == "" || strings.TrimSpace(request.Password) == "" {
		return Solution{}, errors.New("缺少用户名或密码，无法填表")
	}
	budget := request.Timeout
	if budget <= 0 {
		budget = ChromeDefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		session *chromeSession
		exit    string
		err     error
	)
	if request.Phone != nil {
		var phone *PhoneSession
		phone, err = AttachPhone(runCtx, *request.Phone, originOf(page))
		if err == nil {
			session, exit = phone.sess, phone.Describe()
		}
	} else {
		session, err = launchChrome(runCtx, request.Proxy, request.Headless || chromeHeadless())
	}
	if err != nil {
		return Solution{}, err
	}
	defer session.Close()

	var userAgent string
	_ = session.eval(runCtx, "navigator.userAgent", &userAgent)
	// 每条返回都盖上 UA 和出口说明。手工写的话，早退分支会漏 —— 之前 navigate/waitForForm
	// 那三条就漏了 UA，而失败时这两条信息恰恰是最该看的。
	out := func(token string) Solution {
		return Solution{Token: token, UserAgent: userAgent, Exit: exit}
	}

	if err := session.navigate(runCtx, page); err != nil {
		return out(""), err
	}
	if err := session.waitForForm(runCtx); err != nil {
		return out(""), err
	}
	if err := session.fillRegisterForm(runCtx, request); err != nil {
		return out(""), err
	}
	if err := session.waitWidgetLaidOut(runCtx); err != nil {
		return out(""), err
	}
	token, err := session.awaitTurnstileToken(runCtx)
	if err != nil {
		return out(""), err
	}
	upn, err := session.submitRegisterForm(runCtx)
	if err != nil {
		return out(""), err
	}
	if strings.TrimSpace(upn) == "" {
		upn = "registered-ok"
	}
	_ = token // token 已随表单提交出去，调用方不需要它，也不该留副本。
	return out("SUBMITTED:" + upn), nil
}

// originOf 从页面地址取出 scheme://host，用来清这个站点的存储。取不出来就返回空串，
// 让调用方跳过那一步 —— 传一个猜的来源进去只会清错地方。
func originOf(page string) string {
	parsed, err := url.Parse(strings.TrimSpace(page))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// waitForForm 等 app.js 把注册表单渲染出来。
func (s *chromeSession) waitForForm(ctx context.Context) error {
	deadline := time.Now().Add(20 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		var ready bool
		if err := s.eval(ctx, `!!document.getElementById('registerForm') && !!document.getElementById('username')`, &ready); err == nil && ready {
			return nil
		}
		_ = s.eval(ctx, `(document.getElementById('message')||{}).textContent || ''`, &lastState)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if msg := strings.TrimSpace(lastState); msg != "" {
		return fmt.Errorf("注册表单没有出现，页面提示：%s", msg)
	}
	return errors.New("注册表单没有出现（页面可能被站点拦下或 /api/public/site-config 取不到）")
}

type fillReport struct {
	Missing      []string `json:"missing"`
	NeedsEmail   bool     `json:"needsEmail"`
	NeedsInvite  bool     `json:"needsInvite"`
	PlanSelected string   `json:"planSelected"`
}

// fillRegisterForm 按页面自己的字段 ID 填表，并触发 input/change，让站点脚本看到值。
func (s *chromeSession) fillRegisterForm(ctx context.Context, request ChromeRequest) error {
	payload, err := json.Marshal(map[string]string{
		"displayName": strings.TrimSpace(request.DisplayName),
		"username":    strings.TrimSpace(request.Username),
		"password":    request.Password,
		"planId":      strings.TrimSpace(request.PlanID),
		"domainId":    strings.TrimSpace(request.DomainID),
	})
	if err != nil {
		return err
	}
	expression := `(() => {
  const data = ` + string(payload) + `;
  const missing = [];
  const set = (id, value) => {
    const el = document.getElementById(id);
    if (!el) { missing.push(id); return; }
    el.value = value;
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
  };
  const pick = (id, value) => {
    const el = document.getElementById(id);
    if (!el) { missing.push(id); return; }
    if (value) {
      const has = Array.from(el.options || []).some(o => o.value === value);
      if (has) el.value = value;
    }
    el.dispatchEvent(new Event('change', {bubbles: true}));
  };
  pick('planId', data.planId);
  pick('domainId', data.domainId);
  set('displayName', data.displayName);
  set('username', data.username);
  set('password', data.password);
  const emailSection = document.getElementById('emailVerifySection');
  const inviteInput = document.getElementById('inviteCode');
  return {
    missing,
    needsEmail: !!(emailSection && !emailSection.classList.contains('hidden')),
    needsInvite: !!(inviteInput && inviteInput.required),
    planSelected: (document.getElementById('planId')||{}).value || ''
  };
})()`
	var report fillReport
	if err := s.eval(ctx, expression, &report); err != nil {
		return err
	}
	if len(report.Missing) > 0 {
		return fmt.Errorf("注册页缺少字段 %s：站点表单结构可能变了", strings.Join(report.Missing, "、"))
	}
	// 这两项本程序都没有对应能力，静默继续只会在提交时收到含义不明的失败。
	if report.NeedsEmail {
		return errors.New("站点开启了邮箱验证码，本程序没有收码能力，无法自动注册")
	}
	if report.NeedsInvite {
		return errors.New("站点要求邀请码，请先在面板注册配置里补上邀请码")
	}
	return nil
}

// awaitTurnstileToken 等隐藏 input 上出现 token，必要时点一次复选框。
//
// 有头模式下实测两个不同出口都在 12~13 秒内自动给出 token，不需要交互。点击是为需要
// 交互的那种控件留的后手：先等，等不到再点，之后继续等。
//
// 坐标取自 challengeIframeRect —— 不是容器。容器 454 宽，iframe 只有 300 宽，按容器
// 算会偏出复选框。
func (s *chromeSession) awaitTurnstileToken(ctx context.Context) (string, error) {
	deadline := time.Now().Add(75 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		if soft := d.Add(-45 * time.Second); soft.Before(deadline) {
			deadline = soft
		}
	}
	nextClick := time.Now().Add(20 * time.Second)
	clicks := 0
	// window.turnstile 一直不出现，就是 challenges.cloudflare.com 没能通过这个出口加载。
	// 这种出口再等下去也不会好，而调用方要靠换出口重试来绕过它 —— 等满 55 秒纯粹是把
	// 重试预算烧在一个已经确定不行的出口上。实测能过的出口 12~22 秒就给出 token，脚本
	// 本身出现得更早，所以 apiDeadline 给 18 秒足够宽。
	apiDeadline := time.Now().Add(18 * time.Second)
	for time.Now().Before(deadline) {
		var token string
		if err := s.eval(ctx, tokenValueJS, &token); err == nil {
			if token = strings.TrimSpace(token); token != "" {
				return token, nil
			}
		}
		if !apiDeadline.IsZero() && time.Now().After(apiDeadline) {
			var haveAPI bool
			if err := s.eval(ctx, `!!window.turnstile`, &haveAPI); err == nil {
				if !haveAPI {
					return "", errors.New("challenges.cloudflare.com 的脚本没能通过当前出口加载，Turnstile 控件起不来：换一个出口再试")
				}
				apiDeadline = time.Time{} // 已经确认脚本在了，不用再查。
			}
		}
		if time.Now().After(nextClick) && clicks < 3 {
			clicks++
			nextClick = time.Now().Add(12 * time.Second)
			// 先滚再量：拿到的是视口坐标，量完到点下去之间控件要是被滚出视口，点击就落空。
			// 手机视口只有 780 高而控件在页面 1600 多的位置，这一步不是保险，是必需。
			var scrolled bool
			_ = s.eval(ctx, scrollWidgetIntoViewJS, &scrolled)
			if rect, err := s.challengeIframeRect(ctx); err == nil && rect.OK {
				_ = s.clickAt(ctx, rect.X+28, rect.Y+rect.Height/2)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	var state struct {
		Box     bool   `json:"box"`
		Hidden  bool   `json:"hidden"`
		Input   bool   `json:"input"`
		API     bool   `json:"api"`
		Message string `json:"message"`
	}
	_ = s.eval(ctx, widgetStateJS, &state)
	switch {
	case !state.Box:
		return "", errors.New("注册页没有 Turnstile 容器，站点结构可能变了")
	case !state.API:
		return "", errors.New("challenges.cloudflare.com 的脚本没能通过当前出口加载，Turnstile 控件起不来：换一个出口再试")
	default:
		if msg := strings.TrimSpace(state.Message); msg != "" {
			return "", fmt.Errorf("Turnstile 在预算内没给出 token，页面提示：%s", msg)
		}
		// headless 是已知会卡在这里的原因：控件转圈、它自己的请求一路 200，但 token
		// 永远不来。默认已经是有头，所以显式开了 headless 时要先说这件事。
		if chromeHeadless() {
			return "", fmt.Errorf("Turnstile 一直没给出 token（点击 %d 次无效）：当前是 headless 模式，实测这种模式下 Cloudflare 会一直拖着不发 token。去掉 M365_CHROME_HEADLESS 用有头模式", clicks)
		}
		return "", fmt.Errorf("Turnstile 一直没给出 token（点击 %d 次无效）：该出口 IP 可能被 Cloudflare 判为高风险，换一个出口再试", clicks)
	}
}

// widgetRectJS 量的是 #turnstileBox 本身，而不是里面的 iframe。
//
// Turnstile 把 challenge iframe 挂在闭合 shadow root 里：document.querySelectorAll
// ('iframe') 数出来是 0，box.querySelector('iframe') 同样拿不到 —— 但 CDP 的网络日志
// 显示那个 challenge Document 确实请求到并回了 200，容器也从 454x0 长成了 454x69。
// 所以判断「控件在不在、能不能点」只能看容器的布局，不能靠 DOM 里找 iframe。
const widgetRectJS = `(() => {
  const box = document.getElementById('turnstileBox');
  if (!box) return {ok: false, reason: 'no-box'};
  if (box.classList.contains('hidden')) return {ok: false, reason: 'hidden'};
  box.scrollIntoView({block: 'center'});
  const r = box.getBoundingClientRect();
  if (r.width < 20 || r.height < 20) return {ok: false, reason: 'not-laid-out', width: r.width, height: r.height};
  return {ok: true, x: r.x, y: r.y, width: r.width, height: r.height};
})()`

// scrollWidgetIntoViewJS 把控件滚到视口中间，返回它现在是否真的在视口里。
//
// 用 'instant' 而不是默认的平滑滚动：平滑滚动是异步的，量坐标时可能还在半路上，点击就会
// 落在控件外面。
const scrollWidgetIntoViewJS = `(() => {
  const box = document.getElementById('turnstileBox');
  if (!box) return false;
  box.scrollIntoView({block: 'center', behavior: 'instant'});
  const r = box.getBoundingClientRect();
  return r.top >= 0 && r.bottom <= (window.innerHeight || 0);
})()`

type widgetRect struct {
	OK     bool    `json:"ok"`
	Reason string  `json:"reason"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// waitWidgetLaidOut 等控件真的铺开。
//
// 实测容器在 6 秒时还是 454x0，14 秒才变成 454x69。高度是 0 的时候点下去只会落在空
// 处 —— 这正是之前「点了 4 次仍然没有 token」的原因。
func (s *chromeSession) waitWidgetLaidOut(ctx context.Context) error {
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var last widgetRect
	for time.Now().Before(deadline) {
		if err := s.eval(ctx, widgetRectJS, &last); err == nil && last.OK {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	switch last.Reason {
	case "no-box":
		return errors.New("注册页没有 Turnstile 容器，站点结构可能变了")
	case "hidden":
		return errors.New("站点没有下发 Turnstile sitekey（/api/public/site-config 取不到或该出口被拒），控件未加载")
	default:
		return errors.New("Turnstile 控件在预算内没有铺开（challenge iframe 没能通过当前出口加载）：换一个出口再试")
	}
}

// clickTurnstile 在复选框位置派发一次真实点击。
//
// 坐标取容器左侧 30px、垂直居中：那是复选框的位置，点中间会落在说明文字上。CDP 的
// Input 事件带 isTrusted=true，页面里造的事件不算。
func (s *chromeSession) clickTurnstile(ctx context.Context) error {
	var rect widgetRect
	if err := s.eval(ctx, widgetRectJS, &rect); err != nil {
		return err
	}
	if !rect.OK {
		return fmt.Errorf("Turnstile 控件还没有可点击的区域 (%s)", rect.Reason)
	}
	return s.clickAt(ctx, rect.X+30, rect.Y+rect.Height/2)
}

// submitRegisterForm 触发页面自己的提交流程，再读 #message 的结果。
//
// 用 requestSubmit 而不是直接 POST：站点的 submitRegister 会带上 token、按它自己的
// 约定组装 payload，并把 UPN 写进 #message。走页面的路，站点看到的就是正常注册。
func (s *chromeSession) submitRegisterForm(ctx context.Context) (string, error) {
	var submitted bool
	expression := `(() => {
  const form = document.getElementById('registerForm');
  const msg = document.getElementById('message');
  if (!form) return false;
  if (msg) { msg.className = 'message'; msg.textContent = ''; }
  if (form.requestSubmit) { form.requestSubmit(); } else {
    form.dispatchEvent(new Event('submit', {bubbles: true, cancelable: true}));
  }
  return true;
})()`
	if err := s.eval(ctx, expression, &submitted); err != nil {
		return "", err
	}
	if !submitted {
		return "", errors.New("注册表单不存在，无法提交")
	}
	deadline := time.Now().Add(submitResultWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var last struct {
		Class string `json:"class"`
		Text  string `json:"text"`
	}
	readJS := `(() => {
  const msg = document.getElementById('message');
  return {class: msg ? (msg.className || '') : '', text: msg ? (msg.textContent || '') : ''};
})()`
	for time.Now().Before(deadline) {
		if err := s.eval(ctx, readJS, &last); err == nil {
			if strings.Contains(last.Class, "success") {
				return upnFromSuccessMessage(last.Text), nil
			}
			if strings.Contains(last.Class, "error") {
				text := strings.TrimSpace(last.Text)
				if text == "" {
					text = "站点返回失败但没有给出原因"
				}
				return "", errors.New(text)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if text := strings.TrimSpace(last.Text); text != "" {
		return "", fmt.Errorf("提交后在预算内没有拿到结果，页面停在：%s", text)
	}
	return "", errors.New("提交后在预算内没有拿到站点结果")
}

// upnFromSuccessMessage 从「注册成功：user@domain」里取出 UPN。
//
// 站点用的是全角冒号，且成功文案可配置，所以取不出来时返回空串，由调用方回落到
// 本地拼出的邮箱 —— 拿不到 UPN 不该让一个已经注册成功的号算失败。
func upnFromSuccessMessage(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	for _, sep := range []string{"：", ":"} {
		if i := strings.LastIndex(trimmed, sep); i >= 0 {
			if candidate := strings.TrimSpace(trimmed[i+len(sep):]); strings.Contains(candidate, "@") {
				return candidate
			}
		}
	}
	if strings.Contains(trimmed, "@") && !strings.ContainsAny(trimmed, " \t\n") {
		return trimmed
	}
	return ""
}
