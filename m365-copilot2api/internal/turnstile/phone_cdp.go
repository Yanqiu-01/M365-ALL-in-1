package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"m365-copilot2api/internal/phonecdp"
)

// attachPhoneChrome 接管手机上已经在跑的 Cromite，返回一个和 launchChrome 等价的会话。
//
// 和 launchChrome 的差别集中在四处，其余（call/eval/navigate/clickAt/求解流程）完全复用：
//
//  1. 不起进程、没有一次性 user-data-dir —— 浏览器是手机上长驻的，我们只是拨过去。
//     号与号之间的隔离因此改成开跑前显式清掉 cookie/缓存/站点存储，见 clearProfileState。
//     本来想用 Target.createBrowserContext（独立上下文，等于隐身），但 Android 上它恒
//     失败（实测回 "Failed to create browser context"），所以不留那次注定失败的调用。
//  2. 不做 maskHeadless。那个函数是为了把 PC 上的无头 Chrome 伪装成有头 —— 改 UA 里的
//     HeadlessChrome 标记、把 platform 谎报成 Win32、注入 navigator.webdriver=false。
//     这里的浏览器本来就是一台真手机上的真移动端浏览器，UA 和 platform 都是真的，再去
//     覆盖只会造出「UA 说 Android、platform 说 Win32」这种自相矛盾的指纹，比不改更可疑。
//  3. 不传 --proxy-server、不装 Fetch.continueWithAuth。页面在手机上加载，出口天然就是
//     运营商 IP，换 IP 靠飞行模式；PC 这边没有任何流量需要绕。
//  4. 不做无头判断。手机上就是有头的，而有头是这个站点能解出 Turnstile 的前提
//     （headless=new 下挑战会一直转，实测过）。
func attachPhoneChrome(ctx context.Context, base, wsURL, origin string) (*chromeSession, error) {
	if strings.TrimSpace(wsURL) == "" {
		return nil, errors.New("没有手机浏览器的调试 WebSocket 地址")
	}
	session := &chromeSession{
		pending: map[int64]chan cdpReply{},
		events:  map[string]func(json.RawMessage){},
		closed:  make(chan struct{}),
	}
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, ReadBufferSize: 1 << 20, WriteBufferSize: 1 << 20}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接手机浏览器调试端口失败（%s）：%w", base, err)
	}
	conn.SetReadLimit(64 << 20)
	session.conn = conn
	go session.readLoop()

	target, err := session.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("在手机浏览器上新建标签页失败：%w", err)
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	_ = json.Unmarshal(target, &created)
	session.remoteTargetID = strings.TrimSpace(created.TargetID)

	attached, err := session.call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": created.TargetID, "flatten": true})
	if err != nil {
		session.Close()
		return nil, err
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(attached, &att)
	if strings.TrimSpace(att.SessionID) == "" {
		session.Close()
		return nil, errors.New("手机浏览器没有返回调试会话 ID")
	}
	session.sessionID = att.SessionID
	for _, domain := range []string{"Page.enable", "Runtime.enable", "DOM.enable", "Network.enable"} {
		if _, err := session.call(ctx, att.SessionID, domain, map[string]any{}); err != nil {
			session.Close()
			return nil, err
		}
	}
	if err := session.clearProfileState(ctx, origin); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

// clearProfileState 清掉上一个号留下的痕迹。
//
// 本机模式靠一次性 user-data-dir 拿到干净状态；手机上的浏览器是长驻的、只有一个 profile，
// 所以只能显式清。Turnstile 会看 cookie（尤其是 cf_clearance），不清就等于拿上一个号的
// 通行证去解下一个号的挑战。
//
// cookie 清失败要往上报：串味不会当场出错，只会表现成过一段时间后成功率莫名下降 —— 那种
// 问题事后极难归因，宁可当场停下。缓存和站点存储清失败只降级不致命，按注释里的理由忽略。
func (s *chromeSession) clearProfileState(ctx context.Context, origin string) error {
	if _, err := s.call(ctx, s.sessionID, "Network.clearBrowserCookies", map[string]any{}); err != nil {
		return fmt.Errorf("清手机浏览器 cookie 失败，无法保证和上一个号隔离：%w", err)
	}
	// 缓存留着只影响指纹一致性，不会像 cookie 那样直接把身份带过去。
	_, _ = s.call(ctx, s.sessionID, "Network.clearBrowserCache", map[string]any{})
	// localStorage/sessionStorage/IndexedDB。origin 为空就跳过 —— 这个命令必须指定来源，
	// 没有来源可清时不该编一个。
	//
	// 除了站点自己，还必须清 challenges.cloudflare.com：Turnstile 把状态存在**它自己的**
	// 来源下，只清站点等于一次都没清过它。漏掉这一条时，同一个 profile 连着解六次挑战，
	// 前面失败的痕迹全留着，第七次照样不给 token —— 而现象和「这个出口被判高风险」一模一
	// 样，会把排查引到代理池上去。
	//
	// 站点 origin 清失败只降级（缓存和存储不像 cookie 那样直接把身份带过去），所以这里沿用
	// 忽略错误的处置；但两个来源都要清。
	origins := []string{strings.TrimSpace(origin), challengeOrigin}
	for _, o := range origins {
		if o == "" {
			continue
		}
		_, _ = s.call(ctx, "", "Storage.clearDataForOrigin",
			map[string]any{"origin": o, "storageTypes": "all"})
	}
	return nil
}

// challengeOrigin 是 Turnstile 自己的来源。它和注册站点的 origin 是两回事，清状态时两个
// 都要清 —— 见 clearProfileState。
const challengeOrigin = "https://challenges.cloudflare.com"

// closeRemote 把接管模式在手机上留下的东西销毁掉。
//
// 用独立的超时而不是外层 ctx：Close 常常是在外层 ctx 已经取消之后才调用的（求解超时、
// 注册失败都会走到这），拿一个已死的 ctx 去发命令会立刻失败，于是标签页永远留在手机上，
// 跑几百个号之后手机会被几百个空白页拖垮。
func (s *chromeSession) closeRemote() {
	if s == nil || s.conn == nil || s.remoteTargetID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": s.remoteTargetID})
}

// PhoneSession 是外部（注册侧）拿到的手机浏览器会话。
//
// 单独包一层是因为 chromeSession 是包内私有的，而注册侧要能先「接通手机」再「反复用它
// 解 Turnstile」—— 接通一次的成本是拉起浏览器加建转发，不该每个号付一次。
type PhoneSession struct {
	sess *chromeSession
	info phonecdp.Result
}

// AttachPhone 接通手机上的浏览器：确保转发和浏览器就位，开一个新标签页，并清掉上一个号的痕迹。
//
// origin 是即将访问的站点来源（如 https://office.965007.xyz），用来清该站的 localStorage
// 一类存储；留空则只清 cookie 和缓存。
func AttachPhone(ctx context.Context, cfg phonecdp.Config, origin string) (*PhoneSession, error) {
	info, err := phonecdp.EnsureCromite(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ver, err := phonecdp.BrowserWebSocket(ctx, info.BaseURL)
	if err != nil {
		return nil, err
	}
	sess, err := attachPhoneChrome(ctx, info.BaseURL, ver, origin)
	if err != nil {
		return nil, err
	}
	return &PhoneSession{sess: sess, info: info}, nil
}

// Describe 返回一句可以直接写进 Notes 的说明，用来在日志里确认「这一轮确实是手机在跑」。
func (p *PhoneSession) Describe() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("手机浏览器 %s（%s，出口为手机自身运营商 IP）在 %s 上，已清 cookie",
		p.info.Browser, p.info.Package, p.info.BaseURL)
}

// Close 关掉这一轮的标签页和上下文，但不动手机上的浏览器本身。
func (p *PhoneSession) Close() {
	if p == nil {
		return
	}
	p.sess.Close()
}
