package web

import (
	"strings"
	"testing"
	"time"
)

// sanitizeDiagnosticError 必须打码「每一次」出现的凭据。
//
// 旧实现用 if strings.Index(...) 只处理第一次命中，第二个 token 会原样写进
// stage 日志。表里的多 token 用例正是这条泄漏的回归防线。
func TestSanitizeDiagnosticErrorRedactsAllOccurrences(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		want        string
		mustNotHave []string
	}{
		{
			name:        "two_bearer_tokens",
			input:       "post failed: Bearer abc123 retry: Bearer def456",
			want:        "post failed: Bearer [redacted] retry: Bearer [redacted]",
			mustNotHave: []string{"abc123", "def456"},
		},
		{
			name:        "two_authorization_headers",
			input:       "Authorization: Bearer abc123\nAuthorization: Bearer def456",
			want:        "Authorization:[redacted] Bearer [redacted]\nAuthorization:[redacted] Bearer [redacted]",
			mustNotHave: []string{"abc123", "def456"},
		},
		{
			name:        "repeated_query_tokens",
			input:       "url?access_token=aaa&accessToken=bbb&access_token=ccc",
			want:        "url?access_token=[redacted]&accessToken=[redacted]&access_token=[redacted]",
			mustNotHave: []string{"aaa", "bbb", "ccc"},
		},
		{
			name:  "single_marker_still_redacted",
			input: "dial: Bearer abc123",
			want:  "dial: Bearer [redacted]",
		},
		{
			name:  "no_marker_unchanged",
			input: "dial tcp 10.0.0.1:443: i/o timeout",
			want:  "dial tcp 10.0.0.1:443: i/o timeout",
		},
		{
			name:  "empty_unchanged",
			input: "",
			want:  "",
		},
		{
			// marker 落在串尾、后面没有值：不能 panic、不能挂住。
			name:  "bearer_marker_at_end",
			input: "ctx Bearer ",
			want:  "ctx Bearer [redacted]",
		},
		{
			name:  "authorization_marker_at_end",
			input: "ctx Authorization:",
			want:  "ctx Authorization:[redacted]",
		},
		{
			name:  "access_token_marker_at_end",
			input: "ctx access_token=",
			want:  "ctx access_token=[redacted]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRedactWithDeadline(t, tc.name, func() string {
				return sanitizeDiagnosticError(tc.input)
			})
			if got != tc.want {
				t.Fatalf("sanitizeDiagnosticError(%q) = %q, want %q", tc.input, got, tc.want)
			}
			for _, secret := range tc.mustNotHave {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q 泄漏到输出：%q", secret, got)
				}
			}
		})
	}
}

// 共享 helper 在 marker 密集的长输入上必须终止（游标只前进）。
//
// 硬超时而非依赖 go test -timeout：回归时要看到一条具体失败，而不是整个包挂死。
func TestRedactMarkedSecretsTerminatesOnManyMarkers(t *testing.T) {
	cases := []struct {
		name             string
		input            string
		skipSpaceAfter   bool
		markers          []string
		mustNotHaveCount int
	}{
		{
			name:           "hundred_bearer_tokens_no_space_skip",
			input:          strings.Repeat("Bearer tok ", 100),
			markers:        diagnosticSecretMarkers,
			skipSpaceAfter: false,
		},
		{
			name:           "hundred_header_pairs_with_space_skip",
			input:          strings.Repeat("Authorization: Bearer tok ", 100),
			markers:        routerFrameSecretMarkers,
			skipSpaceAfter: true,
		},
		{
			name:           "empty_marker_is_ignored",
			input:          "Bearer tok Bearer tok",
			markers:        []string{"", "Bearer "},
			skipSpaceAfter: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() {
				done <- redactMarkedSecrets(tc.input, tc.markers, tc.skipSpaceAfter)
			}()
			var got string
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: redactMarkedSecrets 2s 内未返回，疑似死循环", tc.name)
			}
			if strings.Contains(got, "tok") {
				t.Fatalf("secret 未被完全打码：%q", got[:min(len(got), 96)])
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("缺少打码标记：%q", got[:min(len(got), 96)])
			}
		})
	}
}
