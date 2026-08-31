// chrome.go 用本机 Chrome/Edge 通过 CDP 求解 Turnstile。
//
// 为什么不能只靠 FlareSolverr：这个注册页的 Turnstile 是 explicit render —— app.js
// 先取 /api/public/site-config，再往 head 里插 challenges.cloudflare.com 的 api.js，
// 在 script.onload 里才 window.turnstile.render()。FlareSolverr 3.5.0 的 request.get
// 只能返回 driver.page_source 的一张快照，而 Turnstile 拿到 token 后是给隐藏 input 赋
// value 属性（DOM property），序列化出来的 HTML 里根本没有 value=... —— 实测过：直连
// 时快照里能看到 <input type="hidden" name="cf-turnstile-response" ...> 但没有 value，
// 走代理时连这个 input 都来不及出现。所以无论等多久，正则都取不到 token。
//
// 结论是 PC 上必须有一个能读 DOM property、能点击、能提交的真浏览器。本机已经装了
// Chrome 和 Edge，CDP 走 WebSocket，而 gorilla/websocket 本来就在依赖里，所以不引入
// 任何新模块。
package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// chromeCandidates 列出本机可能的浏览器位置。M365_CHROME_PATH 优先，方便用户指定。
func chromeCandidates(goos string) []string {
	if custom := strings.TrimSpace(os.Getenv("M365_CHROME_PATH")); custom != "" {
		return []string{custom}
	}
	switch goos {
	case "windows":
		var out []string
		for _, root := range []string{
			os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData"),
		} {
			if strings.TrimSpace(root) == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(root, `Chromium\Application\chrome.exe`),
			)
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium",
			"/usr/bin/chromium-browser", "/usr/bin/microsoft-edge", "/snap/bin/chromium",
		}
	}
}

// findChrome 返回本机第一个可执行的浏览器路径。
func findChrome() (string, error) {
	var tried []string
	for _, candidate := range chromeCandidates(hostOS()) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		tried = append(tried, candidate)
	}
	// PATH 兜底：Linux 容器里路径常常不在上面的清单里。
	for _, name := range []string{"google-chrome", "chromium", "microsoft-edge", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"没找到 Chrome/Edge，无法在本机求解 Turnstile。请安装 Chrome 或 Edge，"+
			"或把 M365_CHROME_PATH 指向浏览器可执行文件（已尝试：%s）", strings.Join(tried, "; "))
}

// ChromeAvailable 报告本机是否具备 CDP 求解能力。
func ChromeAvailable() bool { _, err := findChrome(); return err == nil }

