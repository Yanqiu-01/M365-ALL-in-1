package chathub

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// 畸形 data URL 不得让 isImageURL panic。
//
// 修复前这里是 strings.SplitN(s, ",", 2)[1]：SplitN 只在存在分隔符时才返回两段，
// 一个没有逗号的 "data:image/png;base64" 会让索引 [1] 越界。这不是纸上风险 ——
// isImageURL 的输入来自 imageURLs，而 imageURLs 扫的是上游 SignalR 帧里任何
// url / src / value / data 字段，内容完全由上游决定。
func TestIsImageURLRejectsMalformedDataURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"没有逗号", "data:image/png;base64"},
		{"只有前缀", "data:image/"},
		{"前缀加分号无逗号", "data:image/jpeg;base64;"},
		{"逗号后为空", "data:image/png;base64,"},
		{"非法 base64", "data:image/png;base64,!!!not-base64!!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// panic 会直接让本用例失败，不需要额外的 recover 断言。
			if isImageURL(tc.in) {
				t.Errorf("isImageURL(%q)=true want false", tc.in)
			}
		})
	}
}

// 合法的 data URL 仍然要被认出来。
func TestIsImageURLAcceptsValidDataURL(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("fake-png-bytes"))
	if !isImageURL("data:image/png;base64," + payload) {
		t.Error("合法 data URL 被拒绝")
	}
}

// 端到端：一帧里带畸形 data URL 时，imageURLs 必须正常返回而不是让整个请求崩掉。
//
// imageURLs 跑在 chatWithHandlers 的完成帧处理里，panic 会顺着 HTTP 处理链炸上去。
func TestImageURLsSurvivesMalformedDataURL(t *testing.T) {
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("ok"))
	raw := []json.RawMessage{
		json.RawMessage(`{"type":1,"target":"update","arguments":[{"messages":[{"url":"data:image/png;base64"}]}]}`),
		json.RawMessage(`{"src":"data:image/"}`),
		json.RawMessage(`{"imageUrl":"data:image/gif;base64"}`),
		json.RawMessage(`{"data":"` + valid + `"}`),
	}
	got := imageURLs(raw)
	if len(got) != 1 || got[0] != valid {
		t.Fatalf("imageURLs=%v want 仅 %q", got, valid)
	}
}
