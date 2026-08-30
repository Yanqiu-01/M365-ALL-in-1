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
}

// Solution carries what the solver produced.
type Solution struct {
	Token     string `json:"token"`
	UserAgent string `json:"userAgent,omitempty"`
	Clearance string `json:"clearance,omitempty"`
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

	payload := map[string]any{
		"cmd":         "request.get",
		"url":         page,
		"maxTimeout":  budget.Milliseconds(),
		"displayName": strings.TrimSpace(request.DisplayName),
		"username":    strings.TrimSpace(request.Username),
		"password":    strings.TrimSpace(request.Password),
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
		return Solution{}, fmt.Errorf("FlareSolverr 求解失败: %s", message)
	}

	solution := Solution{UserAgent: strings.TrimSpace(reply.Solution.UserAgent)}
	for _, cookie := range reply.Solution.Cookies {
		if strings.EqualFold(strings.TrimSpace(cookie.Name), "cf_clearance") {
			solution.Clearance = strings.TrimSpace(cookie.Value)
		}
	}
	solution.Token = extractToken(reply.Solution.Response)
	if solution.Token == "" {
		return solution, errors.New("注册页已打开，但没有完成填表和提交")
	}
	return solution, nil
}

func extractToken(document string) string {
	for _, pattern := range tokenPatterns {
		if match := pattern.FindStringSubmatch(document); len(match) == 2 {
			if token := strings.TrimSpace(match[1]); token != "" {
				return token
			}
		}
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
