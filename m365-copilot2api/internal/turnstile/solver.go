// Package turnstile talks to a local FlareSolverr instance so registration can
// obtain a Cloudflare Turnstile token without a human tapping the widget.
//
// The APK embeds a FlareSolverr-compatible endpoint on 127.0.0.1:8191. The
// browser backend is an off-screen WebView: it fills the register form, scrolls
// the Turnstile widget into its own viewport, and reads the token. Tokens are
// single-use and short lived, so callers solve once per account and never
// persist the value.
package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"m365-copilot2api/internal/outbound"
)

// DefaultEndpoint is where the built-in solver listens on the same host.
const DefaultEndpoint = "http://127.0.0.1:8191/v1"

// DefaultTimeout bounds a single solve attempt.
const DefaultTimeout = 90 * time.Second

// Request describes one solve attempt.
type Request struct {
	Endpoint    string        // FlareSolverr /v1 endpoint
	PageURL     string        // page that renders the Turnstile widget
	Proxy       string        // egress proxy FlareSolverr should use, optional
	Timeout     time.Duration // overall budget
	DisplayName string        // filled into #displayName
	Username    string        // filled into #username
	Password    string        // filled into #password
	// Wait 让服务端在快照之前多等一会儿。这个注册页的控件是 explicit render：
	// app.js 要先取 site-config，再加载 challenges.cloudflare.com 的 api.js，之后
	// 才 render。不等的话快照里连隐藏 input 都还没出现。
	Wait time.Duration
}

// DefaultWait 是快照前的等待时长。
//
// 实测：直连、等 18 秒时快照里能看到 Turnstile 注入的隐藏 input；走代理时 api.js
// 的 302 + 正文要 9 秒以上，等不够就只剩一个空的 #turnstileBox。
const DefaultWait = 20 * time.Second

// Solution carries what the solver produced.
type Solution struct {
	Token     string `json:"token"`
	UserAgent string `json:"userAgent,omitempty"`
	Clearance string `json:"clearance,omitempty"`
	// Exit 一句话说明这一轮的页面是在哪儿加载的（本机浏览器还是手机上的 Cromite）。
	//
	// 值得回给调用方：手机模式下「出口」不再是某个代理地址，而是手机自己的运营商 IP，
	// 日志里只看代理字段会看不出区别 —— 而「以为在手机上跑、其实在本机跑」正是会把家里
	// 的 IP 暴露给站点的那种错，必须能一眼看出来。失败时也要带上，不然最需要它的时候没有。
	Exit string `json:"exit,omitempty"`
}

type flareCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type flareSolution struct {
	URL       string        `json:"url"`
	Status    int           `json:"status"`
	Response  string        `json:"response"`
	UserAgent string        `json:"userAgent"`
	Cookies   []flareCookie `json:"cookies"`
	// TurnstileToken 是本机这个 FlareSolverr 镜像的扩展字段（上游 3.5.0 没有）。
	// 它靠 tabs_till_verify 按 Tab/Space 点控件后从 input 上读，能读到就直接用 ——
	// 那是 DOM property，比从快照 HTML 里正则更可靠。
	TurnstileToken string `json:"turnstile_token"`
}

type flareReply struct {
	Status   string        `json:"status"`
	Message  string        `json:"message"`
	Solution flareSolution `json:"solution"`
}