// chromeHeadless 报告是否用 headless 跑。默认不用。
//
// 这是量出来的，不是偏好：同一份代码、同一个出口，headless=new 下 Turnstile 会一直
// 转圈 —— challenge iframe 起得来，它自己的 POST /cdn-cgi/challenge-platform/h/b/fo/
// 也一路回 200，blob worker 反复重启，但 token 永远不出现，点击也没用（Cloudflare 不
// 是拒绝，是拖着）。换成有头（窗口挪到屏幕外），两个不同出口都在 12~13 秒内自动拿到
// 794 字节的 token，连点都不用点。UA 里的 HeadlessChrome 记号已经改掉了，所以差别不在
// UA，而在 headless 本身暴露的那一堆信号。
//
// 代价是它需要一个能建窗口的会话：本程序跑在交互式会话里（session 1）没问题，真做成
// session 0 的 Windows 服务就起不来 —— 那时 M365_CHROME_HEADLESS=1 至少能让流程跑起
// 来，尽管大概率解不出 token。
func chromeHeadless() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("M365_CHROME_HEADLESS"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// chromeSession 是一条连到浏览器的 CDP 连接，外加一个已 attach 的页面 session。
type chromeSession struct {
	cmd       *exec.Cmd
	userDir   string
	conn      *websocket.Conn
	sessionID string

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan cdpReply
	// events 按方法名派发。handler 在 reader goroutine 里跑，因此它自己不能等
	// 另一条回包 —— 需要发命令的 handler 必须自己开 goroutine。
	events map[string]func(json.RawMessage)

	writeMu sync.Mutex
	closed  chan struct{}
	readErr error
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

type cdpFrame struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// launchChrome 起一个一次性浏览器实例并 attach 到一个空白页。
//
// 每次都用全新的 user-data-dir：Turnstile 会看 cookie 和历史，复用配置目录会让上一
// 个号的痕迹带到下一个号；而且并发/残留的 profile 锁会让启动直接失败。
func launchChrome(ctx context.Context, proxyURL string, headless bool) (*chromeSession, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	userDir, err := os.MkdirTemp("", "m365-turnstile-")
	if err != nil {
		return nil, err
	}
	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + userDir,
		"--no-first-run", "--no-default-browser-check", "--disable-sync",
		"--disable-background-networking", "--disable-component-update",
		"--disable-extensions", "--disable-default-apps",
		"--no-service-autorun", "--password-store=basic", "--use-mock-keychain",
		// navigator.webdriver 由 --enable-automation 置真，而那是 Turnstile 会看的
		// 信号之一。这里刻意不带那个开关，并显式关掉对应的 blink feature。
		"--disable-blink-features=AutomationControlled",
		"--window-size=1280,900",
		"--lang=zh-CN",
	}
	if headless {
		// 新版 headless 跑的是完整 Chrome 渲染栈，比老 headless 难被识别 —— 但对
		// Turnstile 仍然不够，见 chromeHeadless 的注释。
		args = append(args, "--headless=new", "--disable-gpu")
	} else {
		// 有头但挪到屏幕外：注册跑在后台，不该在用户屏幕上弹窗。
		args = append(args, "--window-position=-32000,-32000")
	}
	if p := strings.TrimSpace(proxyURL); p != "" {
		args = append(args, "--proxy-server="+chromeProxyArg(p))
	}
	args = append(args, "about:blank")

	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		os.RemoveAll(userDir)
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	session := &chromeSession{
		cmd: cmd, userDir: userDir,
		pending: map[int64]chan cdpReply{},
		events:  map[string]func(json.RawMessage){},
		closed:  make(chan struct{}),
	}
	wsURL, err := waitDevToolsWS(ctx, userDir)
	if err != nil {
		session.Close()
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, ReadBufferSize: 1 << 20, WriteBufferSize: 1 << 20}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("连接浏览器调试端口失败: %w", err)
	}
	conn.SetReadLimit(64 << 20)
	session.conn = conn
	go session.readLoop()

	target, err := session.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		session.Close()
		return nil, err
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	_ = json.Unmarshal(target, &created)
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
		return nil, errors.New("浏览器没有返回调试会话 ID")
	}
	session.sessionID = att.SessionID
	for _, domain := range []string{"Page.enable", "Runtime.enable", "DOM.enable", "Network.enable"} {
		if _, err := session.call(ctx, att.SessionID, domain, map[string]any{}); err != nil {
			session.Close()
			return nil, err
		}
	}
	if err := session.maskHeadless(ctx); err != nil {
		session.Close()
		return nil, err
	}
	if err := session.setupProxyAuth(ctx, proxyURL); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

// chromeProxyArg 把池子里的 URL 转成 --proxy-server 认的形式。
//
// Chrome 不接受 URL 里的 user:pass（那部分会被忽略，随后浏览器弹认证框），所以这里
// 只留 scheme://host:port，凭据交给 Fetch.continueWithAuth。socks5h 是 curl 的写法，
// Chrome 只认 socks5，而 Chrome 的 socks5 本来就在代理端做 DNS，语义一致。
func chromeProxyArg(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5h", "socks5":
		scheme = "socks5"
	case "":
		scheme = "http"
	}
	return scheme + "://" + parsed.Host
}

