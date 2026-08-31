package turnstile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这个注册页的控件是 explicit render：app.js 先取 /api/public/site-config，再插
// challenges.cloudflare.com 的 api.js，在 onload 里才 render。不等就快照，页面里连隐藏
// input 都还没出现 —— 实测直连等 18 秒能看到那个 input，走代理时 api.js 的 302 加正文
// 就要 9 秒以上。所以请求里必须带上等待时间。
func TestSolveSendsWaitInSeconds(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": `<input name="cf-turnstile-response" value="tok_aaaaaaaaaaaaaaaaaaaaaa">`},
		})
	}))
	defer server.Close()

	if _, err := Solve(context.Background(), Request{
		Endpoint: server.URL, PageURL: "https://office.example.test/", Timeout: 90 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	wait, ok := got["waitInSeconds"].(float64)
	if !ok || wait <= 0 {
		t.Fatalf("waitInSeconds missing or non-positive: %#v", got["waitInSeconds"])
	}
	if int(wait) != int(DefaultWait/time.Second) {
		t.Errorf("waitInSeconds = %v, want %v", wait, DefaultWait/time.Second)
	}
}

// 等待占的是同一份预算。预算本身就短的时候还硬等，服务端会先被 maxTimeout 掐断，
// 连快照都拿不到 —— 那比不等更糟。
func TestSolveDropsWaitWhenBudgetIsTight(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": `<input name="cf-turnstile-response" value="tok_aaaaaaaaaaaaaaaaaaaaaa">`},
		})
	}))
	defer server.Close()

	if _, err := Solve(context.Background(), Request{
		Endpoint: server.URL, PageURL: "https://office.example.test/", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if _, present := got["waitInSeconds"]; present {
		t.Errorf("a 5s budget must not also ask the server to wait: %#v", got["waitInSeconds"])
	}
}

// 本机镜像会在 solution.turnstile_token 上直接给出 token：那是从 DOM property 上读的，
// 比从快照 HTML 里正则可靠 —— Turnstile 是给 input.value 赋值，序列化后看不到。
func TestSolvePrefersServerSuppliedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"solution": map[string]any{
				"turnstile_token": "token-from-dom-property",
				"response":        `<input name="cf-turnstile-response" value="stale-token-from-html">`,
			},
		})
	}))
	defer server.Close()

	got, err := Solve(context.Background(), Request{Endpoint: server.URL, PageURL: "https://office.example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "token-from-dom-property" {
		t.Fatalf("Token = %q", got.Token)
	}
}

// 拿不到 token 时的那句话，原来是「注册页已打开，但没有完成填表和提交」—— 把原因推给
// 填表，用户会一直去查字段有没有填对。真实原因是结构性的，必须说清楚，并给出出路。
func TestSolveNoTokenExplainsTheRealReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": `<div id="turnstileBox" class="turnstile-box"></div>`},
		})
	}))
	defer server.Close()

	_, err := Solve(context.Background(), Request{Endpoint: server.URL, PageURL: "https://office.example.test/"})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"快照", "value property", "Chrome"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, "没有完成填表和提交") {
		t.Errorf("still blames form filling: %q", msg)
	}
}