var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<input[^>]*name=["']cf-turnstile-response["'][^>]*value=["']([^"']{20,})["']`),
	regexp.MustCompile(`(?is)<input[^>]*value=["']([^"']{20,})["'][^>]*name=["']cf-turnstile-response["']`),
}

// Available reports whether an endpoint is configured.
func Available(endpoint string) bool {
	return strings.TrimSpace(endpoint) != ""
}

// Solve asks FlareSolverr to render the page and returns the Turnstile token.
func Solve(ctx context.Context, request Request) (Solution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := strings.TrimSpace(request.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	page := strings.TrimSpace(request.PageURL)
	if page == "" {
		return Solution{}, errors.New("缺少注册页地址，无法请求 FlareSolverr")
	}
	budget := request.Timeout
	if budget <= 0 {
		budget = DefaultTimeout
	}

	wait := request.Wait
	if wait <= 0 {
		wait = DefaultWait
	}
	// 等待占用的是同一份预算，留不出等待时间就别等 —— 否则服务端会先被 maxTimeout
	// 掐断，连快照都拿不到。
	if wait >= budget-10*time.Second {
		wait = 0
	}
	payload := map[string]any{
		"cmd":         "request.get",
		"url":         page,
		"maxTimeout":  budget.Milliseconds(),
		"displayName": strings.TrimSpace(request.DisplayName),
		"username":    strings.TrimSpace(request.Username),
		"password":    strings.TrimSpace(request.Password),
	}
	if seconds := int(wait / time.Second); seconds > 0 {
		payload["waitInSeconds"] = seconds
	}
	if proxy := strings.TrimSpace(request.Proxy); proxy != "" {
		payload["proxy"] = map[string]any{"url": proxy}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Solution{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, budget+15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Solution{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: budget + 15*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Solution{}, fmt.Errorf("FlareSolverr 不可用 (%s): %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Solution{}, err
	}
	var reply flareReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Solution{}, fmt.Errorf("FlareSolverr 返回无法解析: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(reply.Status), "ok") {
		message := strings.TrimSpace(reply.Message)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return Solution{}, errors.New(message)
	}

	solution := Solution{UserAgent: strings.TrimSpace(reply.Solution.UserAgent)}
	for _, cookie := range reply.Solution.Cookies {
		if strings.EqualFold(strings.TrimSpace(cookie.Name), "cf_clearance") {
			solution.Clearance = strings.TrimSpace(cookie.Value)
		}
	}
	// 服务端直接给出 token 时优先用它：那是从 DOM property 上读的，而快照 HTML 里
	// 只有属性 —— Turnstile 是给 input.value 赋值，序列化出来看不到。
	if direct := strings.TrimSpace(reply.Solution.TurnstileToken); direct != "" {
		solution.Token = direct
		return solution, nil
	}
	solution.Token = extractToken(reply.Solution.Response)
	if solution.Token == "" {
		return solution, errors.New(flareNoTokenAdvice)
	}
	return solution, nil
}

// flareNoTokenAdvice 说明 FlareSolverr 拿不到 token 的真实原因和出路。
//
// 原来的「注册页已打开，但没有完成填表和提交」把责任推给了填表，让人以为是字段没
// 填对。实际原因是结构性的：request.get 只回一张 driver.page_source 快照，而
// Turnstile 是给隐藏 input 赋 value property，序列化的 HTML 里没有这个值；快照里
// 也不可能有「点过控件」这件事。所以这条路在这个站点上永远取不到 token，正确的出路
// 是走本机浏览器。
const flareNoTokenAdvice = "FlareSolverr 只能返回页面快照，取不到 Turnstile token：" +
	"控件把 token 写在隐藏 input 的 value property 上，序列化后的 HTML 里没有它，" +
	"而 FlareSolverr 也不会替你点控件、提交表单。" +
	"请让本机装上 Chrome 或 Edge（或把 M365_CHROME_PATH 指向浏览器），注册会自动改走本机浏览器完成"

func extractToken(document string) string {
	raw := strings.TrimSpace(document)
	if strings.HasPrefix(raw, "SUBMITTED:") || strings.HasPrefix(raw, "ERROR:") {
		return raw
	}
	for _, pattern := range tokenPatterns {
		if match := pattern.FindStringSubmatch(document); len(match) == 2 {
			if token := strings.TrimSpace(match[1]); token != "" {
				return token
			}
		}
	}
	if i := strings.Index(document, "SUBMITTED:"); i >= 0 {
		end := strings.IndexAny(document[i:], "\"'<> \n")
		if end < 0 {
			return strings.TrimSpace(document[i:])
		}
		return strings.TrimSpace(document[i : i+end])
	}
	return ""
}

// ProbeProxyClient is exposed so callers can reuse the shared outbound stack
// when they need to verify the endpoint by hand.
func ProbeProxyClient(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return http.DefaultClient, nil
	}
	clients, err := outbound.New(proxyURL)
	if err != nil {
		return nil, err
	}
	if clients == nil || clients.HTTP == nil {
		return http.DefaultClient, nil
	}
	return clients.HTTP, nil
}