// setupProxyAuth 只在代理带凭据时开 Fetch 拦截。
//
// Fetch.enable 会让每个请求都要显式 continue，这对页面加载是实打实的开销，所以开放
// 代理不开它。
func (s *chromeSession) setupProxyAuth(ctx context.Context, proxyURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsed == nil || parsed.User == nil {
		return nil
	}
	username := parsed.User.Username()
	password, _ := parsed.User.Password()
	if username == "" && password == "" {
		return nil
	}
	sessionID := s.sessionID
	s.on("Fetch.requestPaused", func(params json.RawMessage) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(params, &p)
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = s.call(cctx, sessionID, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
		}()
	})
	s.on("Fetch.authRequired", func(params json.RawMessage) {
		var p struct {
			RequestID     string `json:"requestId"`
			AuthChallenge struct {
				Source string `json:"source"`
			} `json:"authChallenge"`
		}
		_ = json.Unmarshal(params, &p)
		response := map[string]any{"response": "ProvideCredentials", "username": username, "password": password}
		if !strings.EqualFold(p.AuthChallenge.Source, "Proxy") {
			// 站点自己要认证时不要把代理凭据递出去。
			response = map[string]any{"response": "CancelAuth"}
		}
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = s.call(cctx, sessionID, "Fetch.continueWithAuth",
				map[string]any{"requestId": p.RequestID, "authChallengeResponse": response})
		}()
	})
	_, err = s.call(ctx, sessionID, "Fetch.enable", map[string]any{"handleAuthRequests": true})
	return err
}

// maskHeadless 去掉 UA 里的 Headless 记号。
//
// headless=new 报的是 "HeadlessChrome/152.0.0.0"，Turnstile 会看这个字符串 —— 实测下
// 它把 widget 的壳和隐藏 input 都建好了，却始终不插 challenge iframe，页面上没有任何
// 报错。UA 换成普通 Chrome 之后才继续往下走。
//
// 顺带用 Page.addScriptToEvaluateOnNewDocument 把 navigator.webdriver 摁掉：这里没带
// --enable-automation，正常情况下它本来就是 false，但换过 UA 之后两者要一致，不然反而
// 是个更明显的矛盾信号。
func (s *chromeSession) maskHeadless(ctx context.Context) error {
	var ua string
	if err := s.eval(ctx, "navigator.userAgent", &ua); err != nil {
		return err
	}
	cleaned := strings.ReplaceAll(ua, "HeadlessChrome/", "Chrome/")
	if cleaned == ua {
		return nil
	}
	if _, err := s.call(ctx, s.sessionID, "Network.setUserAgentOverride", map[string]any{
		"userAgent":      cleaned,
		"acceptLanguage": "zh-CN,zh;q=0.9",
		"platform":       "Win32",
	}); err != nil {
		return err
	}
	_, err := s.call(ctx, s.sessionID, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": `Object.defineProperty(navigator, 'webdriver', {get: () => false});`,
	})
	return err
}

func (s *chromeSession) on(method string, handler func(json.RawMessage)) {
	s.mu.Lock()
	s.events[method] = handler
	s.mu.Unlock()
}

func (s *chromeSession) readLoop() {
	defer close(s.closed)
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			s.readErr = err
			for id, ch := range s.pending {
				ch <- cdpReply{err: err}
				delete(s.pending, id)
			}
			s.mu.Unlock()
			return
		}
		var frame cdpFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame.ID != 0 {
			s.mu.Lock()
			ch, ok := s.pending[frame.ID]
			delete(s.pending, frame.ID)
			s.mu.Unlock()
			if !ok {
				continue
			}
			if frame.Error != nil {
				ch <- cdpReply{err: fmt.Errorf("CDP %s: %s", strconv.Itoa(frame.Error.Code), frame.Error.Message)}
				continue
			}
			ch <- cdpReply{result: frame.Result}
			continue
		}
		if frame.Method == "" {
			continue
		}
		s.mu.Lock()
		handler := s.events[frame.Method]
		s.mu.Unlock()
		if handler != nil {
			handler(frame.Params)
		}
	}
}

