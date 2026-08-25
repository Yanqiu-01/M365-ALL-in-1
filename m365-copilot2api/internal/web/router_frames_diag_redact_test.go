package web

import (
	"strings"
	"testing"
	"time"
)

// runRedactWithDeadline 在独立 goroutine 里执行打码调用并设硬超时。
//
// 目的很具体：truncateRouterFrame 曾经在「一条字符串里出现 2 个以上 marker」
// 时死循环（原地重写后又从 index 0 重新搜索，重新插入的 marker 被反复命中），
// 一个 goroutine 直接吃满一个核。若回归，这里会 Fatal 报错，而不是把整个
// go test 二进制挂死等到 -timeout 才被打死。
func runRedactWithDeadline(t *testing.T, label string, fn func() string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- fn() }()
	select {
	case out := <-done:
		return out
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: 打码调用 2s 内未返回，疑似死循环（搜索游标没有前进）", label)
		return ""
	}
}

// 每个 marker 的每一次出现都要打码，且必须终止。
func TestTruncateRouterFrameRedactsEveryMarkerOccurrence(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		want        string
		mustNotHave []string
	}{
		{
			// 复现用例：两个 Authorization + 两个 Bearer，旧实现永不返回。
			name:        "two_authorization_bearer_pairs",
			input:       "Authorization: Bearer abc123 tail Authorization: Bearer def456",
			want:        "Authorization:[redacted] [redacted] tail Authorization:[redacted] [redacted]",
			mustNotHave: []string{"abc123", "def456"},
		},
		{
			name:        "two_bearer_tokens",
			input:       "Bearer abc123 and Bearer def456",
			want:        "Bearer [redacted] and Bearer [redacted]",
			mustNotHave: []string{"abc123", "def456"},
		},
		{
			name:        "three_query_style_tokens",
			input:       "access_token=aaa&accessToken=bbb&refresh_token=ccc",
			want:        "access_token=[redacted]&accessToken=[redacted]&refresh_token=[redacted]",
			mustNotHave: []string{"aaa", "bbb", "ccc"},
		},
		{
			name:        "mixed_case_bearer_twice",
			input:       "bearer aaa, bearer bbb",
			want:        "bearer [redacted], bearer [redacted]",
			mustNotHave: []string{"aaa", "bbb"},
		},
		{
			name:        "single_marker_still_redacted",
			input:       "Bearer abc123",
			want:        "Bearer [redacted]",
			mustNotHave: []string{"abc123"},
		},
		{
			name:  "no_marker_only_trim_and_cr_strip",
			input: "  plain diagnostic\r\nsecond line  ",
			want:  "plain diagnostic\nsecond line",
		},
		{
			name:  "empty_input",
			input: "   ",
			want:  "",
		},
		{
			// marker 落在串尾、后面没有值：不能 panic、不能挂住。
			name:  "authorization_marker_at_end",
			input: "context Authorization:",
			want:  "context Authorization:[redacted]",
		},
		{
			name:  "access_token_marker_at_end",
			input: "context access_token=",
			want:  "context access_token=[redacted]",
		},
		{
			// TrimSpace 先吃掉尾部空格，"Bearer " 因此不再匹配，原样保留。
			name:  "bearer_marker_at_end_without_value",
			input: "context Bearer ",
			want:  "context Bearer",
		},
		{
			// 第二个 marker 落在第一个的取值区间内，被整段吞掉。
			name:  "two_bare_authorization_markers",
			input: "Authorization: Authorization:",
			want:  "Authorization:[redacted]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRedactWithDeadline(t, tc.name, func() string {
				return truncateRouterFrame(tc.input)
			})
			if got != tc.want {
				t.Fatalf("truncateRouterFrame(%q) = %q, want %q", tc.input, got, tc.want)
			}
			for _, secret := range tc.mustNotHave {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q 泄漏到输出：%q", secret, got)
				}
			}
		})
	}
}

// 4096 字节上限与 "…" 后缀在打码之后仍然生效。
func TestTruncateRouterFrameRedactionKeepsByteCap(t *testing.T) {
	const limit = 4096
	cases := []struct {
		name  string
		input string
	}{
		{name: "plain_overlong", input: strings.Repeat("x", limit+500)},
		{
			name:  "overlong_with_many_markers",
			input: strings.Repeat("Bearer abcdefghij ", 400),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRedactWithDeadline(t, tc.name, func() string {
				return truncateRouterFrame(tc.input)
			})
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("截断结果缺少 … 后缀：末尾 %q", got[max(0, len(got)-16):])
			}
			if want := limit + len("…"); len(got) != want {
				t.Fatalf("len=%d, want %d", len(got), want)
			}
			if strings.Contains(got[:limit], "abcdefghij") {
				t.Fatalf("secret 泄漏到截断结果：%q", got[:64])
			}
		})
	}
}

// 短输入下未截断，且不会误加 … 后缀。
func TestTruncateRouterFrameRedactionUnderCapNotTruncated(t *testing.T) {
	got := runRedactWithDeadline(t, "under_cap", func() string {
		return truncateRouterFrame(strings.Repeat("y", 4096))
	})
	if len(got) != 4096 || strings.HasSuffix(got, "…") {
		t.Fatalf("len=%d suffix=%v", len(got), strings.HasSuffix(got, "…"))
	}
}
