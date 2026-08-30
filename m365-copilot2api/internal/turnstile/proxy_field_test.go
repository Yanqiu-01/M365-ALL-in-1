package turnstile

import "testing"

// 本包客户端按 FlareSolverr 规范发 {"proxy":{"url":...}}，而服务端一度用
// fmt.Sprint 去读这个字段，拿到的是 "map[url:socks5://...]" —— 同一个进程的两端
// 各说一套。这组用例把两种形状都钉住。
func TestProxyField(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "nested url as this package's own client sends it",
			payload: map[string]any{"proxy": map[string]any{"url": "socks5://127.0.0.1:1080"}},
			want:    "socks5://127.0.0.1:1080",
		},
		{
			name:    "plain string for clients that send the short form",
			payload: map[string]any{"proxy": "http://127.0.0.1:7897"},
			want:    "http://127.0.0.1:7897",
		},
		{
			name:    "surrounding whitespace is trimmed",
			payload: map[string]any{"proxy": map[string]any{"url": "  socks5://h:1  "}},
			want:    "socks5://h:1",
		},
		{name: "absent", payload: map[string]any{}, want: ""},
		{name: "explicit null", payload: map[string]any{"proxy": nil}, want: ""},
		{
			name:    "nested object without url",
			payload: map[string]any{"proxy": map[string]any{"username": "u"}},
			want:    "",
		},
		{
			name:    "nested url of the wrong type is not stringified",
			payload: map[string]any{"proxy": map[string]any{"url": 8191}},
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proxyField(c.payload); got != c.want {
				t.Errorf("proxyField() = %q, want %q", got, c.want)
			}
		})
	}
}

// Cancel 的返回值是调用方据以如实回话的依据：协作目录不可用时它什么也做不了。
func TestCancelReportsWhetherItSignalled(t *testing.T) {
	t.Setenv("M365_DATA_DIR", "")
	if Cancel() {
		t.Error("Cancel() must report false when there is nowhere to write the signal")
	}
	t.Setenv("M365_DATA_DIR", t.TempDir())
	if !Cancel() {
		t.Error("Cancel() must report true once the collaboration directory is usable")
	}
}