// call 发一条 CDP 命令并等回包。sessionID 为空表示发给 browser 级别。
func (s *chromeSession) call(ctx context.Context, sessionID, method string, params map[string]any) (json.RawMessage, error) {
	if s.conn == nil {
		return nil, errors.New("浏览器调试连接未建立")
	}
	s.mu.Lock()
	if s.readErr != nil {
		err := s.readErr
		s.mu.Unlock()
		return nil, err
	}
	s.nextID++
	id := s.nextID
	ch := make(chan cdpReply, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	frame := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.conn.SetWriteDeadline(deadline)
	err = s.conn.WriteMessage(websocket.TextMessage, body)
	s.writeMu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-s.closed:
		return nil, errors.New("浏览器调试连接已断开")
	}
}

// eval 在页面里跑一段表达式并把结果反序列化到 out。
func (s *chromeSession) eval(ctx context.Context, expression string, out any) error {
	raw, err := s.call(ctx, s.sessionID, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return err
	}
	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return err
	}
	if reply.ExceptionDetails != nil {
		msg := reply.ExceptionDetails.Text
		if reply.ExceptionDetails.Exception != nil && reply.ExceptionDetails.Exception.Description != "" {
			msg = reply.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("页面脚本报错: %s", msg)
	}
	if out == nil || len(reply.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(reply.Result.Value, out)
}

// navigate 打开页面并等到 document.readyState 为 complete。
func (s *chromeSession) navigate(ctx context.Context, page string) error {
	if _, err := s.call(ctx, s.sessionID, "Page.navigate", map[string]any{"url": page}); err != nil {
		return err
	}
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for time.Now().Before(deadline) {
		var state string
		if err := s.eval(ctx, "document.readyState", &state); err == nil && state == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return errors.New("注册页在预算内没有加载完成")
}

// clickAt 派发一次真实的鼠标点击。CDP 的 Input 事件带 isTrusted=true，这是
// Turnstile 会看的信号 —— 用 element.click() 或 dispatchEvent 造的事件不算。
//
// 事件发给 browser 级 session，不是页面 session：Turnstile 的 challenge iframe 是跨站
// 的，站点隔离下它跑在独立渲染进程里（OOPIF）。发给页面 session 的事件只到页面那个进
// 程，进不到子帧 —— 表现成「点了，控件毫无反应，截图前后字节完全一样」。browser 级走
// 的是完整输入管线，带对子帧的命中测试。
func (s *chromeSession) clickAt(ctx context.Context, x, y float64) error {
	return s.clickAtSession(ctx, "", x, y)
}

func (s *chromeSession) clickAtSession(ctx context.Context, sessionID string, x, y float64) error {
	for _, eventType := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		params := map[string]any{
			"type": eventType, "x": x, "y": y, "button": "left", "clickCount": 1,
		}
		if eventType == "mouseMoved" {
			params["button"] = "none"
			params["clickCount"] = 0
		}
		if _, err := s.call(ctx, sessionID, "Input.dispatchMouseEvent", params); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
	}
	return nil
}

// challengeIframeRect 给出 challenge iframe 在视口里的真实位置。
//
// 这个 iframe 挂在闭合 shadow root 里，JS 查不到（document.querySelectorAll('iframe')
// 是 0），只有 DOM.getDocument 带 pierce:true 能看到它。容器的 rect 是 454 宽而 iframe
// 只有 300 宽，两者不能混用 —— 按容器算坐标会偏。
func (s *chromeSession) challengeIframeRect(ctx context.Context) (widgetRect, error) {
	raw, err := s.call(ctx, s.sessionID, "DOM.getDocument", map[string]any{"depth": -1, "pierce": true})
	if err != nil {
		return widgetRect{}, err
	}
	var doc struct {
		Root json.RawMessage `json:"root"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return widgetRect{}, err
	}
	nodeID, ok := findChallengeIframe(doc.Root, false)
	if !ok {
		return widgetRect{}, errors.New("没找到 Turnstile 的 challenge iframe")
	}
	boxRaw, err := s.call(ctx, s.sessionID, "DOM.getBoxModel", map[string]any{"nodeId": nodeID})
	if err != nil {
		return widgetRect{}, err
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
			Width   float64   `json:"width"`
			Height  float64   `json:"height"`
		} `json:"model"`
	}
	if err := json.Unmarshal(boxRaw, &box); err != nil {
		return widgetRect{}, err
	}
	if len(box.Model.Content) < 8 {
		return widgetRect{}, errors.New("challenge iframe 没有可用的布局信息")
	}
	// content 是四个角的坐标：x1,y1,x2,y2,x3,y3,x4,y4。
	return widgetRect{
		OK: true, X: box.Model.Content[0], Y: box.Model.Content[1],
		Width: box.Model.Width, Height: box.Model.Height,
	}, nil
}

type domNode struct {
	NodeID      int               `json:"nodeId"`
	NodeName    string            `json:"nodeName"`
	Attributes  []string          `json:"attributes"`
	Children    []json.RawMessage `json:"children"`
	ShadowRoots []json.RawMessage `json:"shadowRoots"`
}

// findChallengeIframe 在 #turnstileBox 子树里找那个 iframe。
//
// 只在容器内部找，避免把页面上别的 iframe（广告、统计之类）当成控件。
func findChallengeIframe(raw json.RawMessage, inWidget bool) (int, bool) {
	var node domNode
	if json.Unmarshal(raw, &node) != nil {
		return 0, false
	}
	attrs := strings.Join(node.Attributes, " ")
	if !inWidget && strings.Contains(attrs, "turnstileBox") {
		inWidget = true
	}
	if inWidget && strings.EqualFold(node.NodeName, "IFRAME") {
		return node.NodeID, true
	}
	for _, child := range node.Children {
		if id, ok := findChallengeIframe(child, inWidget); ok {
			return id, true
		}
	}
	for _, shadow := range node.ShadowRoots {
		if id, ok := findChallengeIframe(shadow, inWidget); ok {
			return id, true
		}
	}
	return 0, false
}

// Close 关掉浏览器并清掉一次性 profile。
func (s *chromeSession) Close() {
	if s == nil {
		return
	}
	if s.conn != nil {
		s.writeMu.Lock()
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		s.writeMu.Unlock()
		_ = s.conn.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	if s.userDir != "" {
		// Windows 上刚 Kill 的进程可能还占着文件，重试几次再放手。
		for i := 0; i < 5; i++ {
			if err := os.RemoveAll(s.userDir); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// waitDevToolsWS 等 Chrome 写下 DevToolsActivePort，再问它 webSocketDebuggerUrl。
//
// 用 --remote-debugging-port=0 让系统分配端口：写死端口在批量注册里会撞车。
func waitDevToolsWS(ctx context.Context, userDir string) (string, error) {
	portFile := filepath.Join(userDir, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var port string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
				port = strings.TrimSpace(lines[0])
				break
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if port == "" {
		return "", errors.New("浏览器没有在预算内开出调试端口")
	}
	// /json/version 只走本机回环，不能被代理拦下 —— 用一个显式不带代理的客户端。
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1:" + port + "/json/version")
		if err == nil {
			var payload struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if err == nil && strings.TrimSpace(payload.WebSocketDebuggerURL) != "" {
				return payload.WebSocketDebuggerURL, nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("读浏览器调试地址失败: %w", lastErr)
	}
	return "", errors.New("读浏览器调试地址超时")
}

var _ = runtime.GOOS
