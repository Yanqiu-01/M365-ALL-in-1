package turnstile

import (
	"encoding/json"
	"testing"
)

// Turnstile 把 challenge iframe 挂在闭合 shadow root 里。这是实测的形状：
//
//	DIV#turnstileBox > DIV > DIV > #document-fragment[shadow:closed] > IFRAME
//
// JS 里 document.querySelectorAll('iframe') 数出来是 0，el.shadowRoot 也拿不到（闭合），
// 只有 DOM.getDocument 带 pierce:true 能看到。之前用 box.querySelector('iframe') 判断
// 「控件有没有渲染出来」，于是在控件明明已经渲染、challenge 请求一路 200 的情况下报
// 「控件没有渲染出来」—— 诊断完全指错方向。
func TestFindChallengeIframePiercesClosedShadowRoot(t *testing.T) {
	doc := json.RawMessage(`{
      "nodeId": 1, "nodeName": "#document", "children": [
        {"nodeId": 2, "nodeName": "BODY", "children": [
          {"nodeId": 9, "nodeName": "IFRAME", "attributes": ["src", "https://ads.example/frame"]},
          {"nodeId": 217, "nodeName": "DIV", "attributes": ["id", "turnstileBox", "class", "turnstile-box"],
           "children": [
             {"nodeId": 218, "nodeName": "DIV", "children": [
               {"nodeId": 219, "nodeName": "DIV", "shadowRoots": [
                 {"nodeId": 220, "nodeName": "#document-fragment", "shadowRootType": "closed", "children": [
                   {"nodeId": 221, "nodeName": "IFRAME",
                    "attributes": ["src", "https://challenges.cloudflare.com/cdn-cgi/challenge-platform/h/b/turnstile/f/av0"]}
                 ]}
               ]},
               {"nodeId": 222, "nodeName": "INPUT", "attributes": ["type", "hidden", "name", "cf-turnstile-response"]}
             ]}
           ]}
        ]}
      ]}`)
	id, ok := findChallengeIframe(doc, false)
	if !ok {
		t.Fatal("must find the iframe inside the closed shadow root")
	}
	if id != 221 {
		t.Fatalf("nodeId = %d, want 221 (got the wrong iframe — 9 is an unrelated one outside the widget)", id)
	}
}

// 容器外的 iframe 不能被当成控件：页面上可能有广告、统计之类的帧。
func TestFindChallengeIframeIgnoresFramesOutsideWidget(t *testing.T) {
	doc := json.RawMessage(`{
      "nodeId": 1, "nodeName": "#document", "children": [
        {"nodeId": 9, "nodeName": "IFRAME", "attributes": ["src", "https://ads.example/frame"]},
        {"nodeId": 10, "nodeName": "DIV", "attributes": ["id", "somethingElse"]}
      ]}`)
	if _, ok := findChallengeIframe(doc, false); ok {
		t.Fatal("no widget in this tree, so nothing should match")
	}
}

// headless 是量出来的失败原因，默认必须是有头；同时保留一个显式开关，供没有交互式会话
// 的环境（session 0 的 Windows 服务）至少能把流程跑起来。
func TestChromeHeadlessDefaultsOff(t *testing.T) {
	t.Setenv("M365_CHROME_HEADLESS", "")
	if chromeHeadless() {
		t.Error("headless must not be the default: Cloudflare stalls it forever")
	}
	for _, on := range []string{"1", "true", "YES"} {
		t.Setenv("M365_CHROME_HEADLESS", on)
		if !chromeHeadless() {
			t.Errorf("M365_CHROME_HEADLESS=%q should enable headless", on)
		}
	}
	t.Setenv("M365_CHROME_HEADLESS", "0")
	if chromeHeadless() {
		t.Error("M365_CHROME_HEADLESS=0 must stay headful")
	}
}
